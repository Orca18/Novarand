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
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Orca18/novarand/agreement"
	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
	"github.com/Orca18/novarand/network"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/execpool"
)

const catchupPeersForSync = 10
const blockQueryPeerLimit = 10

// this should be at least the number of relays
// 이것은 최소한 릴레이 수여야 합니다??
const catchupRetryLimit = 500

// PendingUnmatchedCertificate is a single certificate that is being waited upon to have its corresponding block fetched.
// PendingUnmatchedCertificate는 해당 블록을 가져오기 위해 대기 중인 단일 인증서입니다.
type PendingUnmatchedCertificate struct {
	Cert         agreement.Certificate
	VoteVerifier *agreement.AsyncVoteVerifier
}

// Ledger represents the interface of a block database which the catchup server should interact with.
// Ledger는 캐치업 서버가 상호 작용해야 하는 블록 데이터베이스의 인터페이스를 나타냅니다.
type Ledger interface {
	agreement.LedgerReader
	AddBlock(bookkeeping.Block, agreement.Certificate) error
	EnsureBlock(block *bookkeeping.Block, c agreement.Certificate)
	LastRound() basics.Round
	Block(basics.Round) (bookkeeping.Block, error)
	IsWritingCatchpointFile() bool
	Validate(ctx context.Context, blk bookkeeping.Block, executionPool execpool.BacklogPool) (*ledgercore.ValidatedBlock, error)
	AddValidatedBlock(vb ledgercore.ValidatedBlock, cert agreement.Certificate) error
}

// Service represents the catchup service. Once started and until it is stopped, it ensures that the ledger is up to date with network.
// 서비스는 캐치업 서비스를 나타냅니다. 일단 시작되고 중지될 때까지 원장이 네트워크에서 최신 상태인지 확인합니다.
type Service struct {
	syncStartNS         int64 // at top of struct to keep 64 bit aligned for atomic.* ops
	cfg                 config.Local
	ledger              Ledger
	ctx                 context.Context
	cancel              func()
	done                chan struct{}
	log                 logging.Logger
	net                 network.GossipNode
	auth                BlockAuthenticator
	parallelBlocks      uint64
	deadlineTimeout     time.Duration
	blockValidationPool execpool.BacklogPool

	// suspendForCatchpointWriting defines whether we've ran into a state where the ledger is currently busy writing the catchpoint file.
	// If so, we want to suspend the catchup process until the catchpoint file writing is complete, and resume from there without stopping the catchup timer.
	// suspendForCatchpointWriting은 원장이 현재 캐치포인트 파일을 작성하는 데 바쁜 상태에 도달했는지 여부를 정의합니다.
	// 그렇다면 캐치포인트 파일 쓰기가 완료될 때까지 캐치업 프로세스를 일시 중단하고 캐치업 타이머를 중지하지 않고 다시 시작합니다.
	suspendForCatchpointWriting bool

	// The channel gets closed when the initial sync is complete.
	// This allows for other services to avoid the overhead of starting prematurely
	// (before this node is caught-up and can validate messages for example).
	// 초기 동기화가 완료되면 채널이 닫힙니다.
	// 이렇게 하면 다른 서비스가 조기에 시작하는 오버헤드를 피할 수 있습니다.
	// (예를 들어 이 노드가 따라잡혀 메시지를 확인할 수 있기 전에).
	InitialSyncDone              chan struct{}
	initialSyncNotified          uint32
	protocolErrorLogged          bool
	lastSupportedRound           basics.Round
	unmatchedPendingCertificates <-chan PendingUnmatchedCertificate
}

// A BlockAuthenticator authenticates blocks given a certificate.
// 블록 인증자는 인증서에 지정된 블록을 인증합니다.
//
// Note that Authenticate does not check if the block contents match their header as it only checks the block header.
// If the contents have not been checked yet, callers should also call block.
// ContentsMatchHeader and reject blocks that do not pass this check.
// Authenticate는 블록 헤더만 확인하므로 블록 내용이 헤더와 일치하는지 확인하지 않습니다.
// 내용이 아직 확인되지 않았다면 호출자도 블록을 호출해야 합니다.
// ContentsMatchHeader 및 이 검사를 통과하지 못한 블록을 거부합니다.
type BlockAuthenticator interface {
	Authenticate(*bookkeeping.Block, *agreement.Certificate) error
	Quit()
}

// MakeService creates a catchup service instance from its constituent components
// MakeService는 구성 요소에서 캐치업 서비스 인스턴스를 생성합니다.
func MakeService(log logging.Logger, config config.Local, net network.GossipNode, ledger Ledger, auth BlockAuthenticator, unmatchedPendingCertificates <-chan PendingUnmatchedCertificate, blockValidationPool execpool.BacklogPool) (s *Service) {
	s = &Service{}

	s.cfg = config
	s.ledger = ledger
	s.net = net
	s.auth = auth
	s.unmatchedPendingCertificates = unmatchedPendingCertificates
	s.log = log.With("Context", "sync")
	s.parallelBlocks = config.CatchupParallelBlocks
	s.deadlineTimeout = agreement.DeadlineTimeout()
	s.blockValidationPool = blockValidationPool

	return s
}

// Start the catchup service
func (s *Service) Start() {
	s.done = make(chan struct{})
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.InitialSyncDone = make(chan struct{})
	go s.periodicSync()
}

