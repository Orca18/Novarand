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
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/algorand/websocket"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/metrics"
)

const maxMessageLength = 4 * 1024 * 1024 // Currently the biggest message is VB vote bundles. TODO: per message type size limit? // 현재 가장 큰 메시지는 VB 투표 번들입니다. TODO: 메시지 유형당 크기 제한?
const averageMessageLength = 2 * 1024    // Most of the messages are smaller than this size, which makes it into a good base allocation. // 대부분의 메시지는 이 크기보다 작으므로 좋은 기본 할당이 됩니다.

// This parameter controls how many messages from a single peer can be queued up in the global wsNetwork.readBuffer at a time.
// 이 매개변수는 한 번에 전역 wsNetwork.readBuffer에 큐에 대기할 수 있는 단일 피어의 메시지 수를 제어합니다.
// Making this too large will allow a small number of peers to flood the global read buffer and starve messages from other peers.
// 이것을 너무 크게 하면 소수의 피어가 전역 읽기 버퍼를 넘칠 수 있고 다른 피어의 메시지가 고갈됩니다.
const msgsInReadBufferPerPeer = 10

var networkSentBytesTotal = metrics.MakeCounter(metrics.NetworkSentBytesTotal)
var networkSentBytesByTag = metrics.NewTagCounter("algod_network_sent_bytes_{TAG}", "Number of bytes that were sent over the network per message tag") //메시지 태그당 네트워크를 통해 전송된 바이트 수
var networkReceivedBytesTotal = metrics.MakeCounter(metrics.NetworkReceivedBytesTotal)
var networkReceivedBytesByTag = metrics.NewTagCounter("algod_network_received_bytes_{TAG}", "Number of bytes that were received from the network per message tag") //메시지 태그당 네트워크에서 수신된 바이트 수

var networkMessageReceivedTotal = metrics.MakeCounter(metrics.NetworkMessageReceivedTotal)
var networkMessageReceivedByTag = metrics.NewTagCounter("algod_network_message_received_{TAG}", "Number of complete messages that were received from the network per message tag")
var networkMessageSentTotal = metrics.MakeCounter(metrics.NetworkMessageSentTotal)
var networkMessageSentByTag = metrics.NewTagCounter("algod_network_message_sent_{TAG}", "Number of complete messages that were sent to the network per message tag")

var networkConnectionsDroppedTotal = metrics.MakeCounter(metrics.NetworkConnectionsDroppedTotal)
var networkMessageQueueMicrosTotal = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_message_sent_queue_micros_total", Description: "Total microseconds message spent waiting in queue to be sent"})

var duplicateNetworkMessageReceivedTotal = metrics.MakeCounter(metrics.DuplicateNetworkMessageReceivedTotal)
var duplicateNetworkMessageReceivedBytesTotal = metrics.MakeCounter(metrics.DuplicateNetworkMessageReceivedBytesTotal)
var outgoingNetworkMessageFilteredOutTotal = metrics.MakeCounter(metrics.OutgoingNetworkMessageFilteredOutTotal)
var outgoingNetworkMessageFilteredOutBytesTotal = metrics.MakeCounter(metrics.OutgoingNetworkMessageFilteredOutBytesTotal)

// defaultSendMessageTags is the default list of messages which a peer would allow to be sent without receiving any explicit request.
// defaultSendMessageTags는 명시적인 요청을 받지 않고 피어가 보낼 수 있는 기본 메시지 목록입니다.
var defaultSendMessageTags = map[protocol.Tag]bool{
	protocol.AgreementVoteTag:   true,
	protocol.MsgDigestSkipTag:   true,
	protocol.NetPrioResponseTag: true,
	protocol.PingTag:            true,
	protocol.PingReplyTag:       true,
	protocol.ProposalPayloadTag: true,
	protocol.TopicMsgRespTag:    true,
	protocol.MsgOfInterestTag:   true,
	protocol.TxnTag:             true,
	protocol.UniCatchupReqTag:   true,
	protocol.UniEnsBlockReqTag:  true,
	protocol.VoteBundleTag:      true,
}

// interface allows substituting debug implementation for *websocket.Conn
// 인터페이스는 *websocket.Conn을 디버그 구현으로 대체할 수 있습니다.
type wsPeerWebsocketConn interface {
	RemoteAddr() net.Addr
	NextReader() (int, io.Reader, error)
	WriteMessage(int, []byte) error
	WriteControl(int, []byte, time.Time) error
	SetReadLimit(int64)
	CloseWithoutFlush() error
	SetPingHandler(h func(appData string) error)
	SetPongHandler(h func(appData string) error)
}

type sendMessage struct {
	data         []byte
	enqueued     time.Time             // the time at which the message was first generated // 메시지가 처음 생성된 시간
	peerEnqueued time.Time             // the time at which the peer was attempting to enqueue the message // 피어가 메시지를 대기열에 넣으려고 시도한 시간
	msgTags      map[protocol.Tag]bool // when msgTags is specified ( i.e. non-nil ), the send goroutine is to replace the message tag filter with this one. No data would be accompanied to this message.
	// msgTags가 지정되면(즉, non-nil ) 전송 고루틴은 메시지 태그 필터를 이 필터로 교체합니다. 이 메시지에는 데이터가 포함되지 않습니다.
	hash crypto.Digest
	ctx  context.Context
}

