// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package catchup

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/network"
)

const (
	// peerRankInitialFirstPriority is the high-priority peers group ( typically, archivers )
	// peerRankInitialFirstPriority는 우선 순위가 높은 피어 그룹입니다(일반적으로 archivers).
	peerRankInitialFirstPriority = 0
	peerRank0LowBlockTime        = 1
	peerRank0HighBlockTime       = 199

	// peerRankInitialSecondPriority is the second priority peers group ( typically, relays )
	// peerRankInitialSecondPriority는 두 번째 우선 순위 피어 그룹입니다(일반적으로 relays).
	peerRankInitialSecondPriority = 200
	peerRank1LowBlockTime         = 201
	peerRank1HighBlockTime        = 399

	peerRankInitialThirdPriority = 400
	peerRank2LowBlockTime        = 401
	peerRank2HighBlockTime       = 599

	peerRankInitialFourthPriority = 600
	peerRank3LowBlockTime         = 601
	peerRank3HighBlockTime        = 799

	// peerRankDownloadFailed is used for responses which could be temporary, such as missing files, or such that we don't have clear resolution
	// peerRankDownloadFailed는 누락된 파일과 같이 일시적일 수 있거나 명확한 해결 방법이 없는 응답에 사용됩니다.
	peerRankDownloadFailed = 900
	// peerRankInvalidDownload is used for responses which are likely to be invalid - whether it's serving the wrong content or attempting to serve malicious content
	// peerRankInvalidDownload는 잘못된 콘텐츠를 제공하거나 악성 콘텐츠를 제공하려는 시도 여부와 상관없이 유효하지 않을 가능성이 있는 응답에 사용됩니다.
	peerRankInvalidDownload = 1000

	// once a block is downloaded, the download duration is clamped into the range of [lowBlockDownloadThreshold..highBlockDownloadThreshold] and then mapped into the a ranking range.
	// 블록이 다운로드되면 다운로드 기간이 [lowBlockDownloadThreshold..highBlockDownloadThreshold] 범위로 고정된 다음 순위 범위에 매핑됩니다.
	lowBlockDownloadThreshold  = 50 * time.Millisecond
	highBlockDownloadThreshold = 8 * time.Second

	// Is the lookback window size of peer usage statistics
	// 피어 사용 통계의 전환 확인 창 크기입니다.
	peerHistoryWindowSize = 100
)

var errPeerSelectorNoPeerPoolsAvailable = errors.New("no peer pools available")

// peerSelectorPeer is a peer wrapper, adding to it the peerClass information
// There can be multiple peer elements with the same address but different peer classes, and it is important to distinguish between them.
// peerSelectorPeer는 피어 래퍼이며 여기에 peerClass 정보를 추가합니다.
// 주소는 같지만 피어 클래스가 다른 피어 요소가 여러 개 있을 수 있으며 이를 구별하는 것이 중요합니다.
type peerSelectorPeer struct {
	network.Peer
	peerClass network.PeerOption
}

// peerClass defines the type of peer we want to have in a particular "class", and define the network.PeerOption that would be used to retrieve that type of peer
// peerClass는 특정 "클래스"에 포함하려는 피어 유형을 정의하고 해당 유형의 피어를 검색하는 데 사용할 network.PeerOption을 정의합니다.
type peerClass struct {
	initialRank int
	peerClass   network.PeerOption
}

// the peersRetriever is a subset of the network.GossipNode used to ensure that we can create an instance of the peerSelector for testing purposes, providing just the above function.
// peersRetriever는 network.GossipNode의 하위 집합입니다. 테스트 목적으로 peerSelector의 인스턴스를 만들 수 있는지 확인하는 데 사용되며 위의 기능만 제공합니다.
type peersRetriever interface {
	// Get a list of Peers we could potentially send a direct message to.
	GetPeers(options ...network.PeerOption) []network.Peer
}

// peerPoolEntry represents a single peer entry in the pool. It contains the underlying network peer as well as the peer class.
// peerPoolEntry는 풀의 단일 피어 항목을 나타냅니다. 여기에는 기본 네트워크 피어와 피어 클래스가 포함됩니다.
type peerPoolEntry struct {
	peer    network.Peer
	class   peerClass
	history *historicStats
}