// Stop informs the catchup service that it should stop, and waits for it to stop (when periodicSync() exits)
// Stop은 캐치업 서비스에 중지해야 함을 알리고 중지될 때까지 기다립니다(주기적인 Sync()가 종료될 때).
func (s *Service) Stop() {
	s.cancel()
	<-s.done
	if atomic.CompareAndSwapUint32(&s.initialSyncNotified, 0, 1) {
		close(s.InitialSyncDone)
	}
}

// IsSynchronizing returns true if we're currently executing a sync() call
// - either initial catchup or attempting to catchup after too-long waiting for next block.
// Also returns a 2nd bool indicating if this is our initial sync
// 현재 sync() 호출을 실행 중인 경우 IsSynchronizing이 true를 반환합니다.
// - 초기 캐치업 또는 다음 블록을 너무 오래 기다린 후 캐치업을 시도합니다.
// 이것이 초기 동기화인지 여부를 나타내는 두 번째 bool도 반환합니다.
func (s *Service) IsSynchronizing() (synchronizing bool, initialSync bool) {
	synchronizing = atomic.LoadInt64(&s.syncStartNS) != 0
	initialSync = atomic.LoadUint32(&s.initialSyncNotified) == 0
	return
}

// SynchronizingTime returns the time we've been performing a catchup operation (0 if not currently catching up)
// 동기화 시간은 따라잡기 작업을 수행한 시간을 반환합니다(현재 따라잡지 않은 경우 0).
func (s *Service) SynchronizingTime() time.Duration {
	startNS := atomic.LoadInt64(&s.syncStartNS)
	if startNS == 0 {
		return time.Duration(0)
	}
	timeInNS := time.Now().UnixNano()
	return time.Duration(timeInNS - startNS)
}

// errLedgerAlreadyHasBlock is returned by innerFetch in case the local ledger already has the requested block.
// errLedgerAlreadyHasBlock은 로컬 원장에 이미 요청된 블록이 있는 경우 innerFetch에 의해 반환됩니다.
var errLedgerAlreadyHasBlock = errors.New("ledger already has block")

// function scope to make a bunch of defer statements better
// 여러 defer 문을 더 잘 만들기 위한 함수 범위
func (s *Service) innerFetch(r basics.Round, peer network.Peer) (blk *bookkeeping.Block, cert *agreement.Certificate, ddur time.Duration, err error) {
	ledgerWaitCh := s.ledger.Wait(r)
	select {
	case <-ledgerWaitCh:
		// if our ledger already have this block, no need to attempt to fetch it.
		// 원장에 이미 이 블록이 있으면 가져오려고 시도할 필요가 없습니다.
		return nil, nil, time.Duration(0), errLedgerAlreadyHasBlock
	default:
	}

	ctx, cf := context.WithCancel(s.ctx)
	fetcher := makeUniversalBlockFetcher(s.log, s.net, s.cfg)
	defer cf()
	stopWaitingForLedgerRound := make(chan struct{})
	defer close(stopWaitingForLedgerRound)
	go func() {
		select {
		case <-stopWaitingForLedgerRound:
		case <-ledgerWaitCh:
			cf()
		}
	}()
	blk, cert, ddur, err = fetcher.fetchBlock(ctx, r, peer)
	// check to see if we aborted due to ledger.
	// 원장으로 인해 중단되었는지 확인합니다.
	if err != nil {
		select {
		case <-ledgerWaitCh:
			// yes, we aborted since the ledger received this round.
			// 예, 원장이 이 라운드를 수신한 이후로 중단했습니다.
			err = errLedgerAlreadyHasBlock
		default:
		}
	}
	return
}