// wsPeerCore also works for non-connected peers we want to do HTTP GET from
// wsPeerCore는 HTTP GET을 수행하려는 연결되지 않은 피어에서도 작동합니다.
type wsPeerCore struct {
	net           *WebsocketNetwork
	rootURL       string
	originAddress string // incoming connection remote host // 들어오는 연결 원격 호스트
	client        http.Client
}

type disconnectReason string

const disconnectReasonNone disconnectReason = ""
const disconnectBadData disconnectReason = "BadData"
const disconnectTooSlow disconnectReason = "TooSlow"
const disconnectReadError disconnectReason = "ReadError"
const disconnectWriteError disconnectReason = "WriteError"
const disconnectIdleConn disconnectReason = "IdleConnection"
const disconnectSlowConn disconnectReason = "SlowConnection"
const disconnectLeastPerformingPeer disconnectReason = "LeastPerformingPeer"
const disconnectCliqueResolve disconnectReason = "CliqueResolving"
const disconnectRequestReceived disconnectReason = "DisconnectRequest"
const disconnectStaleWrite disconnectReason = "DisconnectStaleWrite"

// Response is the structure holding the response from the server
// Response는 서버의 응답을 담고 있는 구조체입니다.
type Response struct {
	Topics Topics
}

type sendMessages struct {
	msgs []sendMessage
}

type wsPeer struct {
	// lastPacketTime contains the UnixNano at the last time a successful communication was made with the peer.
	// lastPacketTime에는 피어와 마지막으로 성공적인 통신이 이루어진 UnixNano가 포함됩니다.
	// "successful communication" above refers to either reading from or writing to a connection without receiving any error.
	// 위의 "성공적인 통신"은 오류를 수신하지 않고 연결에서 읽거나 쓰기를 의미합니다.
	// we want this to be a 64-bit aligned for atomics support on 32bit platforms.
	// 우리는 이것이 32비트 플랫폼에서 원자 지원을 위해 64비트로 정렬되기를 원합니다.
	lastPacketTime int64

	// intermittentOutgoingMessageEnqueueTime contains the UnixNano of the message's enqueue time that is currently being written to the peer, or zero if no message is being written.
	// IntermittentOutgoingMessageEnqueueTime은 현재 피어에 작성 중인 메시지의 큐에 넣은 시간의 UnixNano를 포함하거나, 작성 중인 메시지가 없으면 0을 포함합니다.
	intermittentOutgoingMessageEnqueueTime int64

	// Nonce used to uniquely identify requests
	// 요청을 고유하게 식별하는 데 사용되는 Nonce
	requestNonce uint64

	wsPeerCore

	// conn will be *websocket.Conn (except in testing)
	// conn은 *websocket.Conn이 됩니다(테스트 제외).
	conn wsPeerWebsocketConn

	// we started this connection; otherwise it was inbound
	// 이 연결을 시작했습니다. 그렇지 않으면 그것은 인바운드
	outgoing bool

	closing chan struct{}

	sendBufferHighPrio chan sendMessages
	sendBufferBulk     chan sendMessages

	wg sync.WaitGroup

	didSignalClose int32
	didInnerClose  int32

	TelemetryGUID string
	InstanceName  string

	incomingMsgFilter *messageFilter
	outgoingMsgFilter *messageFilter

	processed chan struct{}

	pingLock              deadlock.Mutex
	pingSent              time.Time
	pingData              []byte
	pingInFlight          bool
	lastPingRoundTripTime time.Duration

	// Hint about position in wn.peers.  Definitely valid if the peer is present in wn.peers.
	// wn.peers의 위치에 대한 힌트. 피어가 wn.peers에 있으면 확실히 유효합니다.
	peerIndex int

	// Challenge sent to the peer on an incoming connection
	// 들어오는 연결에서 피어에게 챌린지 전송
	prioChallenge string

	prioAddress basics.Address
	prioWeight  uint64

	// createTime is the time at which the connection was established with the peer.
	// createTime은 피어와 연결이 설정된 시간입니다.
	createTime time.Time

	// peer version ( this is one of the version supported by the current node and listed in SupportedProtocolVersions )
	// 피어 버전(현재 노드에서 지원하고 SupportedProtocolVersions에 나열된 버전 중 하나입니다)
	version string

	// responseChannels used by the client to wait on the response of the request
	// 클라이언트가 요청 응답을 기다리는 데 사용하는 responseChannels
	responseChannels map[uint64]chan *Response

	// responseChannelsMutex guards the operations of responseChannels
	// responseChannelsMutex는 responseChannels의 작업을 보호합니다.
	responseChannelsMutex deadlock.RWMutex

	// sendMessageTag is a map of allowed message to send to a peer.
	// sendMessageTag는 피어에게 보낼 수 있는 메시지의 맵입니다.
	// We don't use any synchronization on this map, and the only gurentee is that it's being accessed only during startup and/or by the sending loop go routine.
	// 우리는 이 맵에서 어떤 동기화도 사용하지 않으며 유일한 보장은 그것이 시작하는 동안 그리고/또는 송신 루프 go 루틴에 의해서만 액세스된다는 것입니다.
	sendMessageTag map[protocol.Tag]bool

	// connMonitor used to measure the relative performance of the connection compared to the other outgoing connections.
	// 다른 나가는 연결과 비교하여 연결의 상대적 성능을 측정하는 데 사용되는 connMonitor.
	// Incoming connections would have this field set to nil.
	// 들어오는 연결은 이 필드를 nil로 설정합니다.
	connMonitor *connectionPerformanceMonitor

	// peerMessageDelay is calculated by the connection monitor; it's the relative avarage per-message delay.
	// 피어 메시지 지연은 연결 모니터에 의해 계산됩니다. 메시지당 상대적 평균 지연입니다.
	peerMessageDelay int64

	// throttledOutgoingConnection determines if this outgoing connection will be throttled bassed on it's performance or not.
	// throttledOutgoingConnection은 이 나가는 연결이 성능에 따라 조절되는지 여부를 결정합니다.
	//Throttled connections are more likely to be short-lived connections.
	//제한된 연결은 수명이 짧은 연결일 가능성이 더 큽니다.
	throttledOutgoingConnection bool

	// clientDataStore is a generic key/value store used to store client-side data entries associated with a particular peer.
	// clientDataStore는 특정 피어와 관련된 클라이언트 측 데이터 항목을 저장하는 데 사용되는 일반 키/값 저장소입니다.
	// Locked by clientDataStoreMu.
	// clientDataStoreMu에 의해 잠겨 있습니다.
	clientDataStore map[string]interface{}

	// clientDataStoreMu synchronizes access to clientDataStore
	// clientDataStoreMu는 clientDataStore에 대한 액세스를 동기화합니다.
	clientDataStoreMu deadlock.Mutex
}

