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

package network

import (
	"sort"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/crypto"
)

type pmStage int

const (
	pmStagePresync    pmStage = iota // pmStagePresync used as a warmup for the monitoring. it ensures that we've received at least a single message from each peer, and that we've waited enough time before attempting to sync up.
	pmStageSync       pmStage = iota // pmStageSync is syncing up the peer message streams. It exists once all the connections have demonstrated a given idle time.
	pmStageAccumulate pmStage = iota // pmStageAccumulate monitors streams and accumulate the messages between the connections.
	pmStageStopping   pmStage = iota // pmStageStopping keep monitoring the streams, but do not accept new messages. It tries to expire pending messages until all pending messages expires.
	pmStageStopped    pmStage = iota // pmStageStopped is the final stage; it means that the performance monitor reached a conclusion regarding the performance statistics
	// 모니터링을 위한 준비로 pmStagePresync를 사용합니다. 각 피어로부터 최소한 하나의 메시지를 받았고 동기화를 시도하기 전에 충분한 시간을 기다렸는지 확인합니다.
	// pmStageSync는 피어 메시지 스트림을 동기화하고 있습니다. 모든 연결이 주어진 유휴 시간을 나타내면 존재합니다.
	// pmStageAccumulate는 스트림을 모니터링하고 연결 사이의 메시지를 누적합니다.
	// pmStageStopping은 스트림을 계속 모니터링하지만 새 메시지를 수락하지 않습니다. 보류 중인 모든 메시지가 만료될 때까지 보류 중인 메시지를 만료하려고 합니다.
	// pmStageStopped는 마지막 단계입니다. 성능 모니터가 성능 통계에 대한 결론에 도달했음을 의미합니다.
)

const (
	pmPresyncTime                   = 10 * time.Second
	pmSyncIdleTime                  = 2 * time.Second
	pmSyncMaxTime                   = 25 * time.Second
	pmAccumulationTime              = 60 * time.Second
	pmAccumulationTimeRange         = 30 * time.Second
	pmAccumulationIdlingTime        = 2 * time.Second
	pmMaxMessageWaitTime            = 15 * time.Second
	pmUndeliveredMessagePenaltyTime = 5 * time.Second
	pmDesiredMessegeDelayThreshold  = 50 * time.Millisecond
	pmMessageBucketDuration         = time.Second
)

// pmMessage is the internal storage for a single message. We save the time the message arrived from each of the peers.
// pmMessage는 단일 메시지의 내부 저장소입니다. 각 피어에서 메시지가 도착한 시간을 절약합니다.
type pmMessage struct {
	peerMsgTime map[Peer]int64 // for each peer, when did we see a message the first time
	// ↑ 각 피어에 대해 언제 메시지를 처음 보았습니까?
	firstPeerTime int64 // the timestamp of the first peer that has seen this message.
	// ↑ 이 메시지를 본 첫 번째 피어의 타임스탬프.
}

// pmPeerStatistics is the per-peer resulting datastructure of the performance analysis.
// pmPeerStatistics는 성능 분석의 피어별 결과 데이터 구조입니다.
type pmPeerStatistics struct {
	peer             Peer    // the peer interface
	peerDelay        int64   // the peer avarage relative message delay // 피어 평균 상대 메시지 지연
	peerFirstMessage float32 // what percentage of the messages were delivered by this peer before any other peer // 이 피어가 다른 피어보다 먼저 메시지를 전달한 비율
}

// pmStatistics is the resulting datastructure of the performance analysis.
// pmStatistics는 성능 분석의 결과 데이터 구조입니다.
type pmStatistics struct {
	peerStatistics []pmPeerStatistics // an ordered list of the peers performance statistics // 피어 성능 통계의 정렬된 목록
	messageCount   int64              // the number of messages used to calculate the above statistics // 위의 통계를 계산하는 데 사용된 메시지 수
}