// fetchAndWrite fetches a block, checks the cert, and writes it to the ledger. Cert checking and ledger writing both wait for the ledger to advance if necessary.
// Returns false if we should stop trying to catch up.  This may occur for several reasons:
//  - If the context is canceled (e.g. if the node is shutting down)
//  - If we couldn't fetch the block (e.g. if there are no peers available or we've reached the catchupRetryLimit)
//  - If the block is already in the ledger (e.g. if agreement service has already written it)
//  - If the retrieval of the previous block was unsuccessful
// fetchAndWrite는 블록을 가져와서 인증서를 확인하고 원장에 씁니다. 인증서 확인 및 원장 작성은 모두 필요한 경우 원장이 진행되기를 기다립니다.
// 따라잡으려는 시도를 중단해야 하는 경우 false를 반환합니다. 다음과 같은 몇 가지 이유로 발생할 수 있습니다.
// - 컨텍스트가 취소된 경우(예: 노드가 종료되는 경우)
// - 블록을 가져올 수 없는 경우(예: 사용 가능한 피어가 없거나 catchupRetryLimit에 도달한 경우)
// - 블록이 이미 원장에 있는 경우(예: 계약 서비스에서 이미 작성한 경우)
// - 이전 블록의 검색이 실패한 경우
func (s *Service) fetchAndWrite(r basics.Round, prevFetchCompleteChan chan bool, lookbackComplete chan bool, peerSelector *peerSelector) bool {
	i := 0
	hasLookback := false
	for true {
		i++
		select {
		case <-s.ctx.Done():
			s.log.Debugf("fetchAndWrite(%v): Aborted", r)
			return false
		default:
		}

		// Stop retrying after a while.
		if i > catchupRetryLimit {
			loggedMessage := fmt.Sprintf("fetchAndWrite(%d): block retrieval exceeded retry limit", r)
			if _, initialSync := s.IsSynchronizing(); initialSync {
				// on the initial sync, it's completly expected that we won't be able to get all the "next" blocks.
				// Therefore info should suffice.
				// 초기 동기화에서 모든 "다음" 블록을 가져올 수 없을 것으로 완전히 예상됩니다.
				// 따라서 정보로 충분해야 합니다.
				s.log.Info(loggedMessage)
			} else {
				// On any subsequent sync, we migth be looking for multiple rounds into the future, so it's completly reasonable that we would fail retrieving the future block.
				// Generate a warning here only if we're failing to retrieve X+1 or below.
				// All other block retrievals should not generate a warning.
				// 후속 동기화에서 미래에 대한 여러 라운드를 찾을 수 있으므로 미래 블록 검색에 실패하는 것이 완전히 합리적입니다.
				// X+1 이하를 검색하는 데 실패한 경우에만 여기에 경고를 생성합니다.
				// 다른 모든 블록 검색은 경고를 생성하지 않아야 합니다.
				if r > s.ledger.NextRound() {
					s.log.Info(loggedMessage)
				} else {
					s.log.Warn(loggedMessage)
				}
			}
			return false
		}

		psp, getPeerErr := peerSelector.getNextPeer()
		if getPeerErr != nil {
			s.log.Debugf("fetchAndWrite: was unable to obtain a peer to retrieve the block from")
			break
		}
		peer := psp.Peer

		// Try to fetch, timing out after retryInterval
		// 페치 시도, retryInterval 이후 시간 초과
		block, cert, blockDownloadDuration, err := s.innerFetch(r, peer)

		if err != nil {
			if err == errLedgerAlreadyHasBlock {
				// ledger already has the block, no need to request this block.
				// only the agreement could have added this block into the ledger, catchup is complete
				// 원장에 이미 블록이 있으므로 이 블록을 요청할 필요가 없습니다.
				// 합의만이 이 블록을 원장에 추가할 수 있었고, 캐치업이 완료되었습니다.
				s.log.Infof("fetchAndWrite(%d): the block is already in the ledger. The catchup is complete", r)
				return false
			}
			s.log.Debugf("fetchAndWrite(%v): Could not fetch: %v (attempt %d)", r, err, i)
			peerSelector.rankPeer(psp, peerRankDownloadFailed)
			// we've just failed to retrieve a block;
			// wait until the previous block is fetched before trying again to avoid the usecase where the first block doesn't exists and we're making many requests down the chain for no reason.
			// 방금 블록 검색에 실패했습니다.
			// 첫 번째 블록이 존재하지 않고 아무 이유 없이 체인을 따라 많은 요청을 하는 사용 사례를 피하기 위해 다시 시도하기 전에 이전 블록을 가져올 때까지 기다립니다.
			if !hasLookback {
				select {
				case <-s.ctx.Done():
					s.log.Infof("fetchAndWrite(%d): Aborted while waiting for lookback block to ledger after failing once : %v", r, err)
					return false
				case hasLookback = <-lookbackComplete:
					if !hasLookback {
						s.log.Infof("fetchAndWrite(%d): lookback block doesn't exist, won't try to retrieve block again : %v", r, err)
						return false
					}
				}
			}
			continue // retry the fetch
		} else if block == nil || cert == nil {
			// someone already wrote the block to the ledger, we should stop syncing
			return false
		}
		s.log.Debugf("fetchAndWrite(%v): Got block and cert contents: %v %v", r, block, cert)

		// Check that the block's contents match the block header (necessary with an untrusted block because b.Hash() only hashes the header)
		// 블록의 내용이 블록 헤더와 일치하는지 확인합니다(b.Hash()는 헤더만 해시하므로 신뢰할 수 없는 블록에 필요)
		if s.cfg.CatchupVerifyPaysetHash() {
			if !block.ContentsMatchHeader() {
				peerSelector.rankPeer(psp, peerRankInvalidDownload)
				// Check if this mismatch is due to an unsupported protocol version
				// 이 불일치가 지원되지 않는 프로토콜 버전으로 인한 것인지 확인
				if _, ok := config.Consensus[block.BlockHeader.CurrentProtocol]; !ok {
					s.log.Errorf("fetchAndWrite(%v): unsupported protocol version detected: '%v'", r, block.BlockHeader.CurrentProtocol)
					return false
				}

				s.log.Warnf("fetchAndWrite(%v): block contents do not match header (attempt %d)", r, i)
				continue // retry the fetch
			}
		}

		// make sure that we have the lookBack block that's required for authenticating this block
		// 이 블록을 인증하는 데 필요한 lookBack 블록이 있는지 확인합니다.
		if !hasLookback {
			select {
			case <-s.ctx.Done():
				s.log.Debugf("fetchAndWrite(%v): Aborted while waiting for lookback block to ledger", r)
				return false
			case hasLookback = <-lookbackComplete:
				if !hasLookback {
					s.log.Warnf("fetchAndWrite(%v): lookback block doesn't exist, cannot authenticate new block", r)
					return false
				}
			}
		}
		if s.cfg.CatchupVerifyCertificate() {
			err = s.auth.Authenticate(block, cert)
			if err != nil {
				s.log.Warnf("fetchAndWrite(%v): cert did not authenticate block (attempt %d): %v", r, i, err)
				peerSelector.rankPeer(psp, peerRankInvalidDownload)
				continue // retry the fetch
			}
		}

		peerRank := peerSelector.peerDownloadDurationToRank(psp, blockDownloadDuration)
		r1, r2 := peerSelector.rankPeer(psp, peerRank)
		s.log.Debugf("fetchAndWrite(%d): ranked peer with %d from %d to %d", r, peerRank, r1, r2)

		// Write to ledger, noting that ledger writes must be in order
		// 원장에 쓰기, 원장은 순서대로 작성해야 합니다.
		select {
		case <-s.ctx.Done():
			s.log.Debugf("fetchAndWrite(%v): Aborted while waiting to write to ledger", r)
			return false
		case prevFetchSuccess := <-prevFetchCompleteChan:
			if prevFetchSuccess {
				// make sure the ledger wrote enough of the account data to disk, since we don't want the ledger to hold a large amount of data in memory.
				// 원장이 메모리에 많은 양의 데이터를 보유하는 것을 원하지 않기 때문에 원장이 디스크에 계정 데이터를 충분히 썼는지 확인합니다.
				proto, err := s.ledger.ConsensusParams(r.SubSaturate(1))
				if err != nil {
					s.log.Errorf("fetchAndWrite(%d): Unable to determine consensus params for round %d: %v", r, r-1, err)
					return false
				}
				ledgerBacklogRound := r.SubSaturate(basics.Round(proto.MaxBalLookback))
				select {
				case <-s.ledger.Wait(ledgerBacklogRound):
					// i.e. round r-320 is no longer in the blockqueue, so it's account data is either being currently written, or it was already written.
					// 즉, 라운드 r-320은 더 이상 블록 대기열에 없으므로 계정 데이터가 현재 작성 중이거나 이미 작성되었습니다.
				case <-s.ctx.Done():
					s.log.Debugf("fetchAndWrite(%d): Aborted while waiting for ledger to complete writing up to round %d", r, ledgerBacklogRound)
					return false
				}

				if s.cfg.CatchupVerifyTransactionSignatures() || s.cfg.CatchupVerifyApplyData() {
					var vb *ledgercore.ValidatedBlock
					vb, err = s.ledger.Validate(s.ctx, *block, s.blockValidationPool)
					if err != nil {
						if s.ctx.Err() != nil {
							// if the context expired, just exit.
							return false
						}
						if errNSBE, ok := err.(ledgercore.ErrNonSequentialBlockEval); ok && errNSBE.EvaluatorRound <= errNSBE.LatestRound {
							// the block was added to the ledger from elsewhere after fetching it here only the agreement could have added this block into the ledger, catchup is complete
							// 블록은 여기에서 가져온 후 다른 곳에서 원장에 추가되었습니다. 계약만 이 블록을 원장에 추가할 수 있었습니다. 캐치업이 완료되었습니다.
							s.log.Infof("fetchAndWrite(%d): after fetching the block, it is already in the ledger. The catchup is complete", r)
							return false
						}
						s.log.Warnf("fetchAndWrite(%d): failed to validate block : %v", r, err)
						return false
					}
					err = s.ledger.AddValidatedBlock(*vb, *cert)
				} else {
					err = s.ledger.AddBlock(*block, *cert)
				}

				if err != nil {
					switch err.(type) {
					case ledgercore.ErrNonSequentialBlockEval:
						s.log.Infof("fetchAndWrite(%d): no need to re-evaluate historical block", r)
						return true
					case ledgercore.BlockInLedgerError:
						// the block was added to the ledger from elsewhere after fetching it here only the agreement could have added this block into the ledger, catchup is complete
						// 블록은 여기에서 가져온 후 다른 곳에서 원장에 추가되었습니다. 계약만 이 블록을 원장에 추가할 수 있었습니다. 캐치업이 완료되었습니다.
						s.log.Infof("fetchAndWrite(%d): after fetching the block, it is already in the ledger. The catchup is complete", r)
						return false
					case protocol.Error:
						if !s.protocolErrorLogged {
							logging.Base().Errorf("fetchAndWrite(%v): unrecoverable protocol error detected: %v", r, err)
							s.protocolErrorLogged = true
						}
					default:
						s.log.Errorf("fetchAndWrite(%v): ledger write failed: %v", r, err)
					}

					return false
				}
				s.log.Debugf("fetchAndWrite(%v): Wrote block to ledger", r)
				return true
			}
			s.log.Warnf("fetchAndWrite(%v): previous block doesn't exist (perhaps fetching block %v failed)", r, r-1)
			return false
		}
	}
	return false
}