// HTTPPeer is what the opaque Peer might be.
// HTTPPeer는 불투명한 Peer일 수 있습니다.
// If you get an opaque Peer handle from a GossipNode, maybe try a .(HTTPPeer) type assertion on it.
// GossipNode에서 불투명한 피어 핸들을 얻은 경우 .(HTTPPeer) 유형 어설션(:역설,주장)을 시도할 수 있습니다.
type HTTPPeer interface {
	GetAddress() string
	GetHTTPClient() *http.Client
}

// UnicastPeer is another possible interface for the opaque Peer.
// UnicastPeer는 불투명 피어에 대한 또 다른 가능한 인터페이스입니다.
// It is possible that we can only initiate a connection to a peer over websockets.
// 웹 소켓을 통해서만 피어에 대한 연결을 시작할 수 있습니다.
type UnicastPeer interface {
	GetAddress() string
	// Unicast sends the given bytes to this specific peer. Does not wait for message to be sent.
	// 유니캐스트는 주어진 바이트를 이 특정 피어로 보냅니다. 메시지가 전송될 때까지 기다리지 않습니다.
	Unicast(ctx context.Context, data []byte, tag protocol.Tag) error
	// Version returns the matching version from network.SupportedProtocolVersions
	// 버전은 network.SupportedProtocolVersions에서 일치하는 버전을 반환합니다.
	Version() string
	Request(ctx context.Context, tag Tag, topics Topics) (resp *Response, e error)
	Respond(ctx context.Context, reqMsg IncomingMessage, topics Topics) (e error)
}

// Create a wsPeerCore object
// wsPeerCore 객체 생성
func makePeerCore(net *WebsocketNetwork, rootURL string, roundTripper http.RoundTripper, originAddress string) wsPeerCore {
	return wsPeerCore{
		net:           net,
		rootURL:       rootURL,
		originAddress: originAddress,
		client:        http.Client{Transport: roundTripper},
	}
}

// GetAddress returns the root url to use to connect to this peer.
// GetAddress는 이 피어에 연결하는 데 사용할 루트 URL을 반환합니다.
// TODO: should GetAddress be added to Peer interface?
// TODO: GetAddress를 Peer 인터페이스에 추가해야 합니까?
func (wp *wsPeerCore) GetAddress() string {
	return wp.rootURL
}

// GetHTTPClient returns a client for this peer.
// GetHTTPClient는 이 피어에 대한 클라이언트를 반환합니다.
// http.Client will maintain a cache of connections with some keepalive.
// http.Client는 일부 keepalive와 연결 캐시를 유지합니다.
func (wp *wsPeerCore) GetHTTPClient() *http.Client {
	return &wp.client
}

// Version returns the matching version from network.SupportedProtocolVersions
// 버전은 network.SupportedProtocolVersions에서 일치하는 버전을 반환합니다.
func (wp *wsPeer) Version() string {
	return wp.version
}

// 	Unicast sends the given bytes to this specific peer. Does not wait for message to be sent.
// 유니캐스트는 주어진 바이트를 이 특정 피어로 보냅니다. 메시지가 전송될 때까지 기다리지 않습니다.
// (Implements UnicastPeer)
func (wp *wsPeer) Unicast(ctx context.Context, msg []byte, tag protocol.Tag) error {
	var err error

	tbytes := []byte(tag)
	mbytes := make([]byte, len(tbytes)+len(msg))
	copy(mbytes, tbytes)
	copy(mbytes[len(tbytes):], msg)
	var digest crypto.Digest
	if tag != protocol.MsgDigestSkipTag && len(msg) >= messageFilterSize {
		digest = crypto.Hash(mbytes)
	}

	ok := wp.writeNonBlock(ctx, mbytes, false, digest, time.Now())
	if !ok {
		networkBroadcastsDropped.Inc(nil)
		err = fmt.Errorf("wsPeer failed to unicast: %v", wp.GetAddress())
	}

	return err
}