// pmPendingMessageBucket is used to buffer messages in time ranges blocks.
// pmPendingMessageBucket은 시간 범위 블록에서 메시지를 버퍼링하는 데 사용됩니다.
type pmPendingMessageBucket struct {
	messages  map[crypto.Digest]*pmMessage // the pendingMessages map contains messages that haven't been received from all the peers within the pmMaxMessageWaitTime, and belong to the timerange of this bucket.
	startTime int64                        // the inclusive start-range of the timestamp which bounds the messages ranges which would go into this bucket. Time is in nano seconds UTC epoch time.
	endTime   int64                        // the inclusive end-range of the timestamp which bounds the messages ranges which would go into this bucket. Time is in nano seconds UTC epoch time.
	// pendingMessages 맵에는 pmMaxMessageWaitTime 내의 모든 피어로부터 수신되지 않은 메시지가 포함되며 이 버킷의 시간 범위에 속합니다.
	// 이 버킷에 들어갈 메시지 범위를 제한하는 타임스탬프의 시작 범위를 포함합니다. 시간은 나노초 UTC 에포크 시간입니다.
	// 이 버킷에 들어갈 메시지 범위를 제한하는 타임스탬프의 끝 범위. 시간은 나노초 UTC 에포크 시간입니다.
}

// connectionPerformanceMonitor is the connection monitor datatype. We typically would like to have a single monitor for all the outgoing connections.
// 연결 성능 모니터는 연결 모니터 데이터 유형입니다. 우리는 일반적으로 모든 나가는 연결에 대해 단일 모니터를 원합니다.
type connectionPerformanceMonitor struct {
	deadlock.Mutex
	monitoredConnections map[Peer]bool // the map of the connection we're going to monitor. Messages coming from other connections would be ignored.
	// ↑ 우리가 모니터링할 연결의 맵입니다. 다른 연결에서 오는 메시지는 무시됩니다.
	monitoredMessageTags map[Tag]bool // the map of the message tags we're interested in monitoring. Messages that aren't broadcast-type typically would be a good choice here.
	// ↑ 모니터링하려는 메시지 태그의 맵입니다. 브로드캐스트 유형이 아닌 메시지는 일반적으로 여기에서 좋은 선택입니다.
	stage pmStage // the performance monitoring stage.
	// ↑ 성능 모니터링 단계.
	peerLastMsgTime map[Peer]int64 // the map describing the last time we received a message from each of the peers.
	// ↑ 각 피어로부터 메시지를 마지막으로 수신한 시간을 설명하는 맵.
	lastIncomingMsgTime int64 // the time at which the last message was received from any of the peers.
	// ↑ 피어로부터 마지막 메시지를 받은 시간.
	stageStartTime int64 // the timestamp at which we switched to the current stage.
	// ↑ 현재 단계로 전환한 타임스탬프입니다.
	pendingMessagesBuckets []*pmPendingMessageBucket // the pendingMessagesBuckets array contains messages buckets for messages that haven't been received from all the peers within the pmMaxMessageWaitTime
	// ↑ pendingMessagesBuckets 배열에는 pmMaxMessageWaitTime 내의 모든 피어로부터 수신되지 않은 메시지에 대한 메시지 버킷이 포함됩니다.
	connectionDelay map[Peer]int64 // contains the total delay we've sustained by each peer when we're in stages pmStagePresync-pmStageStopping and the average delay after that. ( in nano seconds )
	// ↑ pmStagePresync-pmStageStopping 단계에 있을 때 각 피어가 유지한 총 지연과 그 이후의 평균 지연을 포함합니다. (나노초 단위)
	firstMessageCount map[Peer]int64 // maps the peers to their accumulated first messages ( the number of times a message seen coming from this peer first )
	// ↑ 피어를 누적된 첫 번째 메시지에 매핑합니다(이 피어에서 메시지가 먼저 오는 것을 본 횟수).
	msgCount int64 // total number of messages that we've accumulated.
	// ↑ 누적된 총 메시지 수.
	accumulationTime int64 // the duration of which we're going to accumulate messages. This will get randomized to prevent cross-node synchronization.
	// ↑ 메시지를 누적할 기간. 노드 간 동기화를 방지하기 위해 무작위로 지정됩니다.
}

// makeConnectionPerformanceMonitor creates a new performance monitor instance, that is configured for monitoring the given message tags.
// makeConnectionPerformanceMonitor 는 주어진 메시지 태그를 모니터링하도록 구성된 새 성능 모니터 인스턴스를 생성합니다.
func makeConnectionPerformanceMonitor(messageTags []Tag) *connectionPerformanceMonitor {
	msgTagMap := make(map[Tag]bool, len(messageTags))
	for _, tag := range messageTags {
		msgTagMap[tag] = true
	}
	return &connectionPerformanceMonitor{
		monitoredConnections: make(map[Peer]bool, 0),
		monitoredMessageTags: msgTagMap,
	}
}