// peerPool is a single pool of peers that shares the same rank.
// peerPool은 동일한 순위를 공유하는 단일 피어 풀입니다.
type peerPool struct {
	rank  int
	peers []peerPoolEntry
}

// peerSelector is a helper struct used to select the next peer to try and connect to for various catchup purposes.
// Unlike the underlying network GetPeers(), it allows the client to provide feedback regarding the peer's performance, and to have the subsequent query(s) take advantage of that intel.
// peerSelector는 다양한 캐치업 목적으로 연결을 시도하고 연결할 다음 피어를 선택하는 데 사용되는 도우미 구조체입니다.
// 기본 네트워크 GetPeers()와 달리 클라이언트가 피어의 성능에 대한 피드백을 제공하고 후속 쿼리가 해당 정보를 활용할 수 있도록 합니다.
type peerSelector struct {
	mu          deadlock.Mutex
	net         peersRetriever
	peerClasses []peerClass
	pools       []peerPool
	counter     uint64
}

// historicStats stores the past windowSize ranks for the peer passed into RankPeer (i.e. no averaging or penalty).
// The purpose of this structure is to compute the rank based on the performance of the peer in the past, and to be forgiving of occasional performance variations which may not be representative of the peer's overall performance.
// It also stores the penalty history in the form or peer selection gaps, and the count of peerRankDownloadFailed incidents.
// historyStats는 RankPeer에 전달된 피어의 과거 windowSize 순위를 저장합니다(즉, 평균화 또는 패널티 없음).
// 이 구조의 목적은 과거 피어의 성능을 기반으로 순위를 계산하고 피어의 전체 성능을 나타내지 않을 수 있는 간헐적인 성능 변동을 용서하는 것입니다.
// 또한 페널티 기록을 양식 또는 피어 선택 갭에 저장하고 peerRankDownloadFailed 인시던트 수를 저장합니다.
type historicStats struct {
	windowSize       int
	rankSamples      []int
	rankSum          uint64
	requestGaps      []uint64
	gapSum           float64
	counter          uint64
	downloadFailures int
}

func makeHistoricStatus(windowSize int, class peerClass) *historicStats {
	// Initialize the window (rankSamples) with zeros but rankSum with the equivalent sum of class initial rank.
	// This way, every peer will slowly build up its profile.
	// Otherwise, if the best peer gets a bad download the first time, that will determine the rank of the peer.
	// 창(rankSamples)을 0으로 초기화하지만 rankSum은 클래스 초기 순위의 동일한 합으로 초기화합니다.
	// 이런 식으로 모든 피어는 천천히 프로필을 구축합니다.
	// 그렇지 않으면 최고의 피어가 처음으로 잘못된 다운로드를 받으면 피어의 순위가 결정됩니다.
	hs := historicStats{
		windowSize:  windowSize,
		rankSamples: make([]int, windowSize, windowSize),
		requestGaps: make([]uint64, 0, windowSize),
		rankSum:     uint64(class.initialRank) * uint64(windowSize),
		gapSum:      0.0}
	for i := 0; i < windowSize; i++ {
		hs.rankSamples[i] = class.initialRank
	}
	return &hs
}

// computerPenalty is the formula (exponential) used to calculate the penalty from the sum of gaps.
// computerPenalty는 간격의 합에서 패널티를 계산하는 데 사용되는 공식(지수)입니다.
func (hs *historicStats) computerPenalty() float64 {
	return 1 + (math.Exp(hs.gapSum/10.0) / 1000)
}

// updateRequestPenalty is given a counter, which is the most recent counter for ranking a peer.
// It calculates newGap, which is the number of counter ticks since it last was updated
// (i.e. last ranked after being selected).
// The number of gaps stored is bounded by the windowSize.
// Calculates and returns the new penalty.
// updateRequestPenalty에는 피어 순위를 매기기 위한 가장 최근 카운터인 카운터가 제공됩니다.
// 마지막으로 업데이트된 이후의 카운터 틱 수인 newGap을 계산합니다.
// (즉, 선택 후 마지막 순위).
// 저장된 갭의 수는 windowSize에 의해 제한됩니다.
// 새로운 페널티를 계산하고 반환합니다.
func (hs *historicStats) updateRequestPenalty(counter uint64) float64 {
	newGap := counter - hs.counter
	hs.counter = counter

	if len(hs.requestGaps) == hs.windowSize {
		hs.gapSum -= 1.0 / float64(hs.requestGaps[0])
		hs.requestGaps = hs.requestGaps[1:]
	}

	hs.requestGaps = append(hs.requestGaps, newGap)
	hs.gapSum += 1.0 / float64(newGap)

	return hs.computerPenalty()
}