// Respond sends the response of a request message
// 응답은 요청 메시지의 응답을 보냅니다.
func (wp *wsPeer) Respond(ctx context.Context, reqMsg IncomingMessage, responseTopics Topics) (e error) {

	// Get the hash/key of the request message
	// 요청 메시지의 해시/키 가져오기
	requestHash := hashTopics(reqMsg.Data)

	// Add the request hash
	// 요청 해시 추가
	requestHashData := make([]byte, binary.MaxVarintLen64)
	binary.PutUvarint(requestHashData, requestHash)
	responseTopics = append(responseTopics, Topic{key: requestHashKey, data: requestHashData})

	// Serialize the topics
	// 토픽 직렬화
	serializedMsg := responseTopics.MarshallTopics()

	// Send serializedMsg
	// 직렬화된 메시지 보내기
	msg := make([]sendMessage, 1, 1)
	msg[0] = sendMessage{
		data:         append([]byte(protocol.TopicMsgRespTag), serializedMsg...),
		enqueued:     time.Now(),
		peerEnqueued: time.Now(),
		ctx:          context.Background(),
	}

	select {
	case wp.sendBufferBulk <- sendMessages{msgs: msg}:
	case <-wp.closing:
		wp.net.log.Debugf("peer closing %s", wp.conn.RemoteAddr().String())
		return
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// setup values not trivially assigned
// 사소하게 할당되지 않은 설정 값
func (wp *wsPeer) init(config config.Local, sendBufferLength int) {
	wp.net.log.Debugf("wsPeer init outgoing=%v %#v", wp.outgoing, wp.rootURL)
	wp.closing = make(chan struct{})
	wp.sendBufferHighPrio = make(chan sendMessages, sendBufferLength)
	wp.sendBufferBulk = make(chan sendMessages, sendBufferLength)
	atomic.StoreInt64(&wp.lastPacketTime, time.Now().UnixNano())
	wp.responseChannels = make(map[uint64]chan *Response)
	wp.sendMessageTag = defaultSendMessageTags
	wp.clientDataStore = make(map[string]interface{})

	// processed is a channel that messageHandlerThread writes to when it's done with one of our messages, so that we can queue another one onto wp.net.readBuffer.
	// 처리됨은 messageHandlerThread가 우리 메시지 중 하나가 완료되었을 때 쓰는 채널이므로 다른 메시지를 wp.net.readBuffer에 대기시킬 수 있습니다.
	// Prime it with dummy values so that we can write to readBuffer initially.
	// 처음에 readBuffer에 쓸 수 있도록 더미 값으로 프라이밍합니다.
	wp.processed = make(chan struct{}, msgsInReadBufferPerPeer)
	for i := 0; i < msgsInReadBufferPerPeer; i++ {
		wp.processed <- struct{}{}
	}

	if config.EnableOutgoingNetworkMessageFiltering {
		wp.outgoingMsgFilter = makeMessageFilter(config.OutgoingMessageFilterBucketCount, config.OutgoingMessageFilterBucketSize)
	}

	wp.wg.Add(2)
	go wp.readLoop()
	go wp.writeLoop()
}

// returns the originating address of an incoming connection. For outgoing connection this function returns an empty string.
// 들어오는 연결의 원래 주소를 반환합니다. 나가는 연결의 경우 이 함수는 빈 문자열을 반환합니다.
func (wp *wsPeer) OriginAddress() string {
	return wp.originAddress
}

func (wp *wsPeer) reportReadErr(err error) {
	// only report error if we haven't already closed the peer
	// 피어를 아직 닫지 않은 경우에만 오류를 보고합니다.
	if atomic.LoadInt32(&wp.didInnerClose) == 0 {
		_, _, line, _ := runtime.Caller(1)
		wp.net.log.Warnf("peer[%s] line=%d read err: %s", wp.conn.RemoteAddr().String(), line, err)
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "reader err"})
	}
}

func dedupSafeTag(t protocol.Tag) bool {
	// Votes and Transactions are the only thing we're sure it's safe to de-dup on receipt.
	// 투표와 거래는 수신 시 중복 제거가 안전하다고 확신하는 유일한 것입니다.
	return t == protocol.AgreementVoteTag || t == protocol.TxnTag
}