// GetPeersStatistics returns the statistics result of the performance monitoring, once these becomes available.
// GetPeersStatistics 는 성능 모니터링의 통계 결과를 사용할 수 있게 되면 반환합니다.
// otherwise, it returns nil.
// 그렇지 않으면 nil 을 반환합니다.
func (pm *connectionPerformanceMonitor) GetPeersStatistics() (stat *pmStatistics) {
	pm.Lock()
	defer pm.Unlock()
	if pm.stage != pmStageStopped || len(pm.connectionDelay) == 0 {
		return nil
	}
	stat = &pmStatistics{
		peerStatistics: make([]pmPeerStatistics, 0, len(pm.connectionDelay)),
		messageCount:   pm.msgCount,
	}
	for peer, delay := range pm.connectionDelay {
		peerStat := pmPeerStatistics{
			peer:      peer,
			peerDelay: delay,
		}
		if pm.msgCount > 0 {
			peerStat.peerFirstMessage = float32(pm.firstMessageCount[peer]) / float32(pm.msgCount)
		}
		stat.peerStatistics = append(stat.peerStatistics, peerStat)
	}
	sort.Slice(stat.peerStatistics, func(i, j int) bool {
		return stat.peerStatistics[i].peerDelay > stat.peerStatistics[j].peerDelay
	})
	return
}

// ComparePeers compares the given peers list or the existing peers being monitored.
// ComparePeers는 주어진 피어 목록 또는 모니터링 중인 기존 피어를 비교합니다.
// If the peers list have changed since Reset was called, it would return false.
// Reset이 호출된 이후 피어 목록이 변경된 경우 false를 반환합니다.
// The method is insensitive to peer ordering and uses the peer interface pointer to determine equality.
// 이 메서드는 피어 순서에 민감하지 않으며 피어 인터페이스 포인터를 사용하여 동등성을 결정합니다.
func (pm *connectionPerformanceMonitor) ComparePeers(peers []Peer) bool {
	pm.Lock()
	defer pm.Unlock()
	for _, peer := range peers {
		if pm.monitoredConnections[peer] == false {
			return false
		}
	}
	return len(peers) == len(pm.monitoredConnections)
}

// Reset updates the existing peers list to the one provided. The Reset method is expected to be used
// in three scenarios :
// 1. clearing out the existing monitoring - which brings it to initial state and disable monitoring.
// 2. change monitored peers - in case we've had some of our peers disconnected/reconnected during the monitoring process.
// 3. start monitoring
// ========================================================================================
// 재설정은 기존 피어 목록을 제공된 것으로 업데이트합니다. 재설정 방법이 사용될 것으로 예상됩니다.
// 세 가지 시나리오에서:
// 1. 기존 모니터링 지우기 - 이를 초기 상태로 만들고 모니터링을 비활성화합니다.
// 2. 모니터링되는 피어 변경 - 모니터링 프로세스 중에 일부 피어가 연결 해제/재연결된 경우.
// 3. 모니터링 시작
func (pm *connectionPerformanceMonitor) Reset(peers []Peer) {
	pm.Lock()
	defer pm.Unlock()
	pm.monitoredConnections = make(map[Peer]bool, len(peers))
	pm.peerLastMsgTime = make(map[Peer]int64, len(peers))
	pm.connectionDelay = make(map[Peer]int64, len(peers))
	pm.firstMessageCount = make(map[Peer]int64, len(peers))
	pm.msgCount = 0
	pm.advanceStage(pmStagePresync, time.Now().UnixNano())
	pm.accumulationTime = int64(pmAccumulationTime) + int64(crypto.RandUint63())%int64(pmAccumulationTime)

	for _, peer := range peers {
		pm.monitoredConnections[peer] = true
		pm.peerLastMsgTime[peer] = pm.stageStartTime
		pm.connectionDelay[peer] = 0
		pm.firstMessageCount[peer] = 0
	}

}