// resetRequestPenalty removes steps least recent gaps and recomputes the new penalty.
// Returns the new rank calculated with the new penalty.
// If steps is 0, it is a full reset i.e. drops all gap values.
// updateRequestPenalty에 대한 자세한 안내는 안내 드립니다.
// 최후의 최후의 최후의 케이스 수인 newGap을 지원합니다.
// (정정후 기사).
// 모든 시간의 제한은 windowSize에 의해 결정됩니다.
// 새로운 페이퍼를 반환합니다.
func (hs *historicStats) resetRequestPenalty(steps int, initialRank int, class peerClass) (rank int) {
	if len(hs.requestGaps) == 0 {
		return initialRank
	}
	// resetRequestPenalty cannot move the peer to a better class if the peer was moved to a lower class (e.g. failed downloads or invalid downloads)
	// resetRequestPenalty는 피어가 더 낮은 클래스로 이동된 경우 피어를 더 나은 클래스로 이동할 수 없습니다(예: 다운로드 실패 또는 유효하지 않은 다운로드).
	if upperBound(class) < initialRank {
		return initialRank
	}
	// if setps is 0, it is a full reset
	// setps가 0이면 전체 재설정입니다.
	if steps == 0 {
		hs.requestGaps = make([]uint64, 0, hs.windowSize)
		hs.gapSum = 0.0
		return int(float64(hs.rankSum) / float64(len(hs.rankSamples)))
	}

	if steps > len(hs.requestGaps) {
		steps = len(hs.requestGaps)
	}
	for s := 0; s < steps; s++ {
		hs.gapSum -= 1.0 / float64(hs.requestGaps[s])
	}
	hs.requestGaps = hs.requestGaps[steps:]
	return boundRankByClass(int(hs.computerPenalty()*(float64(hs.rankSum)/float64(len(hs.rankSamples)))), class)
}

// push pushes a new rank to the historicStats, and returns the new rank based on the average of ranks in the windowSize window and the penlaty.
// push는 새로운 순위를 historyStats에 푸시하고 windowSize 창과 penlaty의 평균 순위를 기반으로 새 순위를 반환합니다.
func (hs *historicStats) push(value int, counter uint64, class peerClass) (averagedRank int) {

	// This is the lowest ranking class, and is not subject to giving another chance.
	// Do not modify this value with historical data.
	// 최하위 클래스로, 재차 기회가 주어지지 않습니다.
	// 과거 데이터로 이 값을 수정하지 마십시오.
	if value == peerRankInvalidDownload {
		return value
	}

	// This is a moving window. Remore the least recent value once the window is full
	// 움직이는 창입니다. 창이 가득 차면 가장 최근의 값을 삭제하십시오.
	if len(hs.rankSamples) == hs.windowSize {
		hs.rankSum -= uint64(hs.rankSamples[0])
		hs.rankSamples = hs.rankSamples[1:]
	}

	initialRank := value

	// Download may fail for various reasons. Give it additional tries and see if it recovers/improves.
	// 다양한 이유로 다운로드가 실패할 수 있습니다. 추가 시도를 하고 회복/개선되는지 확인하십시오.
	if value == peerRankDownloadFailed {
		hs.downloadFailures++
		// - Set the rank to the class upper bound multiplied
		//   by the number of downloadFailures.
		// - Each downloadFailure increments the counter, and
		//   each non-failure decrements it, until it gets to 0.
		// - When the peer is consistently failing to
		//   download, the value added to rankSum will
		//   increase at an increasing rate to evict the peer
		//   from the class sooner.
		value = upperBound(class) * int(math.Exp2(float64(hs.downloadFailures)))
	} else {
		if hs.downloadFailures > 0 {
			hs.downloadFailures--
		}
	}

	hs.rankSamples = append(hs.rankSamples, value)
	hs.rankSum += uint64(value)

	// The average performance of the peer
	// 피어의 평균 성능
	average := float64(hs.rankSum) / float64(len(hs.rankSamples))

	if int(average) > upperBound(class) && initialRank == peerRankDownloadFailed {
		// peerRankDownloadFailed will be delayed, to give the peer
		// additional time to improve. If does not improve over time,
		// the average will exceed the class limit. At this point,
		// it will be pushed down to download failed class.
		return peerRankDownloadFailed
	}

	// A penalty is added relative to how freequently the peer is used
	// 피어가 얼마나 자주 사용되는지에 따라 패널티가 추가됩니다.
	penalty := hs.updateRequestPenalty(counter)

	// The rank based on the performance and the freequency
	// 성능과 빈도에 따른 순위
	avgWithPenalty := int(penalty * average)

	// Keep the peer in the same class. The value passed will be within bounds
	// (unless it is downloadFailed or invalidDownload),
	// but the penalty may push it over.
	// Prevent the penalty pushing it off the class bounds.
	// 피어를 같은 클래스에 유지합니다. 전달된 값은 범위 내에 있습니다.
	// (downloadFailed 또는 invalidDownload가 아닌 경우),
	// 하지만 패널티로 인해 넘어갈 수 있습니다.
	// 페널티가 클래스 경계를 벗어나는 것을 방지합니다.
	bounded := boundRankByClass(avgWithPenalty, class)
	return bounded
}