func (wp *wsPeer) readLoop() {
	// the cleanupCloseError sets the default error to disconnectReadError; depending on the exit reason, the error might get changed.
	// cleanupCloseError는 기본 오류를 disconnectReadError로 설정합니다. 종료 사유에 따라 오류가 변경될 수 있습니다.
	cleanupCloseError := disconnectReadError
	defer func() {
		wp.readLoopCleanup(cleanupCloseError)
	}()
	wp.conn.SetReadLimit(maxMessageLength)
	slurper := MakeLimitedReaderSlurper(averageMessageLength, maxMessageLength)
	for {
		msg := IncomingMessage{}
		mtype, reader, err := wp.conn.NextReader()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				switch ce.Code {
				case websocket.CloseNormalClosure, websocket.CloseGoingAway:
					// deliberate close, no error
					// 의도적인 닫기, 오류 없음
					cleanupCloseError = disconnectRequestReceived
					return
				default:
					// fall through to reportReadErr
					// reportReadErr로 넘어갑니다.
				}
			}
			wp.reportReadErr(err)
			return
		}
		if mtype != websocket.BinaryMessage {
			wp.net.log.Errorf("peer sent non websocket-binary message: %#v", mtype)
			networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "protocol"})
			return
		}
		var tag [2]byte
		_, err = io.ReadFull(reader, tag[:])
		if err != nil {
			wp.reportReadErr(err)
			return
		}
		msg.Tag = Tag(string(tag[:]))
		slurper.Reset()
		err = slurper.Read(reader)
		if err != nil {
			wp.reportReadErr(err)
			return
		}

		msg.processing = wp.processed
		msg.Received = time.Now().UnixNano()
		msg.Data = slurper.Bytes()
		msg.Net = wp.net
		atomic.StoreInt64(&wp.lastPacketTime, msg.Received)
		networkReceivedBytesTotal.AddUint64(uint64(len(msg.Data)+2), nil)
		networkMessageReceivedTotal.AddUint64(1, nil)
		networkReceivedBytesByTag.Add(string(tag[:]), uint64(len(msg.Data)+2))
		networkMessageReceivedByTag.Add(string(tag[:]), 1)
		msg.Sender = wp

		// for outgoing connections, we want to notify the connection monitor that we've received a message.
		// 나가는 연결의 경우 연결 모니터에 메시지를 수신했음을 알리고 싶습니다.
		// The connection monitor would update it's statistics accordingly.
		// 연결 모니터는 그에 따라 통계를 업데이트합니다.
		if wp.connMonitor != nil {
			wp.connMonitor.Notify(&msg)
		}

		switch msg.Tag {
		case protocol.MsgOfInterestTag:
			// try to decode the message-of-interest
			// 관심 메시지 디코딩 시도
			if wp.handleMessageOfInterest(msg) {
				return
			}
			continue
		case protocol.TopicMsgRespTag: // Handle Topic message
			topics, err := UnmarshallTopics(msg.Data)
			if err != nil {
				wp.net.log.Warnf("wsPeer readLoop: could not read the message from: %s %s", wp.conn.RemoteAddr().String(), err)
				continue
			}
			requestHash, found := topics.GetValue(requestHashKey)
			if !found {
				wp.net.log.Warnf("wsPeer readLoop: message from %s is missing the %s", wp.conn.RemoteAddr().String(), requestHashKey)
				continue
			}
			hashKey, _ := binary.Uvarint(requestHash)
			channel, found := wp.getAndRemoveResponseChannel(hashKey)
			if !found {
				wp.net.log.Warnf("wsPeer readLoop: received a message response from %s for a stale request", wp.conn.RemoteAddr().String())
				continue
			}

			select {
			case channel <- &Response{Topics: topics}:
				// do nothing. writing was successful.
			default:
				wp.net.log.Warnf("wsPeer readLoop: channel blocked. Could not pass the response to the requester", wp.conn.RemoteAddr().String())
			}
			continue
		case protocol.MsgDigestSkipTag:
			// network maintenance message handled immediately instead of handing off to general handlers
			// 네트워크 유지 관리 메시지를 일반 핸들러에게 넘기지 않고 즉시 처리
			wp.handleFilterMessage(msg)
			continue
		}
		if len(msg.Data) > 0 && wp.incomingMsgFilter != nil && dedupSafeTag(msg.Tag) {
			if wp.incomingMsgFilter.CheckIncomingMessage(msg.Tag, msg.Data, true, true) {
				//wp.net.log.Debugf("dropped incoming duplicate %s(%d)", msg.Tag, len(msg.Data))
				//wp.net.log.Debugf("들어오는 중복 %s(%d) 삭제", msg.Tag, len(msg.Data))
				duplicateNetworkMessageReceivedTotal.Inc(nil)
				duplicateNetworkMessageReceivedBytesTotal.AddUint64(uint64(len(msg.Data)+len(msg.Tag)), nil)
				// drop message, skip adding it to queue
				// 메시지 삭제, 대기열에 추가 건너뛰기
				continue
			}
		}
		//wp.net.log.Debugf("got msg %d bytes from %s", len(msg.Data), wp.conn.RemoteAddr().String())

		// Wait for a previous message from this peer to be processed,
		// 이 피어의 이전 메시지가 처리될 때까지 기다립니다.
		// to achieve fairness in wp.net.readBuffer.
		// wp.net.readBuffer에서 공정성을 달성하기 위해.
		select {
		case <-wp.processed:
		case <-wp.closing:
			wp.net.log.Debugf("peer closing %s", wp.conn.RemoteAddr().String())
			return
		}

		select {
		case wp.net.readBuffer <- msg:
		case <-wp.closing:
			wp.net.log.Debugf("peer closing %s", wp.conn.RemoteAddr().String())
			return
		}
	}
}

func (wp *wsPeer) handleMessageOfInterest(msg IncomingMessage) (shutdown bool) {
	shutdown = false
	// decode the message, and ensure it's a valid message.
	// 메시지를 디코딩하고 유효한 메시지인지 확인합니다.
	msgTagsMap, err := unmarshallMessageOfInterest(msg.Data)
	if err != nil {
		wp.net.log.Warnf("wsPeer handleMessageOfInterest: could not unmarshall message from: %s %v", wp.conn.RemoteAddr().String(), err)
		return
	}
	msgs := make([]sendMessage, 1, 1)
	msgs[0] = sendMessage{
		data:         nil,
		enqueued:     time.Now(),
		peerEnqueued: time.Now(),
		msgTags:      msgTagsMap,
		ctx:          context.Background(),
	}
	sm := sendMessages{msgs: msgs}

	// try to send the message to the send loop. The send loop will store the message locally and would use it.
	// send 루프에 메시지를 보내려고 시도합니다. send 루프는 메시지를 로컬에 저장하고 사용합니다.
	// the rationale here is that this message is rarely sent, and we would benefit from having it being lock-free.
	// 여기에서 근거는 이 메시지가 거의 전송되지 않으며 잠금이 없는 상태에서 이점이 있다는 것입니다.
	select {
	case wp.sendBufferHighPrio <- sm:
		return
	case <-wp.closing:
		wp.net.log.Debugf("peer closing %s", wp.conn.RemoteAddr().String())
		shutdown = true
	default:
	}

	select {
	case wp.sendBufferHighPrio <- sm:
	case wp.sendBufferBulk <- sm:
	case <-wp.closing:
		wp.net.log.Debugf("peer closing %s", wp.conn.RemoteAddr().String())
		shutdown = true
	}
	return
}