// Notify is the single entrypoint for an incoming message processing.
// When an outgoing connection is being monitored, it would make a call to Notify, sending the incoming message details.
// The Notify function will forward this notification to the current stage processing function.
// ======================================================================
// 알림은 들어오는 메시지 처리를 위한 단일 진입점입니다.
// 나가는 연결이 모니터링되면 알림을 호출하여 들어오는 메시지 세부 정보를 보냅니다.
// Notify 함수는 이 알림을 현재 단계 처리 함수로 전달합니다.
func (pm *connectionPerformanceMonitor) Notify(msg *IncomingMessage) {
	pm.Lock()
	defer pm.Unlock()
	if pm.monitoredConnections[msg.Sender] == false {
		return
	}
	if pm.monitoredMessageTags[msg.Tag] == false {
		return
	}
	switch pm.stage {
	case pmStagePresync:
		pm.notifyPresync(msg)
	case pmStageSync:
		pm.notifySync(msg)
	case pmStageAccumulate:
		pm.notifyAccumulate(msg)
	case pmStageStopping:
		pm.notifyStopping(msg)
	default: // pmStageStopped
	}
}

// notifyPresync waits until pmPresyncTime has passed and monitor the last arrivial time of messages from each of the peers.
// notifyPresync 는 pmPresyncTime 이 지나갈 때까지 대기하고 각 피어에서 메시지가 마지막으로 도착한 시간을 모니터링합니다.
func (pm *connectionPerformanceMonitor) notifyPresync(msg *IncomingMessage) {
	pm.peerLastMsgTime[msg.Sender] = msg.Received
	if (msg.Received - pm.stageStartTime) < int64(pmPresyncTime) {
		return
	}
	// presync complete. move to the next stage.\
	// 사전 동기화 완료. 다음 단계로 이동합니다.
	noMsgPeers := make(map[Peer]bool, 0)
	for peer, lastMsgTime := range pm.peerLastMsgTime {
		if lastMsgTime == pm.stageStartTime {
			// we haven't received a single message from this peer during the entire presync time.
			// 전체 사전 동기화 시간 동안 이 피어로부터 단일 메시지를 받지 못했습니다.
			noMsgPeers[peer] = true
		}
	}
	if len(noMsgPeers) >= (len(pm.peerLastMsgTime) / 2) {
		// if more than half of the peers have not sent us a single message,
		// extend the presync time. We might be in agreement recovery, where we have very low
		// traffic. If this becomes a repeated issue, it will get solved by the
		// clique detection algorithm and some of the nodes would get disconnected.
		// ============================================================================
		// 절반 이상의 피어가 단일 메시지를 보내지 않은 경우
		// 사전 동기화 시간을 연장합니다. 우리는 계약 회복이 매우 낮을 수 있습니다.
		// 교통. 이것이 반복되는 문제가 된다면
		// clique 감지 알고리즘과 일부 노드의 연결이 끊어집니다.
		pm.stageStartTime = msg.Received
		return
	}
	if len(noMsgPeers) > 0 {
		// we have one or more peers that did not send a single message thoughtout the presync time.
		// ( but less than half ). since we cannot rely on these to send us messages in the future, we'll disconnect from these peers.
		// =======
		// 사전 동기화 시간을 고려하여 단일 메시지를 보내지 않은 피어가 하나 이상 있습니다.
		// ( 하지만 절반 미만 ). 앞으로 우리에게 메시지를 보내기 위해 이것들에 의존할 수 없기 때문에 우리는 이 피어들로부터 연결을 끊을 것입니다.
		pm.advanceStage(pmStageStopped, msg.Received)
		for peer := range pm.monitoredConnections {
			if noMsgPeers[peer] {
				pm.connectionDelay[peer] = int64(pmUndeliveredMessagePenaltyTime)
			} else {
				pm.connectionDelay[peer] = 0
			}
		}
		return
	}
	pm.lastIncomingMsgTime = msg.Received
	// otherwise, once we received a message from each of the peers, move to the sync stage.
	// 그렇지 않으면 각 피어로부터 메시지를 받으면 동기화 단계로 이동합니다.
	pm.advanceStage(pmStageSync, msg.Received)
}