// makePeerSelector creates a peerSelector, given a peersRetriever and peerClass array.
// makePeerSelector는 peersRetriever 및 peerClass 배열이 주어지면 peerSelector를 생성합니다.
func makePeerSelector(net peersRetriever, initialPeersClasses []peerClass) *peerSelector {
	selector := &peerSelector{
		net:         net,
		peerClasses: initialPeersClasses,
	}
	return selector
}

// getNextPeer returns the next peer.
// It randomally selects a peer from a pool that has the lowest rank value.
// Given that the peers are grouped by their ranks, allow us to prioritize peers based on their class and/or performance.
// getNextPeer는 다음 피어를 반환합니다.
// 가장 낮은 순위 값을 가진 풀에서 무작위로 피어를 선택합니다.
// 피어가 순위별로 그룹화되어 있으므로 클래스 및/또는 성능에 따라 피어의 우선 순위를 지정할 수 있습니다.
func (ps *peerSelector) getNextPeer() (psp *peerSelectorPeer, err error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.refreshAvailablePeers()
	for _, pool := range ps.pools {
		if len(pool.peers) > 0 {
			// the previous call to refreshAvailablePeers ensure that this would always be the case;
			// however, if we do have a zero length pool, we don't want to divide by zero, so this would
			// provide the needed test.
			// pick one of the peers from this pool at random
			peerIdx := crypto.RandUint64() % uint64(len(pool.peers))
			psp = &peerSelectorPeer{pool.peers[peerIdx].peer, pool.peers[peerIdx].class.peerClass}
			return
		}
	}
	return nil, errPeerSelectorNoPeerPoolsAvailable
}