func (wp *wsPeer) readLoopCleanup(reason disconnectReason) {
	wp.internalClose(reason)
	wp.wg.Done()
}

// a peer is telling us not to send messages with some hash
// 피어가 해시가 포함된 메시지를 보내지 말라고 우리에게 말하고 있습니다.
func (wp *wsPeer) handleFilterMessage(msg IncomingMessage) {
	if wp.outgoingMsgFilter == nil {
		return
	}
	if len(msg.Data) != crypto.DigestSize {
		wp.net.log.Warnf("bad filter message size %d", len(msg.Data))
		return
	}
	var digest crypto.Digest
	copy(digest[:], msg.Data)
	//wp.net.log.Debugf("add filter %v", digest)
	wp.outgoingMsgFilter.CheckDigest(digest, true, true)
}

func (wp *wsPeer) writeLoopSend(msgs sendMessages) disconnectReason {
	for _, msg := range msgs.msgs {
		select {
		case <-msg.ctx.Done():
			//logging.Base().Infof("cancelled large send, msg %v out of %v", i, len(msgs.msgs))
			return disconnectReasonNone
		default:
		}

		if err := wp.writeLoopSendMsg(msg); err != disconnectReasonNone {
			return err
		}
	}

	return disconnectReasonNone
}

func (wp *wsPeer) writeLoopSendMsg(msg sendMessage) disconnectReason {
	if len(msg.data) > maxMessageLength {
		wp.net.log.Errorf("trying to send a message longer than we would receive: %d > %d tag=%s", len(msg.data), maxMessageLength, string(msg.data[0:2]))
		// just drop it, don't break the connection
		// 그냥 드롭하고 연결을 끊지 마십시오.
		return disconnectReasonNone
	}
	if msg.msgTags != nil {
		// when msg.msgTags is non-nil, the read loop has received a message-of-interest message that we want to apply.
		// msg.msgTags가 nil이 아니면 읽기 루프가 적용하려는 관심 메시지를 수신했습니다.
		// in order to avoid any locking, it sent it to this queue so that we could set it as the new outgoing message tag filter.
		// 잠금을 방지하기 위해 이 대기열로 보내어 새로운 발신 메시지 태그 필터로 설정할 수 있습니다.
		wp.sendMessageTag = msg.msgTags
		return disconnectReasonNone
	}
	// the tags are always 2 char long; note that this is safe since it's only being used for messages that we have generated locally.
	// 태그는 항상 2자입니다. 이것은 우리가 로컬에서 생성한 메시지에만 사용되기 때문에 안전합니다.
	tag := protocol.Tag(msg.data[:2])
	if !wp.sendMessageTag[tag] {
		// the peer isn't interested in this message.
		return disconnectReasonNone
	}

	// check if this message was waiting in the queue for too long. If this is the case, return "true" to indicate that we want to close the connection.
	// 이 메시지가 대기열에서 너무 오래 대기하고 있는지 확인합니다. 이 경우 "true"를 반환하여 연결을 닫고자 함을 나타냅니다.
	now := time.Now()
	msgWaitDuration := now.Sub(msg.enqueued)
	if msgWaitDuration > maxMessageQueueDuration {
		wp.net.log.Warnf("peer stale enqueued message %dms", msgWaitDuration.Nanoseconds()/1000000)
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "stale message"})
		return disconnectStaleWrite
	}

	atomic.StoreInt64(&wp.intermittentOutgoingMessageEnqueueTime, msg.enqueued.UnixNano())
	defer atomic.StoreInt64(&wp.intermittentOutgoingMessageEnqueueTime, 0)
	err := wp.conn.WriteMessage(websocket.BinaryMessage, msg.data)
	if err != nil {
		if atomic.LoadInt32(&wp.didInnerClose) == 0 {
			wp.net.log.Warn("peer write error ", err)
			networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "write err"})
		}
		return disconnectWriteError
	}
	atomic.StoreInt64(&wp.lastPacketTime, time.Now().UnixNano())
	networkSentBytesTotal.AddUint64(uint64(len(msg.data)), nil)
	networkSentBytesByTag.Add(string(tag), uint64(len(msg.data)))
	networkMessageSentTotal.AddUint64(1, nil)
	networkMessageSentByTag.Add(string(tag), 1)
	networkMessageQueueMicrosTotal.AddUint64(uint64(time.Now().Sub(msg.peerEnqueued).Nanoseconds()/1000), nil)
	return disconnectReasonNone
}