// notifySync waits for all the peers connection's to go into an idle phase.
// notifySync는 모든 피어 연결이 유휴 상태가 될 때까지 기다립니다.
// when we go into this stage, the peerLastMsgTime will be already updated with the recent message time per peer.
// 이 단계에 들어가면 peerLastMsgTime이 이미 피어당 최근 메시지 시간으로 업데이트됩니다.
func (pm *connectionPerformanceMonitor) notifySync(msg *IncomingMessage) {
	minMsgInterval := pm.updateMessageIdlingInterval(msg.Received)
	if minMsgInterval > int64(pmSyncIdleTime) || (msg.Received-pm.stageStartTime > int64(pmSyncMaxTime)) {
		// if we hit the first expression, then it means that we've managed to sync up the connections.
		// 첫 번째 표현식을 치면 연결을 동기화할 수 있음을 의미합니다.
		// otherwise, we've failed to sync up the connections.
		// 그렇지 않으면 연결을 동기화하지 못했습니다.
		// That's not great, as we're likely to have some "penalties" applied, but we can't do much about it.
		// "벌칙"이 적용될 가능성이 높기 때문에 그다지 좋지는 않지만 이에 대해 많은 것을 할 수는 없습니다.
		pm.accumulateMessage(msg, true)
		pm.advanceStage(pmStageAccumulate, msg.Received)
	}
}

// notifyAccumulate accumulate the incoming message as needed, and waiting between pm.accumulationTime to
// (pm.accumulationTime + pmAccumulationTimeRange) before moving to the next stage.
// =======
// notifyAccumulate는 필요에 따라 들어오는 메시지를 누적하고 pm.accumulationTime 사이에서 대기합니다.
// (pm.accumulationTime + pmAccumulationTimeRange) 다음 단계로 이동하기 전.
func (pm *connectionPerformanceMonitor) notifyAccumulate(msg *IncomingMessage) {
	minMsgInterval := pm.updateMessageIdlingInterval(msg.Received)
	if msg.Received-pm.stageStartTime >= pm.accumulationTime {
		if minMsgInterval > int64(pmAccumulationIdlingTime) ||
			(msg.Received-pm.stageStartTime >= pm.accumulationTime+int64(pmAccumulationTimeRange)) {
			// move to the next stage.
			pm.advanceStage(pmStageStopping, msg.Received)
			return
		}
	}
	pm.accumulateMessage(msg, true)
	pm.pruneOldMessages(msg.Received)
}

// notifyStopping attempts to stop the message accumulation.
// Once we reach this stage, no new messages are being added, and old pending messages are being pruned.
// Once all messages are pruned, it moves to the next stage.
// ======
// notifyStopping 은 메시지 누적을 중지하려고 시도합니다.
// 이 단계에 도달하면 새 메시지가 추가되지 않고 보류 중인 오래된 메시지가 정리됩니다.
// 모든 메시지가 제거되면 다음 단계로 이동합니다.
func (pm *connectionPerformanceMonitor) notifyStopping(msg *IncomingMessage) {
	pm.accumulateMessage(msg, false)
	pm.pruneOldMessages(msg.Received)
	if len(pm.pendingMessagesBuckets) > 0 {
		return
	}
	// time to wrap up.
	if pm.msgCount > 0 {
		for peer := range pm.monitoredConnections {
			pm.connectionDelay[peer] /= int64(pm.msgCount)
		}
	}
	pm.advanceStage(pmStageStopped, msg.Received)
}

// advanceStage set the stage variable and update the stage start time.
// AdvanceStage는 스테이지 변수를 설정하고 스테이지 시작 시간을 업데이트합니다.
func (pm *connectionPerformanceMonitor) advanceStage(newStage pmStage, now int64) {
	pm.stage = newStage
	pm.stageStartTime = now
}

// updateMessageIdlingInterval updates the last message received timestamps and determines how long it has been since the last message was received on any of the incoming peers
// updateMessageIdlingInterval 은 마지막 메시지 수신 타임스탬프를 업데이트하고 들어오는 피어에서 마지막 메시지가 수신된 이후 경과된 시간을 결정합니다.
func (pm *connectionPerformanceMonitor) updateMessageIdlingInterval(now int64) (minMsgInterval int64) {
	currentIncomingMsgTime := pm.lastIncomingMsgTime
	if pm.lastIncomingMsgTime < now {
		pm.lastIncomingMsgTime = now
	}
	if currentIncomingMsgTime <= now {
		return now - currentIncomingMsgTime
	}
	return 0
}