// rankPeer ranks a given peer.
// return the old value and the new updated value.
// updated value could be different from the input rank.
// rankPeer는 주어진 피어의 순위를 매깁니다.
// 이전 값과 새로 업데이트된 값을 반환합니다.
// 업데이트된 값은 입력 순위와 다를 수 있습니다.
func (ps *peerSelector) rankPeer(psp *peerSelectorPeer, rank int) (int, int) {
	if psp == nil {
		return -1, -1
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	poolIdx, peerIdx := ps.findPeer(psp)
	if poolIdx < 0 || peerIdx < 0 {
		return -1, -1
	}
	sortNeeded := false

	// we need to remove the peer from the pool so we can place it in a different location.
	// 다른 위치에 놓을 수 있도록 풀에서 피어를 제거해야 합니다.
	pool := ps.pools[poolIdx]
	ps.counter++
	initialRank := pool.rank
	rank = pool.peers[peerIdx].history.push(rank, ps.counter, pool.peers[peerIdx].class)
	if pool.rank != rank {
		class := pool.peers[peerIdx].class
		peerHistory := pool.peers[peerIdx].history
		if len(pool.peers) > 1 {
			pool.peers = append(pool.peers[:peerIdx], pool.peers[peerIdx+1:]...)
			ps.pools[poolIdx] = pool
		} else {
			// the last peer was removed from the pool; delete this pool.
			// 마지막 피어가 풀에서 제거되었습니다. 이 풀을 삭제하십시오.
			ps.pools = append(ps.pools[:poolIdx], ps.pools[poolIdx+1:]...)
		}

		sortNeeded = ps.addToPool(psp.Peer, rank, class, peerHistory)
	}

	// Update the ranks of the peers by reducing the penalty for not being selected
	// 선택되지 않은 것에 대한 페널티를 줄임으로써 피어의 순위를 업데이트합니다.
	for pl := len(ps.pools) - 1; pl >= 0; pl-- {
		pool := ps.pools[pl]
		for pr := len(pool.peers) - 1; pr >= 0; pr-- {
			localPeer := pool.peers[pr]
			if pool.peers[pr].peer == psp.Peer {
				continue
			}
			// make the removal of penalty at a faster rate than adding it, so that the performance of the peer dominates in the evaluation over the freequency.
			// Otherwise, the peer selection will oscillate between the good performing and a bad performing peers when sufficient penalty is accumulated to the good peer.
			// 페널티를 추가하는 것보다 더 빠른 속도로 페널티를 제거하여 프리퀀시보다 평가에서 피어의 성능이 지배하도록 합니다.
			// 그렇지 않으면, 좋은 피어에 충분한 페널티가 누적될 때 피어 선택은 좋은 성능과 나쁜 성능 피어 사이에서 진동합니다.
			newRank := localPeer.history.resetRequestPenalty(5, pool.rank, pool.peers[pr].class)
			if newRank != pool.rank {
				upeer := pool.peers[pr].peer
				class := pool.peers[pr].class
				peerHistory := pool.peers[pr].history
				if len(pool.peers) > 1 {
					pool.peers = append(pool.peers[:pr], pool.peers[pr+1:]...)
					ps.pools[pl] = pool
				} else {
					// the last peer was removed from the pool; delete this pool.
					// 마지막 피어가 풀에서 제거되었습니다. 이 풀을 삭제하십시오.
					ps.pools = append(ps.pools[:pl], ps.pools[pl+1:]...)
				}
				sortNeeded = ps.addToPool(upeer, newRank, class, peerHistory) || sortNeeded
			}
		}
	}
	if sortNeeded {
		ps.sort()
	}
	return initialRank, rank
}

// peerDownloadDurationToRank calculates the rank for a peer given a peer and the block download time.
// peerDownloadDurationToRank는 피어와 블록 다운로드 시간이 주어진 피어의 순위를 계산합니다.
func (ps *peerSelector) peerDownloadDurationToRank(psp *peerSelectorPeer, blockDownloadDuration time.Duration) (rank int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	poolIdx, peerIdx := ps.findPeer(psp)
	if poolIdx < 0 || peerIdx < 0 {
		return peerRankInvalidDownload
	}

	switch ps.pools[poolIdx].peers[peerIdx].class.initialRank {
	case peerRankInitialFirstPriority:
		return downloadDurationToRank(blockDownloadDuration, lowBlockDownloadThreshold, highBlockDownloadThreshold, peerRank0LowBlockTime, peerRank0HighBlockTime)
	case peerRankInitialSecondPriority:
		return downloadDurationToRank(blockDownloadDuration, lowBlockDownloadThreshold, highBlockDownloadThreshold, peerRank1LowBlockTime, peerRank1HighBlockTime)
	case peerRankInitialThirdPriority:
		return downloadDurationToRank(blockDownloadDuration, lowBlockDownloadThreshold, highBlockDownloadThreshold, peerRank2LowBlockTime, peerRank2HighBlockTime)
	default: // i.e. peerRankInitialFourthPriority
		return downloadDurationToRank(blockDownloadDuration, lowBlockDownloadThreshold, highBlockDownloadThreshold, peerRank3LowBlockTime, peerRank3HighBlockTime)
	}
}

// addToPool adds a given peer to the correct group. If no group exists for that peer's rank, a new group is created.
// The method return true if a new group was created ( suggesting that the pools list would need to be re-ordered ), or false otherwise.
// addToPool은 주어진 피어를 올바른 그룹에 추가합니다. 해당 피어의 순위에 대한 그룹이 없으면 새 그룹이 생성됩니다.
// 메서드는 새 그룹이 생성되면 true를 반환하고(풀 목록을 재정렬해야 함을 나타냄) 그렇지 않으면 false를 반환합니다.
func (ps *peerSelector) addToPool(peer network.Peer, rank int, class peerClass, peerHistory *historicStats) bool {
	// see if we already have a list with that rank:
	// 해당 순위의 목록이 이미 있는지 확인합니다.
	for i, pool := range ps.pools {
		if pool.rank == rank {
			// we found an existing group, add this peer to the list.
			// 기존 그룹을 찾았으면 이 피어를 목록에 추가합니다.
			ps.pools[i].peers = append(pool.peers, peerPoolEntry{peer: peer, class: class, history: peerHistory})
			return false
		}
	}
	ps.pools = append(ps.pools, peerPool{rank: rank, peers: []peerPoolEntry{{peer: peer, class: class, history: peerHistory}}})
	return true
}

// sort the pools array in an ascending order according to the rank of each pool.
// 각 풀의 순위에 따라 오름차순으로 pools 배열을 정렬합니다.
func (ps *peerSelector) sort() {
	sort.SliceStable(ps.pools, func(i, j int) bool {
		return ps.pools[i].rank < ps.pools[j].rank
	})
}

// peerAddress returns the peer's underlying address.
// The network.Peer object cannot be compared to itself, since the network package dynamically creating a new instance on every network.GetPeers() call.
// The method retrun the peer address or an empty string if the peer is not one of HTTPPeer/UnicastPeer
// peerAddress는 피어의 기본 주소를 반환합니다.
// network.Peer 개체는 네트워크 패키지가 모든 network.GetPeers() 호출에서 새 인스턴스를 동적으로 생성하기 때문에 자신과 비교할 수 없습니다.
// 피어가 HTTPPeer/UnicastPeer 중 하나가 아닌 경우 메서드는 피어 주소 또는 빈 문자열을 다시 실행합니다.
func peerAddress(peer network.Peer) string {
	if httpPeer, ok := peer.(network.HTTPPeer); ok {
		return httpPeer.GetAddress()
	} else if unicastPeer, ok := peer.(network.UnicastPeer); ok {
		return unicastPeer.GetAddress()
	}
	return ""
}

// refreshAvailablePeers reload the available peers from the network package, add new peers along with their corresponding initial rank, and deletes peers that have been dropped by the network package.
// refreshAvailablePeers는 네트워크 패키지에서 사용 가능한 피어를 다시 로드하고 해당 초기 순위와 함께 새 피어를 추가하고 네트워크 패키지에 의해 삭제된 피어를 삭제합니다.
func (ps *peerSelector) refreshAvailablePeers() {
	existingPeers := make(map[network.PeerOption]map[string]bool)
	for _, pool := range ps.pools {
		for _, localPeer := range pool.peers {
			if peerAddress := peerAddress(localPeer.peer); peerAddress != "" {
				// Init the existingPeers map
				if _, has := existingPeers[localPeer.class.peerClass]; !has {
					existingPeers[localPeer.class.peerClass] = make(map[string]bool)
				}
				existingPeers[localPeer.class.peerClass][peerAddress] = true
			}
		}
	}
	sortNeeded := false
	for _, initClass := range ps.peerClasses {
		peers := ps.net.GetPeers(initClass.peerClass)
		for _, peer := range peers {
			peerAddress := peerAddress(peer)
			if peerAddress == "" {
				continue
			}
			if _, has := existingPeers[initClass.peerClass][peerAddress]; has {
				// Setting to false instead of deleting the element to be safe against duplicate peer addresses.
				// 중복 피어 주소에 대해 안전하도록 요소를 삭제하는 대신 false로 설정합니다.
				existingPeers[initClass.peerClass][peerAddress] = false
				continue
			}
			// it's an entry which we did not have before.
			// 이전에는 없었던 항목입니다.
			sortNeeded = ps.addToPool(peer, initClass.initialRank, initClass, makeHistoricStatus(peerHistoryWindowSize, initClass)) || sortNeeded
		}
	}

	// delete from the pools array the peers that do not exist on the network anymore.
	// 네트워크에 더 이상 존재하지 않는 피어를 풀 배열에서 삭제합니다.
	for poolIdx := len(ps.pools) - 1; poolIdx >= 0; poolIdx-- {
		pool := ps.pools[poolIdx]
		for peerIdx := len(pool.peers) - 1; peerIdx >= 0; peerIdx-- {
			peer := pool.peers[peerIdx].peer
			if peerAddress := peerAddress(peer); peerAddress != "" {
				if toRemove, _ := existingPeers[pool.peers[peerIdx].class.peerClass][peerAddress]; toRemove {
					// need to be removed.
					pool.peers = append(pool.peers[:peerIdx], pool.peers[peerIdx+1:]...)
				}
			}
		}
		if len(pool.peers) == 0 {
			ps.pools = append(ps.pools[:poolIdx], ps.pools[poolIdx+1:]...)
			sortNeeded = true
		} else {
			ps.pools[poolIdx] = pool
		}
	}

	if sortNeeded {
		ps.sort()
	}
}

// findPeer look into the peer pool and find the given peer.
// The method returns the pool and peer indices if a peer was found, or (-1, -1) otherwise.
// findPeer는 피어 풀을 살펴보고 주어진 피어를 찾습니다.
// 이 메서드는 피어가 있으면 풀과 피어 인덱스를 반환하고 그렇지 않으면 (-1, -1)을 반환합니다.
func (ps *peerSelector) findPeer(psp *peerSelectorPeer) (poolIdx, peerIdx int) {
	peerAddr := peerAddress(psp.Peer)
	if peerAddr == "" {
		return -1, -1
	}
	for i, pool := range ps.pools {
		for j, localPeerEntry := range pool.peers {
			if peerAddress(localPeerEntry.peer) == peerAddr &&
				localPeerEntry.class.peerClass == psp.peerClass {
				return i, j
			}
		}
	}
	return -1, -1
}

// calculate the duration rank by mapping the range of [minDownloadDuration..maxDownloadDuration] into the rank range of [minRank..maxRank]
// [minDownloadDuration..maxDownloadDuration]의 범위를 [minRank..maxRank]의 순위 범위에 매핑하여 지속 시간 순위를 계산합니다.
func downloadDurationToRank(downloadDuration, minDownloadDuration, maxDownloadDuration time.Duration, minRank, maxRank int) (rank int) {
	// clamp the downloadDuration into the range of [minDownloadDuration .. maxDownloadDuration]
	// downloadDuration을 [minDownloadDuration .. maxDownloadDuration] 범위로 고정합니다.
	if downloadDuration < minDownloadDuration {
		downloadDuration = minDownloadDuration
	} else if downloadDuration > maxDownloadDuration {
		downloadDuration = maxDownloadDuration
	}
	// the formula below maps an element in the range of [minDownloadDuration .. maxDownloadDuration] onto the range of [minRank .. maxRank]
	// 아래 공식은 [minDownloadDuration .. maxDownloadDuration] 범위의 요소를 [minRank .. maxRank] 범위에 매핑합니다.
	rank = minRank + int((downloadDuration-minDownloadDuration).Nanoseconds()*int64(maxRank-minRank)/(maxDownloadDuration-minDownloadDuration).Nanoseconds())
	return
}

func lowerBound(class peerClass) int {
	switch class.initialRank {
	case peerRankInitialFirstPriority:
		return peerRank0LowBlockTime
	case peerRankInitialSecondPriority:
		return peerRank1LowBlockTime
	case peerRankInitialThirdPriority:
		return peerRank2LowBlockTime
	default: // i.e. peerRankInitialFourthPriority
		return peerRank3LowBlockTime
	}
}

func upperBound(class peerClass) int {
	switch class.initialRank {
	case peerRankInitialFirstPriority:
		return peerRank0HighBlockTime
	case peerRankInitialSecondPriority:
		return peerRank1HighBlockTime
	case peerRankInitialThirdPriority:
		return peerRank2HighBlockTime
	default: // i.e. peerRankInitialFourthPriority
		return peerRank3HighBlockTime
	}
}

func boundRankByClass(rank int, class peerClass) int {
	if rank < lowerBound(class) {
		return lowerBound(class)
	}
	if rank > upperBound(class) {
		return upperBound(class)
	}
	return rank
}