func (wp *wsPeer) writeLoop() {
	// the cleanupCloseError sets the default error to disconnectWriteError; depending on the exit reason, the error might get changed.
	// cleanupCloseError는 기본 오류를 disconnectWriteError로 설정합니다. 종료 사유에 따라 오류가 변경될 수 있습니다.
	cleanupCloseError := disconnectWriteError
	defer func() {
		wp.writeLoopCleanup(cleanupCloseError)
	}()
	for {
		// send from high prio channel as long as we can
		// 가능한 한 높은 우선순위 채널에서 전송
		select {
		case data := <-wp.sendBufferHighPrio:
			if writeErr := wp.writeLoopSend(data); writeErr != disconnectReasonNone {
				cleanupCloseError = writeErr
				return
			}
			continue
		default:
		}
		// if nothing high prio, send anything
		// 우선순위가 없으면 아무 것도 보내십시오.
		select {
		case <-wp.closing:
			return
		case data := <-wp.sendBufferHighPrio:
			if writeErr := wp.writeLoopSend(data); writeErr != disconnectReasonNone {
				cleanupCloseError = writeErr
				return
			}
		case data := <-wp.sendBufferBulk:
			if writeErr := wp.writeLoopSend(data); writeErr != disconnectReasonNone {
				cleanupCloseError = writeErr
				return
			}
		}
	}
}
func (wp *wsPeer) writeLoopCleanup(reason disconnectReason) {
	wp.internalClose(reason)
	wp.wg.Done()
}

func (wp *wsPeer) writeNonBlock(ctx context.Context, data []byte, highPrio bool, digest crypto.Digest, msgEnqueueTime time.Time) bool {
	msgs := make([][]byte, 1, 1)
	digests := make([]crypto.Digest, 1, 1)
	msgs[0] = data
	digests[0] = digest
	return wp.writeNonBlockMsgs(ctx, msgs, highPrio, digests, msgEnqueueTime)
}

// return true if enqueued/sent
// 대기열에 있거나 전송되면 true를 반환합니다.
func (wp *wsPeer) writeNonBlockMsgs(ctx context.Context, data [][]byte, highPrio bool, digest []crypto.Digest, msgEnqueueTime time.Time) bool {
	includeIndices := make([]int, 0, len(data))
	for i := range data {
		if wp.outgoingMsgFilter != nil && len(data[i]) > messageFilterSize && wp.outgoingMsgFilter.CheckDigest(digest[i], false, false) {
			//wp.net.log.Debugf("msg drop as outbound dup %s(%d) %v", string(data[:2]), len(data)-2, digest)
			// peer has notified us it doesn't need this message
			// 피어가 이 메시지가 필요하지 않다고 알려왔습니다.
			outgoingNetworkMessageFilteredOutTotal.Inc(nil)
			outgoingNetworkMessageFilteredOutBytesTotal.AddUint64(uint64(len(data)), nil)
		} else {
			includeIndices = append(includeIndices, i)
		}
	}
	if len(includeIndices) == 0 {
		// returning true because it is as good as sent, the peer already has it.
		return true
	}

	var outchan chan sendMessages

	msgs := make([]sendMessage, 0, len(includeIndices))
	enqueueTime := time.Now()
	for _, index := range includeIndices {
		msgs = append(msgs, sendMessage{data: data[index], enqueued: msgEnqueueTime, peerEnqueued: enqueueTime, hash: digest[index], ctx: ctx})
	}

	if highPrio {
		outchan = wp.sendBufferHighPrio
	} else {
		outchan = wp.sendBufferBulk
	}
	select {
	case outchan <- sendMessages{msgs: msgs}:
		return true
	default:
	}
	return false
}

const pingLength = 8
const maxPingWait = 60 * time.Second

// sendPing sends a ping block to the peer.
// sendPing은 피어에게 ping 블록을 보냅니다.
// return true if either a ping request was enqueued or there is already ping request in flight in the past maxPingWait time.
// ping 요청이 대기열에 있거나 지난 maxPingWait 시간에 이미 ping 요청이 진행 중인 경우 true를 반환합니다.
func (wp *wsPeer) sendPing() bool {
	wp.pingLock.Lock()
	defer wp.pingLock.Unlock()
	now := time.Now()
	if wp.pingInFlight && (now.Sub(wp.pingSent) < maxPingWait) {
		return true
	}

	tagBytes := []byte(protocol.PingTag)
	mbytes := make([]byte, len(tagBytes)+pingLength)
	copy(mbytes, tagBytes)
	crypto.RandBytes(mbytes[len(tagBytes):])
	wp.pingData = mbytes[len(tagBytes):]
	sent := wp.writeNonBlock(context.Background(), mbytes, false, crypto.Digest{}, time.Now())

	if sent {
		wp.pingInFlight = true
		wp.pingSent = now
	}
	return sent
}

// get some times out of the peer while observing the ping data lock
// ping 데이터 잠금을 관찰하는 동안 피어에서 시간을 얻습니다.
func (wp *wsPeer) pingTimes() (lastPingSent time.Time, lastPingRoundTripTime time.Duration) {
	wp.pingLock.Lock()
	defer wp.pingLock.Unlock()
	lastPingSent = wp.pingSent
	lastPingRoundTripTime = wp.lastPingRoundTripTime
	return
}

// called when the connection had an error or closed remotely
// 연결에 오류가 있거나 원격으로 닫힐 때 호출됩니다.
func (wp *wsPeer) internalClose(reason disconnectReason) {
	if atomic.CompareAndSwapInt32(&wp.didSignalClose, 0, 1) {
		wp.net.peerRemoteClose(wp, reason)
	}
	wp.Close(time.Now().Add(peerDisconnectionAckDuration))
}