type task func() basics.Round

func (s *Service) pipelineCallback(r basics.Round, thisFetchComplete chan bool, prevFetchCompleteChan chan bool, lookbackChan chan bool, peerSelector *peerSelector) func() basics.Round {
	return func() basics.Round {
		fetchResult := s.fetchAndWrite(r, prevFetchCompleteChan, lookbackChan, peerSelector)

		// the fetch result will be read at most twice (once as the lookback block and once as the prev block, so we write the result twice)
		// 가져오기 결과는 최대 두 번 읽힙니다(한 번은 lookback 블록으로, 한 번은 prev 블록으로 읽으므로 결과를 두 번 씁니다).
		thisFetchComplete <- fetchResult
		thisFetchComplete <- fetchResult

		if !fetchResult {
			s.log.Infof("pipelineCallback(%d): did not fetch or write the block", r)
			return 0
		}
		return r
	}
}

// TODO the following code does not handle the following case: seedLookback upgrades during fetch
// TODO 다음 코드는 다음 경우를 처리하지 않습니다. 가져오기 중 seedLookback 업그레이드
func (s *Service) pipelinedFetch(seedLookback uint64) {
	parallelRequests := s.parallelBlocks
	if parallelRequests < seedLookback {
		parallelRequests = seedLookback
	}

	completed := make(chan basics.Round, parallelRequests)
	taskCh := make(chan task, parallelRequests)
	var wg sync.WaitGroup

	defer func() {
		close(taskCh)
		wg.Wait()
		close(completed)
	}()

	peerSelector := s.createPeerSelector(true)

	if _, err := peerSelector.getNextPeer(); err == errPeerSelectorNoPeerPoolsAvailable {
		s.log.Debugf("pipelinedFetch: was unable to obtain a peer to retrieve the block from")
		return
	}

	// Invariant: len(taskCh) + (# pending writes to completed) <= N
	// 불변: len(taskCh) + (# 쓰기를 완료하기 위해 보류 중) <= N
	wg.Add(int(parallelRequests))
	for i := uint64(0); i < parallelRequests; i++ {
		go func() {
			defer wg.Done()
			for t := range taskCh {
				completed <- t() // This write to completed comes after a read from taskCh, so the invariant is preserved.
				// 이 쓰기 완료는 taskCh에서 읽은 후에 이루어지므로 불변이 유지됩니다.
			}
		}()
	}

	recentReqs := make([]chan bool, 0)
	for i := 0; i < int(seedLookback); i++ {
		// the fetch result will be read at most twice (once as the lookback block and once as the prev block, so we write the result twice)
		// 가져오기 결과는 최대 두 번 읽힙니다(한 번은 lookback 블록으로, 한 번은 prev 블록으로 읽으므로 결과를 두 번 씁니다).
		reqComplete := make(chan bool, 2)
		reqComplete <- true
		reqComplete <- true
		recentReqs = append(recentReqs, reqComplete)
	}

	from := s.ledger.NextRound()
	nextRound := from
	for ; nextRound < from+basics.Round(parallelRequests); nextRound++ {
		// If the next round is not supported
		// 다음 라운드가 지원되지 않는 경우
		if s.nextRoundIsNotSupported(nextRound) {
			// We may get here when (1) The service starts and gets to an unsupported round.
			// Since in this loop we do not wait for the requests to be written to the ledger, there is no guarantee that the unsupported round will be stopped in this case.
			// (1) 서비스가 시작되고 지원되지 않는 라운드에 도달하면 여기에 도달할 수 있습니다.
			// 이 루프에서는 요청이 원장에 기록될 때까지 기다리지 않기 때문에 이 경우 지원되지 않는 라운드가 중지된다는 보장은 없습니다.

			// (2) The unsupported round is detected in the "the rest" loop, but did not cancel because the last supported round was not yet written to the ledger.
			// (2) 지원되지 않는 라운드는 "rest" 루프에서 감지되지만 마지막 지원 라운드가 아직 원장에 기록되지 않았기 때문에 취소되지 않았습니다.

			// It is sufficient to check only in the first iteration, however checking in all in favor of code simplicity.\
			// 첫 번째 반복에서만 확인하는 것으로 충분하지만 코드 단순성을 위해 모두 확인합니다.
			s.handleUnsupportedRound(nextRound)
			break
		}

		currentRoundComplete := make(chan bool, 2)
		// len(taskCh) + (# pending writes to completed) increases by 1
		taskCh <- s.pipelineCallback(nextRound, currentRoundComplete, recentReqs[len(recentReqs)-1], recentReqs[len(recentReqs)-int(seedLookback)], peerSelector)
		recentReqs = append(recentReqs[1:], currentRoundComplete)
	}

	completedRounds := make(map[basics.Round]bool)
	// the rest
	for {
		select {
		case round := <-completed:
			if round == 0 {
				// there was an error
				return
			}
			// if we're writing a catchpoint file, stop catching up to reduce the memory pressure.
			// Once we finish writing the file we could resume with the catchup.
			// 캐치포인트 파일을 작성하는 경우 캐치업을 중지하여 메모리 압력을 줄입니다.
			// 파일 쓰기를 마치면 캐치업으로 다시 시작할 수 있습니다.
			if s.ledger.IsWritingCatchpointFile() {
				s.log.Info("Catchup is stopping due to catchpoint file being written")
				s.suspendForCatchpointWriting = true
				return
			}
			completedRounds[round] = true
			// fetch rounds we can validate
			// 검증할 수 있는 라운드 가져오기
			for completedRounds[nextRound-basics.Round(parallelRequests)] {
				// If the next round is not supported
				// 다음 라운드가 지원되지 않는 경우
				if s.nextRoundIsNotSupported(nextRound) {
					s.handleUnsupportedRound(nextRound)
					return
				}
				delete(completedRounds, nextRound)

				currentRoundComplete := make(chan bool, 2)
				// len(taskCh) + (# pending writes to completed) increases by 1
				taskCh <- s.pipelineCallback(nextRound, currentRoundComplete, recentReqs[len(recentReqs)-1], recentReqs[0], peerSelector)
				recentReqs = append(recentReqs[1:], currentRoundComplete)
				nextRound++
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// periodicSync periodically asks the network for its latest round and syncs if we've fallen behind (also if our ledger stops advancing)
// periodSync는 주기적으로 네트워크에 최신 라운드를 요청하고 뒤처지면 동기화합니다(원장이 더 이상 진행되지 않는 경우에도).
func (s *Service) periodicSync() {
	defer close(s.done)
	// if the catchup is disabled in the config file, just skip it.
	// 구성 파일에서 캐치업이 비활성화되어 있으면 그냥 건너뜁니다.
	if s.parallelBlocks != 0 && !s.cfg.DisableNetworking {
		// The following request might be redundant, but it ensures we wait long enough for the DNS records to be loaded, which are required for the sync operation.
		// 다음 요청은 중복될 수 있지만 동기화 작업에 필요한 DNS 레코드가 로드될 때까지 충분히 오래 기다려야 합니다.
		s.net.RequestConnectOutgoing(false, s.ctx.Done())
		s.sync()
	}
	stuckInARow := 0
	sleepDuration := s.deadlineTimeout
	for {
		currBlock := s.ledger.LastRound()
		select {
		case <-s.ctx.Done():
			return
		case <-s.ledger.Wait(currBlock + 1):
			// Ledger moved forward; likely to be by the agreement service.
			// 원장이 앞으로 이동했습니다. 계약 서비스에 의해 가능성이 높습니다.
			stuckInARow = 0
			// go to sleep for a short while, for a random duration.
			// we want to sleep for a random duration since it would "de-syncronize" us from the ledger advance sync
			// 임의의 시간 동안 잠시 동안 절전 모드로 전환합니다.
			// 우리는 원장 사전 동기화에서 우리를 "비동기화"할 것이기 때문에 임의의 기간 동안 잠자기를 원합니다.
			sleepDuration = time.Duration(crypto.RandUint63()) % s.deadlineTimeout
			continue
		case <-time.After(sleepDuration):
			if sleepDuration < s.deadlineTimeout || s.cfg.DisableNetworking {
				sleepDuration = s.deadlineTimeout
				continue
			}
			// if the catchup is disabled in the config file, just skip it.
			// 구성 파일에서 캐치업이 비활성화되어 있으면 그냥 건너뜁니다.
			if s.parallelBlocks == 0 {
				continue
			}
			// check to see if we're currently writing a catchpoint file. If so, wait longer before attempting again.
			// 현재 캐치포인트 파일을 작성 중인지 확인합니다. 그렇다면 다시 시도하기 전에 더 오래 기다리십시오.
			if s.ledger.IsWritingCatchpointFile() {
				// keep the existing sleep duration and try again later.
				// 기존 절전 시간을 유지하고 나중에 다시 시도합니다.
				continue
			}
			s.suspendForCatchpointWriting = false
			s.log.Info("It's been too long since our ledger advanced; resyncing")
			s.sync()
		case cert := <-s.unmatchedPendingCertificates:
			// the agreement service has a valid certificate for a block, but not the block itself.
			if s.cfg.DisableNetworking {
				s.log.Warnf("the local node is missing block %d, however, the catchup would not be able to provide it when the network is disabled.", cert.Cert.Round)
				continue
			}
			s.syncCert(&cert)
		}

		if currBlock == s.ledger.LastRound() {
			stuckInARow++
		} else {
			stuckInARow = 0
		}
		if stuckInARow == s.cfg.CatchupFailurePeerRefreshRate {
			stuckInARow = 0
			// TODO: RequestConnectOutgoing in terms of Context
			s.net.RequestConnectOutgoing(true, s.ctx.Done())
		}
	}
}

// Syncs the client with the network. sync asks the network for last known block and tries to sync the system up the to the highest number it gets.
// 클라이언트를 네트워크와 동기화합니다. sync는 네트워크에 마지막으로 알려진 블록을 요청하고 가장 높은 수까지 시스템을 동기화하려고 시도합니다.
func (s *Service) sync() {
	// Only run sync once at a time
	// Store start time of sync - in NS so we can compute time.Duration (which is based on NS)
	// 한 번에 한 번만 동기화 실행
	// 동기화 시작 시간 저장 - 시간을 계산할 수 있도록 NS에 저장합니다.Duration(NS 기반)
	start := time.Now()

	timeInNS := start.UnixNano()
	if !atomic.CompareAndSwapInt64(&s.syncStartNS, 0, timeInNS) {
		s.log.Infof("resuming previous sync from %d (now=%d)", atomic.LoadInt64(&s.syncStartNS), timeInNS)
	}

	pr := s.ledger.LastRound()

	s.log.EventWithDetails(telemetryspec.ApplicationState, telemetryspec.CatchupStartEvent, telemetryspec.CatchupStartEventDetails{
		StartRound: uint64(pr),
	})

	seedLookback := uint64(2)
	proto, err := s.ledger.ConsensusParams(pr)
	if err != nil {
		s.log.Errorf("catchup: could not get consensus parameters for round %v: %v", pr, err)
	} else {
		seedLookback = proto.SeedLookback
	}
	s.pipelinedFetch(seedLookback)

	initSync := false

	// if the catchupWriting flag is set, it means that we aborted the sync due to the ledger writing the catchup file.
	// catchupWriting 플래그가 설정되어 있으면 캐치업 파일을 작성하는 원장으로 인해 동기화가 중단되었음을 의미합니다.
	if !s.suspendForCatchpointWriting {
		// in that case, don't change the timer so that the "timer" would keep running.
		// 이 경우 "타이머"가 계속 실행되도록 타이머를 변경하지 마십시오.
		atomic.StoreInt64(&s.syncStartNS, 0)

		// close the initial sync channel if not already close
		// 아직 닫히지 않은 경우 초기 동기화 채널을 닫습니다.
		if atomic.CompareAndSwapUint32(&s.initialSyncNotified, 0, 1) {
			close(s.InitialSyncDone)
			initSync = true
		}
	}

	elapsedTime := time.Now().Sub(start)
	s.log.EventWithDetails(telemetryspec.ApplicationState, telemetryspec.CatchupStopEvent, telemetryspec.CatchupStopEventDetails{
		StartRound: uint64(pr),
		EndRound:   uint64(s.ledger.LastRound()),
		Time:       elapsedTime,
		InitSync:   initSync,
	})
	s.log.Infof("Catchup Service: finished catching up, now at round %v (previously %v). Total time catching up %v.", s.ledger.LastRound(), pr, elapsedTime)
}

// syncCert retrieving a single round identified by the provided certificate and adds it to the ledger.
// The sync function attempts to keep trying to fetch the matching block or abort when the catchup service exits.
// syncCert는 제공된 인증서로 식별되는 단일 라운드를 검색하여 원장에 추가합니다.
// sync 함수는 일치하는 블록을 계속 가져오려고 시도하거나 캐치업 서비스가 종료될 때 중단합니다.
func (s *Service) syncCert(cert *PendingUnmatchedCertificate) {
	// we want to fetch a single round. no need to be concerned about lookback.
	// 우리는 단일 라운드를 가져오기를 원합니다. 룩백에 대해 걱정할 필요가 없습니다.
	s.fetchRound(cert.Cert, cert.VoteVerifier)
}

// TODO this doesn't actually use the digest from cert!
func (s *Service) fetchRound(cert agreement.Certificate, verifier *agreement.AsyncVoteVerifier) {
	// is there any point attempting to retrieve the block ?
	// 블록을 검색하려는 지점이 있습니까?
	if s.nextRoundIsNotSupported(cert.Round) {
		// we might get here if the agreement service was seeing the certs votes for the next block, without seeing the actual block.
		// Since it hasn't seen the block, it couldn't tell that it's an unsupported protocol, and would try to request it from the catchup.
		// 계약 서비스가 실제 블록을 보지 않고 다음 블록에 대한 인증서 투표를 보고 있는 경우 여기에 도달할 수 있습니다.
		// 블록을 보지 못했기 때문에 지원되지 않는 프로토콜임을 알 수 없으며 캐치업에서 요청을 시도합니다.
		s.handleUnsupportedRound(cert.Round)
		return
	}

	blockHash := bookkeeping.BlockHash(cert.Proposal.BlockDigest) // semantic digest (i.e., hash of the block header), not byte-for-byte digest
	// 시맨틱 다이제스트(즉, 블록 헤더의 해시)가 아닌 바이트 단위 다이제스트
	peerSelector := s.createPeerSelector(false)
	for s.ledger.LastRound() < cert.Round {
		psp, getPeerErr := peerSelector.getNextPeer()
		if getPeerErr != nil {
			s.log.Debugf("fetchRound: was unable to obtain a peer to retrieve the block from")
			s.net.RequestConnectOutgoing(true, s.ctx.Done())
			continue
		}
		peer := psp.Peer

		// Ask the fetcher to get the block somehow
		// 페처에게 어떻게든 블록을 가져오도록 요청합니다.
		block, fetchedCert, _, err := s.innerFetch(cert.Round, peer)

		if err != nil {
			select {
			case <-s.ctx.Done():
				logging.Base().Debugf("fetchRound was asked to quit before we could acquire the block")
				return
			default:
			}
			logging.Base().Warnf("fetchRound could not acquire block, fetcher errored out: %v", err)
			peerSelector.rankPeer(psp, peerRankDownloadFailed)
			continue
		}

		if block.Hash() == blockHash && block.ContentsMatchHeader() {
			s.ledger.EnsureBlock(block, cert)
			return
		}
		// Otherwise, fetcher gave us the wrong block
		// 그렇지 않으면 fetcher가 잘못된 블록을 제공했습니다.
		logging.Base().Warnf("fetcher gave us bad/wrong block (for round %d): fetched hash %v; want hash %v", cert.Round, block.Hash(), blockHash)
		peerSelector.rankPeer(psp, peerRankInvalidDownload)

		// As a failsafe, if the cert we fetched is valid but for the wrong block, panic as loudly as possible
		// 안전 장치로 가져온 인증서가 유효하지만 잘못된 블록에 대해 가능한 한 큰 소리로 패닉
		if cert.Round == fetchedCert.Round &&
			cert.Proposal.BlockDigest != fetchedCert.Proposal.BlockDigest &&
			fetchedCert.Authenticate(*block, s.ledger, verifier) == nil {
			s := "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n"
			s += "!!!!!!!!!! FORK DETECTED !!!!!!!!!!!\n"
			s += "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n"
			s += "fetchRound called with a cert authenticating block with hash %v.\n"
			s += "We fetched a valid cert authenticating a different block, %v. This indicates a fork.\n\n"
			s += "Cert from our agreement service:\n%#v\n\n"
			s += "Cert from the fetcher:\n%#v\n\n"
			s += "Block from the fetcher:\n%#v\n\n"
			s += "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n"
			s += "!!!!!!!!!! FORK DETECTED !!!!!!!!!!!\n"
			s += "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n"
			s = fmt.Sprintf(s, cert.Proposal.BlockDigest, fetchedCert.Proposal.BlockDigest, cert, fetchedCert, block)
			fmt.Println(s)
			logging.Base().Error(s)
		}
	}
}

// nextRoundIsNotSupported returns true if the next round upgrades to a protocol version which is not supported.
// In case of an error, it returns false
// nextRoundIsNotSupported는 다음 라운드가 지원되지 않는 프로토콜 버전으로 업그레이드되면 true를 반환합니다.
// 오류가 발생하면 false를 반환합니다.
func (s *Service) nextRoundIsNotSupported(nextRound basics.Round) bool {
	lastLedgerRound := s.ledger.LastRound()
	supportedUpgrades := config.Consensus

	block, err := s.ledger.Block(lastLedgerRound)
	if err != nil {
		s.log.Errorf("nextRoundIsNotSupported: could not retrieve last block (%d) from the ledger : %v", lastLedgerRound, err)
		return false
	}
	bh := block.BlockHeader
	_, isSupportedUpgrade := supportedUpgrades[bh.NextProtocol]

	if bh.NextProtocolSwitchOn > 0 && !isSupportedUpgrade {
		// Save the last supported round number
		// It is not necessary to check bh.NextProtocolSwitchOn < s.lastSupportedRound
		// since there cannot be two protocol updates scheduled.
		// 마지막으로 지원되는 라운드 번호를 저장합니다.
		// bh.NextProtocolSwitchOn < s.lastSupportedRound를 확인할 필요가 없습니다.
		// 두 개의 프로토콜 업데이트가 예약될 수 없기 때문입니다.
		s.lastSupportedRound = bh.NextProtocolSwitchOn - 1

		if nextRound >= bh.NextProtocolSwitchOn {
			return true
		}
	}
	return false
}

// handleUnSupportedRound receives a verified unsupported round: nextUnsupportedRound
// Checks if the last supported round was added to the ledger, and stops the service.
// handleUnSupportedRound는 확인된 지원되지 않는 라운드를 수신합니다. nextUnsupportedRound
// 마지막 지원 라운드가 원장에 추가되었는지 확인하고 서비스를 중지합니다.
func (s *Service) handleUnsupportedRound(nextUnsupportedRound basics.Round) {

	s.log.Infof("Catchup Service: round %d is not approved. Service will stop once the last supported round is added to the ledger.",
		nextUnsupportedRound)

	// If the next round is an unsupported round, need to stop the catchup service.
	// Should stop after the last supported round is added to the ledger.
	// 다음 라운드가 지원되지 않는 라운드인 경우 캐치업 서비스를 중지해야 합니다.
	// 지원되는 마지막 라운드가 원장에 추가된 후 중지되어야 합니다.
	lr := s.ledger.LastRound()
	// Ledger writes are in order. >= guarantees last supported round is added to the ledger.
	// 원장 쓰기가 순서대로 진행됩니다. >=는 마지막 지원 라운드가 원장에 추가되도록 보장합니다.
	if lr >= s.lastSupportedRound {
		s.log.Infof("Catchup Service: finished catching up to the last supported round %d. The subsequent rounds are not supported. Service is stopping.",
			lr)
		s.cancel()
	}
}

func (s *Service) createPeerSelector(pipelineFetch bool) *peerSelector {
	var peerClasses []peerClass
	if s.cfg.EnableCatchupFromArchiveServers {
		if pipelineFetch {
			if s.cfg.NetAddress != "" { // Relay node
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookArchivers},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersPhonebookRelays},
					{initialRank: peerRankInitialFourthPriority, peerClass: network.PeersConnectedIn},
				}
			} else {
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersPhonebookArchivers},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersPhonebookRelays},
				}
			}
		} else {
			if s.cfg.NetAddress != "" { // Relay node
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersConnectedIn},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersPhonebookRelays},
					{initialRank: peerRankInitialFourthPriority, peerClass: network.PeersPhonebookArchivers},
				}
			} else {
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookRelays},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersPhonebookArchivers},
				}
			}
		}
	} else {
		if pipelineFetch {
			if s.cfg.NetAddress != "" { // Relay node
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookRelays},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersConnectedIn},
				}
			} else {
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookRelays},
				}
			}
		} else {
			if s.cfg.NetAddress != "" { // Relay node
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersConnectedIn},
					{initialRank: peerRankInitialThirdPriority, peerClass: network.PeersPhonebookRelays},
				}
			} else {
				peerClasses = []peerClass{
					{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersConnectedOut},
					{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookRelays},
				}
			}
		}
	}
	return makePeerSelector(s.net, peerClasses)
}