func (pm *connectionPerformanceMonitor) pruneOldMessages(now int64) {
	oldestMessage := now - int64(pmMaxMessageWaitTime)
	prunedBucketsCount := 0
	for bucketIdx, currentMsgBucket := range pm.pendingMessagesBuckets {
		if currentMsgBucket.endTime > oldestMessage {
			pm.pendingMessagesBuckets[bucketIdx-prunedBucketsCount] = currentMsgBucket
			continue
		}
		for _, pendingMsg := range currentMsgBucket.messages {
			for peer := range pm.monitoredConnections {
				if msgTime, hasPeer := pendingMsg.peerMsgTime[peer]; hasPeer {
					msgDelayInterval := msgTime - pendingMsg.firstPeerTime
					pm.connectionDelay[peer] += msgDelayInterval
				} else {
					// we never received this message from this peer.
					pm.connectionDelay[peer] += int64(pmUndeliveredMessagePenaltyTime)
				}
			}
		}
		prunedBucketsCount++
	}
	pm.pendingMessagesBuckets = pm.pendingMessagesBuckets[:len(pm.pendingMessagesBuckets)-prunedBucketsCount]
}

func (pm *connectionPerformanceMonitor) accumulateMessage(msg *IncomingMessage, newMessages bool) {
	msgDigest := generateMessageDigest(msg.Tag, msg.Data)

	var msgBucket *pmPendingMessageBucket
	var pendingMsg *pmMessage
	var msgFound bool
	// try to find the message. It's more likely to be found in the most recent bucket, so start there and go backward.
	// 메시지를 찾으려고 시도합니다. 가장 최근 버킷에서 찾을 가능성이 높으므로 거기에서 시작하여 뒤로 이동합니다.
	for bucketIndex := range pm.pendingMessagesBuckets {
		currentMsgBucket := pm.pendingMessagesBuckets[len(pm.pendingMessagesBuckets)-1-bucketIndex]
		if pendingMsg, msgFound = currentMsgBucket.messages[msgDigest]; msgFound {
			msgBucket = currentMsgBucket
			break
		}
		if msg.Received >= currentMsgBucket.startTime && msg.Received <= currentMsgBucket.endTime {
			msgBucket = currentMsgBucket
		}
	}
	if pendingMsg == nil {
		if newMessages {
			if msgBucket == nil {
				// no bucket was found. create one.
				// 버킷을 찾지 못했습니다. 하나를 만듭니다.
				msgBucket = &pmPendingMessageBucket{
					messages:  make(map[crypto.Digest]*pmMessage),
					startTime: msg.Received - (msg.Received % int64(pmMessageBucketDuration)), // align with pmMessageBucketDuration
				}
				msgBucket.endTime = msgBucket.startTime + int64(pmMessageBucketDuration) - 1
				pm.pendingMessagesBuckets = append(pm.pendingMessagesBuckets, msgBucket)
			}
			// we don't have this one yet, add it.
			// 우리는 아직 이것을 가지고 있지 않습니다. 추가하십시오.
			msgBucket.messages[msgDigest] = &pmMessage{
				peerMsgTime: map[Peer]int64{
					msg.Sender: msg.Received,
				},
				firstPeerTime: msg.Received,
			}
			pm.firstMessageCount[msg.Sender]++
			pm.msgCount++
		}
		return
	}
	// we already seen this digest make sure we're only moving forward in time.
	// This could be caused when we have lock contention.
	// 우리는 이미 이 다이제스트가 시간에 따라 앞으로만 나아가고 있는지 확인하는 것을 보았습니다.
	// 이것은 잠금 경합이 있을 때 발생할 수 있습니다.
	pendingMsg.peerMsgTime[msg.Sender] = msg.Received
	if msg.Received < pendingMsg.firstPeerTime {
		pendingMsg.firstPeerTime = msg.Received
	}

	if len(pendingMsg.peerMsgTime) == len(pm.monitoredConnections) {
		// we've received the same message from all out peers.
		// 모든 피어로부터 동일한 메시지를 받았습니다.
		for peer, msgTime := range pendingMsg.peerMsgTime {
			pm.connectionDelay[peer] += msgTime - pendingMsg.firstPeerTime
		}
		delete(msgBucket.messages, msgDigest)
	}
}