// called either here or from above enclosing node logic
// 여기 또는 엔클로징 노드 로직 위에서 호출됨
func (wp *wsPeer) Close(deadline time.Time) {
	atomic.StoreInt32(&wp.didSignalClose, 1)
	if atomic.CompareAndSwapInt32(&wp.didInnerClose, 0, 1) {
		close(wp.closing)
		err := wp.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
		if err != nil {
			wp.net.log.Infof("failed to write CloseMessage to connection for %s", wp.conn.RemoteAddr().String())
		}
		err = wp.conn.CloseWithoutFlush()
		if err != nil {
			wp.net.log.Infof("failed to CloseWithoutFlush to connection for %s", wp.conn.RemoteAddr().String())
		}
	}
}

// CloseAndWait internally calls Close() then waits for all peer activity to stop
// CloseAndWait는 내부적으로 Close()를 호출한 다음 모든 피어 활동이 중지될 때까지 기다립니다.
func (wp *wsPeer) CloseAndWait(deadline time.Time) {
	wp.Close(deadline)
	wp.wg.Wait()
}

func (wp *wsPeer) GetLastPacketTime() int64 {
	return atomic.LoadInt64(&wp.lastPacketTime)
}

func (wp *wsPeer) CheckSlowWritingPeer(now time.Time) bool {
	ongoingMessageTime := atomic.LoadInt64(&wp.intermittentOutgoingMessageEnqueueTime)
	if ongoingMessageTime == 0 {
		return false
	}
	timeSinceMessageCreated := now.Sub(time.Unix(0, ongoingMessageTime))
	return timeSinceMessageCreated > maxMessageQueueDuration
}

// getRequestNonce returns the byte representation of ever increasing uint64
// getRequestNonce는 계속 증가하는 uint64의 바이트 표현을 반환합니다.
// The value is stored on wsPeer
// 값은 wsPeer에 저장됩니다.
func (wp *wsPeer) getRequestNonce() []byte {
	buf := make([]byte, binary.MaxVarintLen64)
	binary.PutUvarint(buf, atomic.AddUint64(&wp.requestNonce, 1))
	return buf
}

// Request submits the request to the server, waits for a response
// 요청은 서버에 요청을 제출하고 응답을 기다립니다.
func (wp *wsPeer) Request(ctx context.Context, tag Tag, topics Topics) (resp *Response, e error) {

	// Add nonce as a topic
	// 논스를 주제로 추가
	nonce := wp.getRequestNonce()
	topics = append(topics, Topic{key: "nonce", data: nonce})

	// serialize the topics
	// 토픽 직렬화
	serializedMsg := topics.MarshallTopics()

	// Get the topics' hash
	// 주제의 해시를 가져옵니다.
	hash := hashTopics(serializedMsg)

	// Make a response channel to wait on the server response
	// 서버 응답을 기다리는 응답 채널을 만듭니다.
	responseChannel := wp.makeResponseChannel(hash)
	defer wp.getAndRemoveResponseChannel(hash)

	// Send serializedMsg
	// 직렬화된 메시지 보내기
	msg := make([]sendMessage, 1, 1)
	msg[0] = sendMessage{
		data:         append([]byte(tag), serializedMsg...),
		enqueued:     time.Now(),
		peerEnqueued: time.Now(),
		ctx:          context.Background()}
	select {
	case wp.sendBufferBulk <- sendMessages{msgs: msg}:
	case <-wp.closing:
		e = fmt.Errorf("peer closing %s", wp.conn.RemoteAddr().String())
		return
	case <-ctx.Done():
		return resp, ctx.Err()
	}

	// wait for the channel.
	select {
	case resp = <-responseChannel:
		return resp, nil
	case <-wp.closing:
		e = fmt.Errorf("peer closing %s", wp.conn.RemoteAddr().String())
		return
	case <-ctx.Done():
		return resp, ctx.Err()
	}
}

func (wp *wsPeer) makeResponseChannel(key uint64) (responseChannel chan *Response) {
	newChan := make(chan *Response, 1)
	wp.responseChannelsMutex.Lock()
	defer wp.responseChannelsMutex.Unlock()
	wp.responseChannels[key] = newChan
	return newChan
}

// getAndRemoveResponseChannel returns the channel and deletes the channel from the map
// getAndRemoveResponseChannel은 채널을 반환하고 맵에서 채널을 삭제합니다.
func (wp *wsPeer) getAndRemoveResponseChannel(key uint64) (respChan chan *Response, found bool) {
	wp.responseChannelsMutex.Lock()
	defer wp.responseChannelsMutex.Unlock()
	respChan, found = wp.responseChannels[key]
	delete(wp.responseChannels, key)
	return
}

func (wp *wsPeer) getPeerData(key string) interface{} {
	wp.clientDataStoreMu.Lock()
	defer wp.clientDataStoreMu.Unlock()
	return wp.clientDataStore[key]
}

func (wp *wsPeer) setPeerData(key string, value interface{}) {
	wp.clientDataStoreMu.Lock()
	defer wp.clientDataStoreMu.Unlock()
	if value == nil {
		delete(wp.clientDataStore, key)
	} else {
		wp.clientDataStore[key] = value
	}
}
