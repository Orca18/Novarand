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
	"container/heap"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/algorand/websocket"
	"github.com/gorilla/mux"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
	"github.com/Orca18/novarand/network/limitlistener"
	"github.com/Orca18/novarand/protocol"
	tools_network "github.com/Orca18/novarand/tools/network"
	"github.com/Orca18/novarand/tools/network/dnssec"
	"github.com/Orca18/novarand/util/metrics"
)

const incomingThreads = 20
const messageFilterSize = 5000 // messages greater than that size may be blocked by incoming/outgoing filter
// 해당 크기보다 큰 메시지는 수신/발신 필터에 의해 차단될 수 있습니다.

// httpServerReadHeaderTimeout is the amount of time allowed to read request headers.
// httpServerReadHeaderTimeout은 요청 헤더를 읽을 수 있는 시간입니다.
// The connection's read deadline is reset after reading the headers and the Handler can decide what is considered too slow for the body.
// 연결의 읽기 기한은 헤더를 읽은 후 재설정되며 핸들러는 본문에 대해 너무 느린 것으로 간주되는 것을 결정할 수 있습니다.
const httpServerReadHeaderTimeout = time.Second * 10

// httpServerWriteTimeout is the maximum duration before timing out writes of the response
// httpServerWriteTimeout은 응답 쓰기 시간이 초과되기 전의 최대 지속 시간입니다.
// It is reset whenever a new request's header is read.
// 새 요청의 헤더를 읽을 때마다 재설정됩니다.
const httpServerWriteTimeout = time.Second * 60

// httpServerIdleTimeout is the maximum amount of time to wait for the next request when keep-alives are enabled.
// httpServerIdleTimeout은 연결 유지가 활성화되었을 때 다음 요청을 기다리는 최대 시간입니다.
// If httpServerIdleTimeout is zero, the value of ReadTimeout is used.
// httpServerIdleTimeout이 0이면 ReadTimeout 값을 사용한다.
// If both are zero, ReadHeaderTimeout is used.
// 둘 다 0이면 ReadHeaderTimeout이 사용됩니다.
const httpServerIdleTimeout = time.Second * 4

// MaxHeaderBytes controls the maximum number of bytes the server will read parsing the request header's keys and values, including the request line.
// MaxHeaderBytes는 요청 행을 포함하여 요청 헤더의 키와 값을 구문 분석하여 서버가 읽을 최대 바이트 수를 제어합니다.
// It does not limit the size of the request body.
// 요청 본문의 크기를 제한하지 않습니다.
const httpServerMaxHeaderBytes = 4096

// connectionActivityMonitorInterval is the interval at which we check if any of the connected peers have been idle for a long while and need to be disconnected.
// connectionActivityMonitorInterval은 연결된 피어가 오랫동안 유휴 상태였으며 연결을 끊을 필요가 있는지 확인하는 간격입니다.
const connectionActivityMonitorInterval = 3 * time.Minute

// maxPeerInactivityDuration is the maximum allowed duration for a peer to remain completly idle (i.e. no inbound or outbound communication), before we discard the connection.
// maxPeerInactivityDuration은 연결을 버리기 전에 피어가 완전히 유휴 상태(즉, 인바운드 또는 아웃바운드 통신 없음)를 유지하는 데 허용되는 최대 지속 시간입니다.
const maxPeerInactivityDuration = 5 * time.Minute

// maxMessageQueueDuration is the maximum amount of time a message is allowed to be waiting in the various queues before being sent.
// maxMessageQueueDuration은 메시지가 전송되기 전에 다양한 대기열에서 대기할 수 있는 최대 시간입니다.
// Once that deadline has reached, sending the message is pointless, as it's too stale to be of any value
// 해당 기한에 도달하면 메시지를 보내는 것은 의미가 없습니다. 너무 오래되어 가치가 없기 때문입니다.
const maxMessageQueueDuration = 25 * time.Second

// slowWritingPeerMonitorInterval is the interval at which we peek on the connected peers to verify that their current outgoing message is not being blocked for too long.
// slowWritingPeerMonitorInterval은 현재 나가는 메시지가 너무 오랫동안 차단되지 않았는지 확인하기 위해 연결된 피어를 엿보는 간격입니다.
const slowWritingPeerMonitorInterval = 5 * time.Second

// unprintableCharacterGlyph is used to replace any non-ascii character when logging incoming network string directly to the log file.
// unprintableCharacterGlyph는 들어오는 네트워크 문자열을 로그 파일에 직접 기록할 때 ASCII가 아닌 문자를 대체하는 데 사용됩니다.
//  Note that the log file itself would also json-encode these before placing them in the log file.
// 로그 파일 자체는 로그 파일에 저장하기 전에 이를 json으로 인코딩합니다.
const unprintableCharacterGlyph = "▯"

var networkIncomingConnections = metrics.MakeGauge(metrics.NetworkIncomingConnections)
var networkOutgoingConnections = metrics.MakeGauge(metrics.NetworkOutgoingConnections)

var networkIncomingBufferMicros = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_rx_buffer_micros_total", Description: "microseconds spent by incoming messages on the receive buffer"})
var networkHandleMicros = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_rx_handle_micros_total", Description: "microseconds spent by protocol handlers in the receive thread"})

var networkBroadcasts = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_broadcasts_total", Description: "number of broadcast operations"})
var networkBroadcastQueueMicros = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_broadcast_queue_micros_total", Description: "microseconds broadcast requests sit on queue"})
var networkBroadcastSendMicros = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_broadcast_send_micros_total", Description: "microseconds spent broadcasting"})
var networkBroadcastsDropped = metrics.MakeCounter(metrics.MetricName{Name: "algod_broadcasts_dropped_total", Description: "number of broadcast messages not sent to any peer"})
var networkPeerBroadcastDropped = metrics.MakeCounter(metrics.MetricName{Name: "algod_peer_broadcast_dropped_total", Description: "number of broadcast messages not sent to some peer"})

var networkSlowPeerDrops = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_slow_drops_total", Description: "number of peers dropped for being slow to send to"})
var networkIdlePeerDrops = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_idle_drops_total", Description: "number of peers dropped due to idle connection"})
var networkBroadcastQueueFull = metrics.MakeCounter(metrics.MetricName{Name: "algod_network_broadcast_queue_full_total", Description: "number of messages that were drops due to full broadcast queue"})

var minPing = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_peer_min_ping_seconds", Description: "Network round trip time to fastest peer in seconds."})
var meanPing = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_peer_mean_ping_seconds", Description: "Network round trip time to average peer in seconds."})
var medianPing = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_peer_median_ping_seconds", Description: "Network round trip time to median peer in seconds."})
var maxPing = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_peer_max_ping_seconds", Description: "Network round trip time to slowest peer in seconds."})

var peers = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_peers", Description: "Number of active peers."})
var incomingPeers = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_incoming_peers", Description: "Number of active incoming peers."})
var outgoingPeers = metrics.MakeGauge(metrics.MetricName{Name: "algod_network_outgoing_peers", Description: "Number of active outgoing peers."})

// peerDisconnectionAckDuration defines the time we would wait for the peer disconnection to compelete.
// peerDisconnectionAckDuration은 피어 연결 해제가 완료될 때까지 기다리는 시간을 정의합니다.
const peerDisconnectionAckDuration time.Duration = 5 * time.Second

// peerShutdownDisconnectionAckDuration defines the time we would wait for the peer disconnection to compelete during shutdown.
// peerShutdownDisconnectionAckDuration은 종료 중에 피어 연결 해제가 완료될 때까지 기다리는 시간을 정의합니다.
const peerShutdownDisconnectionAckDuration time.Duration = 50 * time.Millisecond

// Peer opaque interface for referring to a neighbor in the network
// 네트워크에서 이웃을 참조하기 위한 피어 불투명 인터페이스
type Peer interface{}

// PeerOption allows users to specify a subset of peers to query
// PeerOption을 사용하면 사용자가 쿼리할 피어의 하위 집합을 지정할 수 있습니다.
type PeerOption int

const (
	// PeersConnectedOut specifies all peers with outgoing connections
	// PeersConnectedOut은 나가는 연결이 있는 모든 피어를 지정합니다.
	PeersConnectedOut PeerOption = iota
	// PeersConnectedIn specifies all peers with inbound connections
	// PeersConnectedIn은 인바운드 연결이 있는 모든 피어를 지정합니다.
	PeersConnectedIn PeerOption = iota
	// PeersPhonebookRelays specifies all relays in the phonebook
	// PeersPhonebookRelays는 전화번호부의 모든 릴레이를 지정합니다.
	PeersPhonebookRelays PeerOption = iota
	// PeersPhonebookArchivers specifies all archivers in the phonebook
	// PeersPhonebookArchivers는 전화번호부의 모든 아카이버를 지정합니다.
	PeersPhonebookArchivers PeerOption = iota
)

// GossipNode represents a node in the gossip network
// GossipNode는 가십 네트워크의 노드를 나타냅니다.
type GossipNode interface {
	Address() (string, bool)
	Broadcast(ctx context.Context, tag protocol.Tag, data []byte, wait bool, except Peer) error
	BroadcastArray(ctx context.Context, tag []protocol.Tag, data [][]byte, wait bool, except Peer) error
	Relay(ctx context.Context, tag protocol.Tag, data []byte, wait bool, except Peer) error
	RelayArray(ctx context.Context, tag []protocol.Tag, data [][]byte, wait bool, except Peer) error
	Disconnect(badnode Peer)
	DisconnectPeers()
	Ready() chan struct{}

	// RegisterHTTPHandler path accepts gorilla/mux path annotations
	// RegisterHTTPHandler 경로는 gorilla/mux 경로 주석을 허용합니다.
	RegisterHTTPHandler(path string, handler http.Handler)

	// RequestConnectOutgoing asks the system to actually connect to peers.
	// RequestConnectOutgoing은 시스템에 실제로 피어에 연결하도록 요청합니다.
	// `replace` optionally drops existing connections before making new ones.
	// `replace`는 새 연결을 만들기 전에 선택적으로 기존 연결을 삭제합니다.
	// `quit` chan allows cancellation. TODO: use `context`
	// `quit` chan은 취소를 허용합니다. TODO: `컨텍스트 사용
	RequestConnectOutgoing(replace bool, quit <-chan struct{})

	// Get a list of Peers we could potentially send a direct message to.
	// 잠재적으로 다이렉트 메시지를 보낼 수 있는 피어 목록을 가져옵니다.
	GetPeers(options ...PeerOption) []Peer

	// Start threads, listen on sockets.
	// 스레드를 시작하고 소켓에서 수신 대기합니다.
	Start()

	// Close sockets. Stop threads.
	// 소켓을 닫습니다. 스레드를 중지합니다.
	Stop()

	// RegisterHandlers adds to the set of given message handlers.
	// RegisterHandlers는 주어진 메시지 핸들러 세트에 추가합니다.
	RegisterHandlers(dispatch []TaggedMessageHandler)

	// ClearHandlers deregisters all the existing message handlers.
	// ClearHandlers는 기존의 모든 메시지 핸들러를 등록 취소합니다.
	ClearHandlers()

	// GetRoundTripper returns a Transport that would limit the number of outgoing connections.
	// GetRoundTripper는 나가는 연결 수를 제한하는 Transport를 반환합니다.
	GetRoundTripper() http.RoundTripper

	// OnNetworkAdvance notifies the network library that the agreement protocol was able to make a notable progress.
	// this is the only indication that we have that we haven't formed a clique, where all incoming messages
	// arrive very quickly, but might be missing some votes.
	// OnNetworkAdvance는 합의 프로토콜이 주목할 만한 진전을 이룰 수 있었음을 네트워크 라이브러리에 알립니다.
	// 이것은 우리가 파벌을 형성하지 않았다는 유일한 표시입니다.
	// 매우 빨리 도착하지만 일부 투표가 누락될 수 있습니다.
	// The usage of this call is expected to have similar characteristics as with a watchdog timer.
	// 이 호출의 사용은 워치독 타이머와 유사한 특성을 가질 것으로 예상됩니다.
	OnNetworkAdvance()

	// GetHTTPRequestConnection returns the underlying connection for the given request.
	// GetHTTPRequestConnection은 주어진 요청에 대한 기본 연결을 반환합니다.
	// Note that the request must be the same request that was provided to the http handler ( or provide a fallback Context() to that )
	// 요청은 http 핸들러에 제공된 것과 동일한 요청이어야 합니다(또는 이에 대한 대체 Context()를 제공해야 함).
	GetHTTPRequestConnection(request *http.Request) (conn net.Conn)

	// RegisterMessageInterest notifies the network library that this node wants to receive messages with the specified tag.
	// RegisterMessageInterest는 이 노드가 지정된 태그가 있는 메시지를 수신하기를 원한다는 것을 네트워크 라이브러리에 알립니다.
	// This will cause this node to send corresponding MsgOfInterest notifications to any newly connecting peers.
	// 이것은 이 노드가 대응하는 MsgOfInterest 알림을 새로 연결하는 피어에게 보내도록 합니다.
	// This should be called before the network is started.
	// 이것은 네트워크가 시작되기 전에 호출되어야 합니다.
	RegisterMessageInterest(protocol.Tag) error

	// SubstituteGenesisID substitutes the "{genesisID}" with their network-specific genesisID.
	// SubstituteGenesisID는 "{genesisID}"를 네트워크별 genesisID로 대체합니다.
	SubstituteGenesisID(rawURL string) string

	// GetPeerData returns a value stored by SetPeerData
	// GetPeerData는 SetPeerData에 의해 저장된 값을 반환합니다.
	GetPeerData(peer Peer, key string) interface{}

	// SetPeerData attaches a piece of data to a peer.
	// SetPeerData는 데이터 조각을 피어에 연결합니다.
	// Other services inside go-algorand may attach data to a peer that gets garbage collected when the peer is closed.
	// go-algorand 내부의 다른 서비스는 피어가 닫힐 때 수집되는 가비지 데이터를 피어에 연결할 수 있습니다.
	SetPeerData(peer Peer, key string, value interface{})
}

// IncomingMessage represents a message arriving from some peer in our p2p network
// IncomingMessage는 p2p 네트워크의 일부 피어에서 도착하는 메시지를 나타냅니다.
type IncomingMessage struct {
	Sender Peer
	Tag    Tag
	Data   []byte
	Err    error
	Net    GossipNode

	// Received is time.Time.UnixNano()
	// 수신은 time.Time.UnixNano()
	Received int64

	// processing is a channel that is used by messageHandlerThread to indicate that it has started processing this message
	// 처리는 이 메시지 처리를 시작했음을 나타내기 위해 messageHandlerThread에서 사용하는 채널입니다.
	// It is used to ensure fairness across peers in terms of processing messages.
	// 메시지 처리 측면에서 피어 간의 공정성을 보장하는 데 사용됩니다.
	processing chan struct{}
}

// Tag is a short string (2 bytes) marking a type of message
// 태그는 메시지 유형을 표시하는 짧은 문자열(2바이트)입니다.
type Tag = protocol.Tag

func highPriorityTag(tags []protocol.Tag) bool {
	for _, tag := range tags {
		if tag == protocol.AgreementVoteTag || tag == protocol.ProposalPayloadTag {
			return true
		}
	}
	return false
}

// OutgoingMessage represents a message we want to send.
// OutgoingMessage는 보내려는 메시지를 나타냅니다.
type OutgoingMessage struct {
	Action  ForwardingPolicy
	Tag     Tag
	Payload []byte
	Topics  Topics
}

// ForwardingPolicy is an enum indicating to whom we should send a message
// ForwardingPolicy는 누구에게 메시지를 보내야 하는지를 나타내는 열거형입니다.
type ForwardingPolicy int

const (
	// Ignore - discard (don't forward)
	// 무시 - 폐기(전달하지 않음)
	Ignore ForwardingPolicy = iota

	// Disconnect - disconnect from the peer that sent this message
	// Disconnect - 이 메시지를 보낸 피어와의 연결을 끊습니다.
	Disconnect

	// Broadcast - forward to everyone (except the sender)
	// 브로드캐스트 - 모든 사람에게 전달(발신자 제외)
	Broadcast

	// Respond - reply to the sender
	// 응답 - 보낸 사람에게 응답
	Respond
)

// ====== 멀티플렉서 ======
// MessageHandler takes a IncomingMessage (e.g., vote, transaction), processes it, and returns what (if anything) to send to the network in response.
// MessageHandler는 IncomingMessage(예: 투표, 트랜잭션)를 받아 처리하고 응답으로 네트워크에 보낼 내용(있는 경우)을 반환합니다.
// The ForwardingPolicy field of the returned OutgoingMessage indicates whether to reply directly to the sender (unicast), propagate to everyone except the sender (broadcast), or do nothing (ignore).
// 반환된 OutgoingMessage의 ForwardingPolicy 필드는 보낸 사람에게 직접 회신할지(유니캐스트), 보낸 사람을 제외한 모든 사람에게 전파할지(브로드캐스트), 아무 작업도 수행하지 않을(무시) 여부를 나타냅니다.
type MessageHandler interface {
	Handle(message IncomingMessage) OutgoingMessage
}

// HandlerFunc represents an implemenation of the MessageHandler interface
// HandlerFunc는 MessageHandler 인터페이스의 구현을 나타냅니다.
type HandlerFunc func(message IncomingMessage) OutgoingMessage

// Handle implements MessageHandler.Handle, calling the handler with the IncomingKessage and returning the OutgoingMessage
// 핸들은 MessageHandler.Handle을 구현하여 IncomingKessage로 핸들러를 호출하고 OutgoingMessage를 반환합니다.
func (f HandlerFunc) Handle(message IncomingMessage) OutgoingMessage {
	return f(message)
}

// TaggedMessageHandler receives one type of broadcast messages
// TaggedMessageHandler는 한 가지 유형의 브로드캐스트 메시지를 수신합니다.
type TaggedMessageHandler struct {
	Tag
	MessageHandler
}

// ======   ======

// Propagate is a convenience function to save typing in the common case of a message handler telling us to propagate an incoming message "return network.Propagate(msg)" instead of "return network.OutgoingMsg{network.Broadcast, msg.Tag, msg.Data}"
// Propagate는 "return network.OutgoingMsg{network.Broadcast, msg.Tag" 대신 "return network.Propagate(msg)"가 들어오는 메시지를 전파하도록 지시하는 메시지 처리기의 일반적인 경우 입력을 저장하는 편리한 기능입니다. msg.Data}"
func Propagate(msg IncomingMessage) OutgoingMessage {
	return OutgoingMessage{Broadcast, msg.Tag, msg.Data, nil}
}

// GossipNetworkPath is the URL path to connect to the websocket gossip node at.
// GossipNetworkPath는 websocket gossip 노드에 연결할 URL 경로입니다.
// Contains {genesisID} param to be handled by gorilla/mux
// gorilla/mux가 처리할 {genesisID} 매개변수를 포함합니다.
const GossipNetworkPath = "/v1/{genesisID}/gossip"

// WebsocketNetwork implements GossipNode
type WebsocketNetwork struct {
	listener net.Listener
	server   http.Server
	router   *mux.Router
	scheme   string // are we serving http or https ?
	// 우리는 http 또는 https를 제공하고 있습니까?

	upgrader websocket.Upgrader

	config config.Local

	log logging.Logger

	readBuffer chan IncomingMessage

	wg sync.WaitGroup

	handlers Multiplexer

	ctx       context.Context
	ctxCancel context.CancelFunc

	peersLock          deadlock.RWMutex
	peers              []*wsPeer
	peersChangeCounter int32 // peersChangeCounter is an atomic variable that increases on each change to the peers.
	// peersChangeCounter는 피어가 변경될 때마다 증가하는 원자 변수입니다.
	// It helps avoiding taking the peersLock when checking if the peers list was modified.
	// 피어 목록이 수정되었는지 확인할 때 peersLock을 사용하는 것을 방지하는 데 도움이 됩니다.

	broadcastQueueHighPrio chan broadcastRequest
	broadcastQueueBulk     chan broadcastRequest

	phonebook Phonebook

	GenesisID string
	NetworkID protocol.NetworkID
	RandomID  string

	ready     int32
	readyChan chan struct{}

	meshUpdateRequests chan meshRequest

	// Keep a record of pending outgoing connections so we don't start duplicates connection attempts.
	// 중복 연결 시도를 시작하지 않도록 보류 중인 발신 연결 기록을 유지합니다.
	// Needs to be locked because it's accessed from the meshThread and also threads started to run tryConnect()
	// meshThread에서 액세스하고 스레드가 tryConnect()를 실행하기 시작했기 때문에 잠겨 있어야 합니다.
	tryConnectAddrs map[string]int64
	tryConnectLock  deadlock.Mutex

	incomingMsgFilter *messageFilter // message filter to remove duplicate incoming messages from different peers
	// 다른 피어로부터 중복되는 수신 메시지를 제거하기 위한 메시지 필터

	eventualReadyDelay time.Duration

	relayMessages bool // True if we should relay messages from other nodes (nominally true for relays, false otherwise)
	// 다른 노드의 메시지를 릴레이해야 하는 경우 true(릴레이의 경우 일반적으로 true, 그렇지 않은 경우 false)

	prioScheme       NetPrioScheme
	prioTracker      *prioTracker
	prioResponseChan chan *wsPeer

	// outgoingMessagesBufferSize is the size used for outgoing messages.
	// outgoingMessage 버퍼 크기는 보내는 메시지에 사용되는 크기입니다.
	outgoingMessagesBufferSize int

	// slowWritingPeerMonitorInterval defines the interval between two consecutive tests for slow peer writing
	// slowWritingPeerMonitorInterval은 느린 피어 쓰기에 대한 두 개의 연속 테스트 사이의 간격을 정의합니다.
	slowWritingPeerMonitorInterval time.Duration

	requestsTracker *RequestTracker
	requestsLogger  *RequestLogger

	// lastPeerConnectionsSent is the last time the peer connections were sent ( or attempted to be sent ) to the telemetry server.
	// lastPeerConnectionsSent는 피어 연결이 원격 측정 서버로 전송된(또는 전송을 시도한) 마지막 시간입니다.
	lastPeerConnectionsSent time.Time

	// connPerfMonitor is used on outgoing connections to measure their relative message timing
	// connPerfMonitor는 상대 메시지 타이밍을 측정하기 위해 나가는 연결에 사용됩니다.
	connPerfMonitor *connectionPerformanceMonitor

	// lastNetworkAdvanceMu syncronized the access to lastNetworkAdvance
	// lastNetworkAdvanceMu 는 lastNetworkAdvance 에 대한 액세스를 동기화합니다.
	lastNetworkAdvanceMu deadlock.Mutex

	// lastNetworkAdvance contains the last timestamp where the agreement protocol was able to make a notable progress.
	// lastNetworkAdvance에는 계약 프로토콜이 주목할 만한 진전을 이룰 수 있었던 마지막 타임스탬프가 포함됩니다.
	// it used as a watchdog to help us detect connectivity issues ( such as cliques )
	// 연결 문제(예: 파벌)를 감지하는 데 도움이 되는 감시 장치로 사용되었습니다.
	lastNetworkAdvance time.Time

	// number of throttled outgoing connections "slots" needed to be populated.
	// 채워야 하는 제한된 나가는 연결 "슬롯"의 수입니다.
	throttledOutgoingConnections int32

	// transport and dialer are customized to limit the number of connection in compliance with connectionsRateLimitingCount.
	// 전송 및 다이얼러는 connectionsRateLimitingCount에 따라 연결 수를 제한하도록 사용자 정의됩니다.
	transport rateLimitingTransport
	dialer    Dialer

	// messagesOfInterest specifies the message types that this node wants to receive
	// messagesOfInterest는 이 노드가 수신하려는 메시지 유형을 지정합니다.
	// nil means default.
	// nil은 기본값을 의미합니다.
	// non-nil causes this map to be sent to new peers as a MsgOfInterest message type.
	// non-nil은 이 맵이 MsgOfInterest 메시지 유형으로 새 피어에게 전송되도록 합니다.
	messagesOfInterest map[protocol.Tag]bool

	// messagesOfInterestEnc is the encoding of messagesOfInterest, to be sent to new peers.
	// messagesOfInterestEnc는 새로운 피어에게 보낼 메시지의 인코딩입니다.
	// This is filled in at network start, at which point messagesOfInterestEncoded is set to prevent further changes.
	// 이것은 네트워크 시작 시 채워지며, 이 시점에서 messagesOfInterestEncoded가 추가 변경을 방지하도록 설정됩니다.
	messagesOfInterestEnc     []byte
	messagesOfInterestEncoded bool

	// messagesOfInterestMu protects messagesOfInterest and ensures that messagesOfInterestEnc does not change once it is set during network start.
	// messagesOfInterestMu는 messagesOfInterest를 보호하고 네트워크 시작 중에 한 번 설정되면 messagesOfInterestEnc가 변경되지 않도록 합니다.
	messagesOfInterestMu deadlock.Mutex

	// peersConnectivityCheckTicker is the timer for testing that all the connected peers are still transmitting or receiving information.
	// peersConnectivityCheckTicker 는 연결된 모든 피어가 여전히 정보를 전송하거나 수신하고 있는지 테스트하기 위한 타이머입니다.
	// The channel produced by this ticker is consumed by any of the messageHandlerThread(s).
	// 이 티커에 의해 생성된 채널은 모든 messageHandlerThread(s)에 의해 사용됩니다.
	// The ticker itself is created during Start(), and being shut down when Stop() is called.
	// 티커 자체는 Start() 중에 생성되고 Stop()이 호출되면 종료됩니다.
	peersConnectivityCheckTicker *time.Ticker
}

type broadcastRequest struct {
	tags        []Tag
	data        [][]byte
	except      *wsPeer
	done        chan struct{}
	enqueueTime time.Time
	ctx         context.Context
}

// Address returns a string and whether that is a 'final' address or guessed.
// 주소는 문자열과 그것이 '최종' 주소인지 추측된 주소인지를 반환합니다.
// Part of GossipNode interface
// GossipNode 인터페이스의 일부
func (wn *WebsocketNetwork) Address() (string, bool) {
	parsedURL := url.URL{Scheme: wn.scheme}
	var connected bool
	if wn.listener == nil {
		if wn.config.NetAddress == "" {
			parsedURL.Scheme = ""
		}
		parsedURL.Host = wn.config.NetAddress
		connected = false
	} else {
		parsedURL.Host = wn.listener.Addr().String()
		connected = true
	}
	return parsedURL.String(), connected
}

// PublicAddress what we tell other nodes to connect to.
// 다른 노드에 연결하도록 지시하는 PublicAddress입니다.
// Might be different than our locally perceived network address due to NAT/etc.
// NAT/등으로 인해 로컬에서 인식되는 네트워크 주소와 다를 수 있습니다.
// Returns config "PublicAddress" if available, otherwise local addr.
// 사용 가능한 경우 구성 "PublicAddress"를 반환하고, 그렇지 않으면 로컬 주소를 반환합니다.
func (wn *WebsocketNetwork) PublicAddress() string {
	if len(wn.config.PublicAddress) > 0 {
		return wn.config.PublicAddress
	}
	localAddr, _ := wn.Address()
	return localAddr
}

// Broadcast sends a message.
// 브로드캐스트는 메시지를 보냅니다.
// If except is not nil then we will not send it to that neighboring Peer.
// 만약 except가 nil이 아니라면 우리는 그것을 이웃 Peer로 보내지 않을 것입니다.
// if wait is true then the call blocks until the packet has actually been sent to all neighbors.
// wait가 참이면 패킷이 모든 이웃에게 실제로 전송될 때까지 호출이 차단됩니다.
func (wn *WebsocketNetwork) Broadcast(ctx context.Context, tag protocol.Tag, data []byte, wait bool, except Peer) error {
	dataArray := make([][]byte, 1, 1)
	dataArray[0] = data
	tagArray := make([]protocol.Tag, 1, 1)
	tagArray[0] = tag
	return wn.BroadcastArray(ctx, tagArray, dataArray, wait, except)
}

// BroadcastArray sends an array of messages.
// BroadcastArray는 메시지 배열을 보냅니다.
// If except is not nil then we will not send it to that neighboring Peer.
// 만약 except가 nil이 아니라면 우리는 그것을 이웃 Peer로 보내지 않을 것입니다.
// if wait is true then the call blocks until the packet has actually been sent to all neighbors.
// wait가 참이면 패킷이 모든 이웃에게 실제로 전송될 때까지 호출이 차단됩니다.
// TODO: add `priority` argument so that we don't have to guess it based on tag
func (wn *WebsocketNetwork) BroadcastArray(ctx context.Context, tags []protocol.Tag, data [][]byte, wait bool, except Peer) error {
	if wn.config.DisableNetworking {
		return nil
	}

	if len(tags) != len(data) {
		return errBcastInvalidArray
	}

	request := broadcastRequest{tags: tags, data: data, enqueueTime: time.Now(), ctx: ctx}
	if except != nil {
		request.except = except.(*wsPeer)
	}

	broadcastQueue := wn.broadcastQueueBulk
	if highPriorityTag(tags) {
		broadcastQueue = wn.broadcastQueueHighPrio
	}
	if wait {
		request.done = make(chan struct{})
		select {
		case broadcastQueue <- request:
			// ok, enqueued
			//wn.log.Debugf("broadcast enqueued")
		case <-wn.ctx.Done():
			return errNetworkClosing
		case <-ctx.Done():
			return errBcastCallerCancel
		}
		select {
		case <-request.done:
			//wn.log.Debugf("broadcast done")
			return nil
		case <-wn.ctx.Done():
			return errNetworkClosing
		case <-ctx.Done():
			return errBcastCallerCancel
		}
	}
	// no wait
	select {
	case broadcastQueue <- request:
		//wn.log.Debugf("broadcast enqueued nowait")
		return nil
	default:
		wn.log.Debugf("broadcast queue full")
		// broadcastQueue full, and we're not going to wait for it.
		// broadcastQueue가 가득 차서 기다리지 않을 것입니다.
		networkBroadcastQueueFull.Inc(nil)
		return errBcastQFull
	}
}

// Relay message
func (wn *WebsocketNetwork) Relay(ctx context.Context, tag protocol.Tag, data []byte, wait bool, except Peer) error {
	if wn.relayMessages {
		return wn.Broadcast(ctx, tag, data, wait, except)
	}
	return nil
}

// RelayArray relays array of messages
// RelayArray는 메시지 배열을 릴레이합니다.
func (wn *WebsocketNetwork) RelayArray(ctx context.Context, tags []protocol.Tag, data [][]byte, wait bool, except Peer) error {
	if wn.relayMessages {
		return wn.BroadcastArray(ctx, tags, data, wait, except)
	}
	return nil
}

func (wn *WebsocketNetwork) disconnectThread(badnode Peer, reason disconnectReason) {
	defer wn.wg.Done()
	wn.disconnect(badnode, reason)
}

// Disconnect from a peer, probably due to protocol errors.
// 프로토콜 오류로 인해 피어에서 연결을 끊습니다.
func (wn *WebsocketNetwork) Disconnect(node Peer) {
	wn.disconnect(node, disconnectBadData)
}

// Disconnect from a peer, probably due to protocol errors.
// 프로토콜 오류로 인해 피어에서 연결을 끊습니다.
func (wn *WebsocketNetwork) disconnect(badnode Peer, reason disconnectReason) {
	if badnode == nil {
		return
	}
	peer := badnode.(*wsPeer)
	peer.CloseAndWait(time.Now().Add(peerDisconnectionAckDuration))
	wn.removePeer(peer, reason)
}

func closeWaiter(wg *sync.WaitGroup, peer *wsPeer, deadline time.Time) {
	defer wg.Done()
	peer.CloseAndWait(deadline)
}

// DisconnectPeers shuts down all connections
// DisconnectPeers는 모든 연결을 종료합니다.
func (wn *WebsocketNetwork) DisconnectPeers() {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	closeGroup := sync.WaitGroup{}
	closeGroup.Add(len(wn.peers))
	deadline := time.Now().Add(peerDisconnectionAckDuration)
	for _, peer := range wn.peers {
		go closeWaiter(&closeGroup, peer, deadline)
	}
	wn.peers = wn.peers[:0]
	closeGroup.Wait()
}

// Ready returns a chan that will be closed when we have a minimum number of peer connections active
// Ready는 최소한의 피어 연결이 활성화되었을 때 닫힐 chan을 반환합니다.
func (wn *WebsocketNetwork) Ready() chan struct{} {
	return wn.readyChan
}

// RegisterHTTPHandler path accepts gorilla/mux path annotations
// RegisterHTTPHandler 경로는 gorilla/mux 경로 주석을 허용합니다.
func (wn *WebsocketNetwork) RegisterHTTPHandler(path string, handler http.Handler) {
	wn.router.Handle(path, handler)
}

// RequestConnectOutgoing tries to actually do the connect to new peers.
// RequestConnectOutgoing 은 실제로 새 피어에 연결을 시도합니다.
// `replace` drop all connections first and find new peers.
// `replace`는 먼저 모든 연결을 삭제하고 새 피어를 찾습니다.
func (wn *WebsocketNetwork) RequestConnectOutgoing(replace bool, quit <-chan struct{}) {
	request := meshRequest{disconnect: false}
	if quit != nil {
		request.done = make(chan struct{})
	}
	select {
	case wn.meshUpdateRequests <- request:
	case <-quit:
		return
	}
	if request.done != nil {
		select {
		case <-request.done:
		case <-quit:
		}
	}
}

// GetPeers returns a snapshot of our Peer list, according to the specified options.
// GetPeers는 지정된 옵션에 따라 피어 목록의 스냅샷을 반환합니다.
// Peers may be duplicated and refer to the same underlying node.
// 피어는 중복될 수 있으며 동일한 기본 노드를 참조할 수 있습니다.
func (wn *WebsocketNetwork) GetPeers(options ...PeerOption) []Peer {
	outPeers := make([]Peer, 0)
	for _, option := range options {
		switch option {
		case PeersConnectedOut:
			wn.peersLock.RLock()
			for _, peer := range wn.peers {
				if peer.outgoing {
					outPeers = append(outPeers, Peer(peer))
				}
			}
			wn.peersLock.RUnlock()
		case PeersPhonebookRelays:
			// return copy of phonebook, which probably also contains peers we're connected to, but if it doesn't maybe we shouldn't be making new connections to those peers (because they disappeared from the directory)
			// 전화번호부 사본을 반환합니다. 여기에는 우리가 연결된 피어도 포함되어 있을 수 있지만 그렇지 않은 경우 해당 피어에 새 연결을 만들지 않아야 합니다(디렉토리에서 사라졌기 때문에).
			var addrs []string
			addrs = wn.phonebook.GetAddresses(1000, PhoneBookEntryRelayRole)
			for _, addr := range addrs {
				peerCore := makePeerCore(wn, addr, wn.GetRoundTripper(), "" /*origin address*/)
				outPeers = append(outPeers, &peerCore)
			}
		case PeersPhonebookArchivers:
			// return copy of phonebook, which probably also contains peers we're connected to, but if it doesn't maybe we shouldn't be making new connections to those peers (because they disappeared from the directory)
			// 전화번호부 사본을 반환합니다. 여기에는 우리가 연결된 피어도 포함되어 있을 수 있지만 그렇지 않은 경우 해당 피어에 새 연결을 만들지 않아야 합니다(디렉토리에서 사라졌기 때문에).
			var addrs []string
			addrs = wn.phonebook.GetAddresses(1000, PhoneBookEntryArchiverRole)
			for _, addr := range addrs {
				peerCore := makePeerCore(wn, addr, wn.GetRoundTripper(), "" /*origin address*/)
				outPeers = append(outPeers, &peerCore)
			}
		case PeersConnectedIn:
			wn.peersLock.RLock()
			for _, peer := range wn.peers {
				if !peer.outgoing {
					outPeers = append(outPeers, Peer(peer))
				}
			}
			wn.peersLock.RUnlock()
		}
	}
	return outPeers
}

// find the max value across the given uint64 numbers.
// 주어진 uint64 숫자에서 최대값을 찾습니다.
func max(numbers ...uint64) (maxNum uint64) {
	maxNum = 0 // this is the lowest uint64 value.
	for _, num := range numbers {
		if num > maxNum {
			maxNum = num
		}
	}
	return
}

func (wn *WebsocketNetwork) setup() {
	var preferredResolver dnssec.ResolverIf
	if wn.config.DNSSecurityRelayAddrEnforced() {
		preferredResolver = dnssec.MakeDefaultDnssecResolver(wn.config.FallbackDNSResolverAddress, wn.log)
	}
	maxIdleConnsPerHost := int(wn.config.ConnectionsRateLimitingCount)
	wn.dialer = makeRateLimitingDialer(wn.phonebook, preferredResolver)
	wn.transport = makeRateLimitingTransport(wn.phonebook, 10*time.Second, &wn.dialer, maxIdleConnsPerHost)

	wn.upgrader.ReadBufferSize = 4096
	wn.upgrader.WriteBufferSize = 4096
	wn.upgrader.EnableCompression = false
	wn.lastPeerConnectionsSent = time.Now()
	wn.router = mux.NewRouter()
	wn.router.Handle(GossipNetworkPath, wn)
	wn.requestsTracker = makeRequestsTracker(wn.router, wn.log, wn.config)
	if wn.config.EnableRequestLogger {
		wn.requestsLogger = makeRequestLogger(wn.requestsTracker, wn.log)
		wn.server.Handler = wn.requestsLogger
	} else {
		wn.server.Handler = wn.requestsTracker
	}
	wn.server.ReadHeaderTimeout = httpServerReadHeaderTimeout
	wn.server.WriteTimeout = httpServerWriteTimeout
	wn.server.IdleTimeout = httpServerIdleTimeout
	wn.server.MaxHeaderBytes = httpServerMaxHeaderBytes
	wn.ctx, wn.ctxCancel = context.WithCancel(context.Background())
	wn.relayMessages = wn.config.NetAddress != "" || wn.config.ForceRelayMessages
	// roughly estimate the number of messages that could be seen at any given moment.
	// 주어진 순간에 볼 수 있는 메시지 수를 대략적으로 추정합니다.
	// For the late/redo/down committee, which happen in parallel, we need to allocate extra space there.
	// 병렬로 발생하는 late/redo/down 위원회의 경우 추가 공간을 할당해야 합니다.
	wn.outgoingMessagesBufferSize = int(
		max(config.Consensus[protocol.ConsensusCurrentVersion].NumProposers,
			config.Consensus[protocol.ConsensusCurrentVersion].SoftCommitteeSize,
			config.Consensus[protocol.ConsensusCurrentVersion].CertCommitteeSize,
			config.Consensus[protocol.ConsensusCurrentVersion].NextCommitteeSize) +
			max(config.Consensus[protocol.ConsensusCurrentVersion].LateCommitteeSize,
				config.Consensus[protocol.ConsensusCurrentVersion].RedoCommitteeSize,
				config.Consensus[protocol.ConsensusCurrentVersion].DownCommitteeSize),
	)

	wn.broadcastQueueHighPrio = make(chan broadcastRequest, wn.outgoingMessagesBufferSize)
	wn.broadcastQueueBulk = make(chan broadcastRequest, 100)
	wn.meshUpdateRequests = make(chan meshRequest, 5)
	wn.readyChan = make(chan struct{})
	wn.tryConnectAddrs = make(map[string]int64)
	wn.eventualReadyDelay = time.Minute
	wn.prioTracker = newPrioTracker(wn)
	if wn.slowWritingPeerMonitorInterval == 0 {
		wn.slowWritingPeerMonitorInterval = slowWritingPeerMonitorInterval
	}

	readBufferLen := wn.config.IncomingConnectionsLimit + wn.config.GossipFanout
	if readBufferLen < 100 {
		readBufferLen = 100
	}
	if readBufferLen > 10000 {
		readBufferLen = 10000
	}
	wn.readBuffer = make(chan IncomingMessage, readBufferLen)

	var rbytes [10]byte
	crypto.RandBytes(rbytes[:])
	wn.RandomID = base64.StdEncoding.EncodeToString(rbytes[:])

	if wn.config.EnableIncomingMessageFilter {
		wn.incomingMsgFilter = makeMessageFilter(wn.config.IncomingMessageFilterBucketCount, wn.config.IncomingMessageFilterBucketSize)
	}
	wn.connPerfMonitor = makeConnectionPerformanceMonitor([]Tag{protocol.AgreementVoteTag, protocol.TxnTag})
	wn.lastNetworkAdvance = time.Now().UTC()
	wn.handlers.log = wn.log

	if wn.config.NetworkProtocolVersion != "" {
		SupportedProtocolVersions = []string{wn.config.NetworkProtocolVersion}
	}

	if wn.relayMessages {
		wn.RegisterMessageInterest(protocol.CompactCertSigTag)
	}
}

// Start makes network connections and threads
// 시작은 네트워크 연결 및 스레드를 만듭니다.
func (wn *WebsocketNetwork) Start() {
	wn.messagesOfInterestMu.Lock()
	defer wn.messagesOfInterestMu.Unlock()
	wn.messagesOfInterestEncoded = true
	if wn.messagesOfInterest != nil {
		wn.messagesOfInterestEnc = MarshallMessageOfInterestMap(wn.messagesOfInterest)
	}

	if wn.config.NetAddress != "" {
		listener, err := net.Listen("tcp", wn.config.NetAddress)
		if err != nil {
			wn.log.Errorf("network could not listen %v: %s", wn.config.NetAddress, err)
			return
		}
		// wrap the original listener with a limited connection listener
		// 제한된 연결 리스너로 원래 리스너를 래핑합니다.
		listener = limitlistener.RejectingLimitListener(
			listener, uint64(wn.config.IncomingConnectionsLimit), wn.log)
		// wrap the limited connection listener with a requests tracker listener
		// 제한된 연결 수신기를 요청 추적기 수신기로 래핑합니다.
		wn.listener = wn.requestsTracker.Listener(listener)
		wn.log.Debugf("listening on %s", wn.listener.Addr().String())
		wn.throttledOutgoingConnections = int32(wn.config.GossipFanout / 2)
	} else {
		// on non-relay, all the outgoing connections are throttled.
		// 릴레이가 아닌 경우 모든 나가는 연결이 제한됩니다.
		wn.throttledOutgoingConnections = int32(wn.config.GossipFanout)
	}
	if wn.config.DisableOutgoingConnectionThrottling {
		wn.throttledOutgoingConnections = 0
	}
	if wn.config.TLSCertFile != "" && wn.config.TLSKeyFile != "" {
		wn.scheme = "https"
	} else {
		wn.scheme = "http"
	}
	wn.meshUpdateRequests <- meshRequest{false, nil}
	if wn.prioScheme != nil {
		wn.RegisterHandlers(prioHandlers)
	}
	if wn.listener != nil {
		wn.wg.Add(1)
		go wn.httpdThread()
	}
	wn.wg.Add(1)
	go wn.meshThread()

	// we shouldn't have any ticker here.. but in case we do - just stop it.
	// 여기에 시세 표시기가 없어야 합니다. 하지만 만일의 경우를 대비하여 - 그냥 중지하십시오.
	if wn.peersConnectivityCheckTicker != nil {
		wn.peersConnectivityCheckTicker.Stop()
	}
	wn.peersConnectivityCheckTicker = time.NewTicker(connectionActivityMonitorInterval)
	for i := 0; i < incomingThreads; i++ {
		wn.wg.Add(1)
		// We pass the peersConnectivityCheckTicker.C here so that we don't need to syncronize the access to the ticker's data structure.
		// 티커의 데이터 구조에 대한 액세스를 동기화할 필요가 없도록 여기에 peersConnectivityCheckTicker.C를 전달합니다.
		go wn.messageHandlerThread(wn.peersConnectivityCheckTicker.C)
	}
	wn.wg.Add(1)
	go wn.broadcastThread()
	if wn.prioScheme != nil {
		wn.wg.Add(1)
		go wn.prioWeightRefresh()
	}
	wn.log.Infof("serving genesisID=%s on %#v with RandomID=%s", wn.GenesisID, wn.PublicAddress(), wn.RandomID)
}

func (wn *WebsocketNetwork) httpdThread() {
	defer wn.wg.Done()
	var err error
	if wn.config.TLSCertFile != "" && wn.config.TLSKeyFile != "" {
		err = wn.server.ServeTLS(wn.listener, wn.config.TLSCertFile, wn.config.TLSKeyFile)
	} else {
		err = wn.server.Serve(wn.listener)
	}
	if err == http.ErrServerClosed {
	} else if err != nil {
		wn.log.Info("ws net http server exited ", err)
	}
}

// innerStop context for shutting down peers
// 피어를 종료하기 위한 innerStop 컨텍스트
func (wn *WebsocketNetwork) innerStop() {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	wn.wg.Add(len(wn.peers))
	// this method is called only during node shutdown.
	// 이 메소드는 노드 종료 중에만 호출됩니다.
	// In this case, we want to send the shutdown message, but we don't want to wait for a long time - since we might not be lucky to get a response.
	// 이 경우 종료 메시지를 보내고 싶지만 응답을 받는 운이 좋지 않을 수 있으므로 오랜 시간을 기다리고 싶지 않습니다.
	deadline := time.Now().Add(peerShutdownDisconnectionAckDuration)
	for _, peer := range wn.peers {
		go closeWaiter(&wn.wg, peer, deadline)
	}
	wn.peers = wn.peers[:0]
}

// Stop closes network connections and stops threads.
// Stop은 네트워크 연결을 닫고 스레드를 중지합니다.
// Stop blocks until all activity on this node is done.
// 이 노드의 모든 활동이 완료될 때까지 블록을 중지합니다.
func (wn *WebsocketNetwork) Stop() {
	wn.handlers.ClearHandlers([]Tag{})

	// if we have a working ticker, just stop it and clear it out.
	// 작동하는 티커가 있으면 중지하고 지웁니다.
	// The access to this variable is safe since the Start()/Stop() are synced by the caller, and the WebsocketNetwork doesn't access wn.peersConnectivityCheckTicker directly.
	// Start()/Stop()이 호출자에 의해 동기화되고 WebsocketNetwork가 wn.peersConnectivityCheckTicker에 직접 액세스하지 않기 때문에 이 변수에 대한 액세스는 안전합니다.
	if wn.peersConnectivityCheckTicker != nil {
		wn.peersConnectivityCheckTicker.Stop()
		wn.peersConnectivityCheckTicker = nil
	}
	wn.innerStop()
	var listenAddr string
	if wn.listener != nil {
		listenAddr = wn.listener.Addr().String()
	}
	wn.ctxCancel()
	ctx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer timeoutCancel()
	err := wn.server.Shutdown(ctx)
	if err != nil {
		wn.log.Warnf("problem shutting down %s: %v", listenAddr, err)
	}
	wn.wg.Wait()
	if wn.listener != nil {
		wn.log.Debugf("closed %s", listenAddr)
	}

	// Wait for the requestsTracker to finish up to avoid potential race condition
	// 잠재적인 경쟁 조건을 피하기 위해 requestsTracker가 완료될 때까지 기다립니다.
	<-wn.requestsTracker.getWaitUntilNoConnectionsChannel(5 * time.Millisecond)
	wn.messagesOfInterestMu.Lock()
	defer wn.messagesOfInterestMu.Unlock()

	wn.messagesOfInterestEncoded = false
	wn.messagesOfInterestEnc = nil
	wn.messagesOfInterest = nil
}

// RegisterHandlers registers the set of given message handlers.
// RegisterHandlers는 주어진 메시지 핸들러 세트를 등록합니다.
func (wn *WebsocketNetwork) RegisterHandlers(dispatch []TaggedMessageHandler) {
	wn.handlers.RegisterHandlers(dispatch)
}

// ClearHandlers deregisters all the existing message handlers.
// ClearHandlers는 기존의 모든 메시지 핸들러를 등록 취소합니다.
func (wn *WebsocketNetwork) ClearHandlers() {
	// exclude the internal handlers. These would get cleared out when Stop is called.
	// 내부 핸들러를 제외합니다. Stop이 호출되면 지워집니다.
	wn.handlers.ClearHandlers([]Tag{protocol.PingTag, protocol.PingReplyTag, protocol.NetPrioResponseTag})
}

func (wn *WebsocketNetwork) setHeaders(header http.Header) {
	localTelemetryGUID := wn.log.GetTelemetryHostName()
	localInstanceName := wn.log.GetInstanceName()
	header.Set(TelemetryIDHeader, localTelemetryGUID)
	header.Set(InstanceNameHeader, localInstanceName)
	header.Set(AddressHeader, wn.PublicAddress())
	header.Set(NodeRandomHeader, wn.RandomID)
}

// checkServerResponseVariables check that the version and random-id in the request headers matches the server ones.
// 서버 응답 변수 확인 요청 헤더의 버전 및 random-id가 서버의 버전과 일치하는지 확인합니다.
// it returns true if it's a match, and false otherwise.
// 일치하면 true를 반환하고 그렇지 않으면 false를 반환합니다.
func (wn *WebsocketNetwork) checkServerResponseVariables(otherHeader http.Header, addr string) (bool, string) {
	matchingVersion, otherVersion := wn.checkProtocolVersionMatch(otherHeader)
	if matchingVersion == "" {
		wn.log.Info(filterASCII(fmt.Sprintf("new peer %s version mismatch, mine=%v theirs=%s, headers %#v", addr, SupportedProtocolVersions, otherVersion, otherHeader)))
		return false, ""
	}
	otherRandom := otherHeader.Get(NodeRandomHeader)
	if otherRandom == wn.RandomID || otherRandom == "" {
		// This is pretty harmless and some configurations of phonebooks or DNS records make this likely. Quietly filter it out.
		// 이것은 매우 무해하며 전화번호부 또는 DNS 레코드의 일부 구성이 이를 가능하게 합니다. 조용히 걸러냅니다.
		if otherRandom == "" {
			// missing header.
			// 헤더가 없습니다.
			wn.log.Warn(filterASCII(fmt.Sprintf("new peer %s did not include random ID header in request. mine=%s headers %#v", addr, wn.RandomID, otherHeader)))
		} else {
			wn.log.Debugf("new peer %s has same node random id, am I talking to myself? %s", addr, wn.RandomID)
		}
		return false, ""
	}
	otherGenesisID := otherHeader.Get(GenesisHeader)
	if wn.GenesisID != otherGenesisID {
		if otherGenesisID != "" {
			wn.log.Warn(filterASCII(fmt.Sprintf("new peer %#v genesis mismatch, mine=%#v theirs=%#v, headers %#v", addr, wn.GenesisID, otherGenesisID, otherHeader)))
		} else {
			wn.log.Warnf("new peer %#v did not include genesis header in response. mine=%#v headers %#v", addr, wn.GenesisID, otherHeader)
		}
		return false, ""
	}
	return true, matchingVersion
}

// getCommonHeaders retreives the common headers for both incoming and outgoing connections from the provided headers.
// getCommonHeaders는 제공된 헤더에서 들어오는 연결과 나가는 연결 모두에 대한 공통 헤더를 검색합니다.
func getCommonHeaders(headers http.Header) (otherTelemetryGUID, otherInstanceName, otherPublicAddr string) {
	otherTelemetryGUID = logging.SanitizeTelemetryString(headers.Get(TelemetryIDHeader), 1)
	otherInstanceName = logging.SanitizeTelemetryString(headers.Get(InstanceNameHeader), 2)
	otherPublicAddr = logging.SanitizeTelemetryString(headers.Get(AddressHeader), 1)
	return
}

// checkIncomingConnectionLimits perform the connection limits counting for the incoming connections.
// checkIncomingConnectionLimits는 들어오는 연결에 대한 연결 제한 계산을 수행합니다.
func (wn *WebsocketNetwork) checkIncomingConnectionLimits(response http.ResponseWriter, request *http.Request, remoteHost, otherTelemetryGUID, otherInstanceName string) int {
	if wn.numIncomingPeers() >= wn.config.IncomingConnectionsLimit {
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "incoming_connection_limit"})
		wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerFailEvent,
			telemetryspec.ConnectPeerFailEventDetails{
				Address:      remoteHost,
				HostName:     otherTelemetryGUID,
				Incoming:     true,
				InstanceName: otherInstanceName,
				Reason:       "Connection Limit",
			})
		response.WriteHeader(http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable
	}

	totalConnections := wn.connectedForIP(remoteHost)
	if totalConnections >= wn.config.MaxConnectionsPerIP {
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "incoming_connection_per_ip_limit"})
		wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerFailEvent,
			telemetryspec.ConnectPeerFailEventDetails{
				Address:      remoteHost,
				HostName:     otherTelemetryGUID,
				Incoming:     true,
				InstanceName: otherInstanceName,
				Reason:       "Remote IP Connection Limit",
			})
		response.WriteHeader(http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable
	}

	return http.StatusOK
}

// checkProtocolVersionMatch test ProtocolAcceptVersionHeader and ProtocolVersionHeader headers from the request/response and see if it can find a match.
// checkProtocolVersionMatch 요청/응답에서 ProtocolAcceptVersionHeader 및 ProtocolVersionHeader 헤더를 테스트하고 일치 항목을 찾을 수 있는지 확인합니다.
func (wn *WebsocketNetwork) checkProtocolVersionMatch(otherHeaders http.Header) (matchingVersion string, otherVersion string) {
	otherAcceptedVersions := otherHeaders[textproto.CanonicalMIMEHeaderKey(ProtocolAcceptVersionHeader)]
	for _, otherAcceptedVersion := range otherAcceptedVersions {
		// do we have a matching version ?
		for _, supportedProtocolVersion := range SupportedProtocolVersions {
			if supportedProtocolVersion == otherAcceptedVersion {
				matchingVersion = supportedProtocolVersion
				return matchingVersion, ""
			}
		}
	}

	otherVersion = otherHeaders.Get(ProtocolVersionHeader)
	for _, supportedProtocolVersion := range SupportedProtocolVersions {
		if supportedProtocolVersion == otherVersion {
			return supportedProtocolVersion, otherVersion
		}
	}

	return "", filterASCII(otherVersion)
}

// checkIncomingConnectionVariables checks the variables that were provided on the request, and compares them to the local server supported parameters.
// checkIncomingConnectionVariables는 요청에 제공된 변수를 확인하고 로컬 서버에서 지원하는 매개변수와 비교합니다.
// If all good, it returns http.StatusOK; otherwise, it write the error to the ResponseWriter and returns the http status.
// 모든 것이 정상이면 http.StatusOK를 반환합니다. 그렇지 않으면 오류를 ResponseWriter에 쓰고 http 상태를 반환합니다.
func (wn *WebsocketNetwork) checkIncomingConnectionVariables(response http.ResponseWriter, request *http.Request) int {
	// check to see that the genesisID in the request URI is valid and matches the supported one.
	// 요청 URI의 genesisID가 유효하고 지원되는 것과 일치하는지 확인합니다.
	pathVars := mux.Vars(request)
	otherGenesisID, hasGenesisID := pathVars["genesisID"]
	if !hasGenesisID || otherGenesisID == "" {
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "missing genesis-id"})
		response.WriteHeader(http.StatusNotFound)
		return http.StatusNotFound
	}

	if wn.GenesisID != otherGenesisID {
		wn.log.Warn(filterASCII(fmt.Sprintf("new peer %#v genesis mismatch, mine=%#v theirs=%#v, headers %#v", request.RemoteAddr, wn.GenesisID, otherGenesisID, request.Header)))
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "mismatching genesis-id"})
		response.WriteHeader(http.StatusPreconditionFailed)
		n, err := response.Write([]byte("mismatching genesis ID"))
		if err != nil {
			wn.log.Warnf("ws failed to write mismatching genesis ID response '%s' : n = %d err = %v", n, err)
		}
		return http.StatusPreconditionFailed
	}

	otherRandom := request.Header.Get(NodeRandomHeader)
	if otherRandom == "" {
		// This is pretty harmless and some configurations of phonebooks or DNS records make this likely. Quietly filter it out.
		// 이것은 매우 무해하며 전화번호부 또는 DNS 레코드의 일부 구성이 이를 가능하게 합니다. 조용히 걸러냅니다.
		var message string
		// missing header.
		// 헤더가 없습니다.
		wn.log.Warn(filterASCII(fmt.Sprintf("new peer %s did not include random ID header in request. mine=%s headers %#v", request.RemoteAddr, wn.RandomID, request.Header)))
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "missing random ID header"})
		message = fmt.Sprintf("Request was missing a %s header", NodeRandomHeader)
		response.WriteHeader(http.StatusPreconditionFailed)
		n, err := response.Write([]byte(message))
		if err != nil {
			wn.log.Warnf("ws failed to write response '%s' : n = %d err = %v", message, n, err)
		}
		return http.StatusPreconditionFailed
	} else if otherRandom == wn.RandomID {
		// This is pretty harmless and some configurations of phonebooks or DNS records make this likely. Quietly filter it out.
		// 이것은 매우 무해하며 전화번호부 또는 DNS 레코드의 일부 구성이 이를 가능하게 합니다. 조용히 걸러냅니다.
		var message string
		wn.log.Debugf("new peer %s has same node random id, am I talking to myself? %s", request.RemoteAddr, wn.RandomID)
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "matching random ID header"})
		message = fmt.Sprintf("Request included matching %s=%s header", NodeRandomHeader, otherRandom)
		response.WriteHeader(http.StatusLoopDetected)
		n, err := response.Write([]byte(message))
		if err != nil {
			wn.log.Warnf("ws failed to write response '%s' : n = %d err = %v", message, n, err)
		}
		return http.StatusLoopDetected
	}
	return http.StatusOK
}

// GetHTTPRequestConnection returns the underlying connection for the given request.
// GetHTTPRequestConnection은 주어진 요청에 대한 기본 연결을 반환합니다.
// Note that the request must be the same request that was provided to the http handler ( or provide a fallback Context() to that )
// 요청은 http 핸들러에 제공된 것과 동일한 요청이어야 합니다(또는 이에 대한 대체 Context()를 제공해야 함).
// if the provided request has no associated connection, it returns nil. ( this should not happen for any http request that was registered by WebsocketNetwork )
// 제공된 요청에 연결된 연결이 없으면 nil을 반환합니다. (WebsocketNetwork에 의해 등록된 모든 http 요청에 대해서는 발생하지 않아야 함)
func (wn *WebsocketNetwork) GetHTTPRequestConnection(request *http.Request) (conn net.Conn) {
	if wn.requestsTracker != nil {
		conn = wn.requestsTracker.GetRequestConnection(request)
	}
	return
}

// ServerHTTP handles the gossip network functions over websockets
// ServerHTTP는 웹 소켓을 통해 가십 네트워크 기능을 처리합니다. keep serving request
func (wn *WebsocketNetwork) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	trackedRequest := wn.requestsTracker.GetTrackedRequest(request)

	if wn.checkIncomingConnectionLimits(response, request, trackedRequest.remoteHost, trackedRequest.otherTelemetryGUID, trackedRequest.otherInstanceName) != http.StatusOK {
		// we've already logged and written all response(s).
		// 이미 모든 응답을 기록하고 작성했습니다.
		return
	}

	matchingVersion, otherVersion := wn.checkProtocolVersionMatch(request.Header)
	if matchingVersion == "" {
		wn.log.Info(filterASCII(fmt.Sprintf("new peer %s version mismatch, mine=%v theirs=%s, headers %#v", request.RemoteAddr, SupportedProtocolVersions, otherVersion, request.Header)))
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "mismatching protocol version"})
		response.WriteHeader(http.StatusPreconditionFailed)
		message := fmt.Sprintf("Requested version %s not in %v mismatches server version", filterASCII(otherVersion), SupportedProtocolVersions)
		n, err := response.Write([]byte(message))
		if err != nil {
			wn.log.Warnf("ws failed to write response '%s' : n = %d err = %v", message, n, err)
		}
		return
	}

	if wn.checkIncomingConnectionVariables(response, request) != http.StatusOK {
		// we've already logged and written all response(s).
		return
	}

	// if UseXForwardedForAddressField is not empty, attempt to override the otherPublicAddr with the X Forwarded For origin
	// UseXForwardedForAddressField가 비어 있지 않으면 otherPublicAddr을 X Forwarded For 원점으로 재정의하려고 시도합니다.
	trackedRequest.otherPublicAddr = trackedRequest.remoteAddr

	responseHeader := make(http.Header)
	wn.setHeaders(responseHeader)
	responseHeader.Set(ProtocolVersionHeader, matchingVersion)
	responseHeader.Set(GenesisHeader, wn.GenesisID)
	var challenge string
	if wn.prioScheme != nil {
		challenge = wn.prioScheme.NewPrioChallenge()
		responseHeader.Set(PriorityChallengeHeader, challenge)
	}
	conn, err := wn.upgrader.Upgrade(response, request, responseHeader)
	if err != nil {
		wn.log.Info("ws upgrade fail ", err)
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "ws upgrade fail"})
		return
	}

	// we want to tell the response object that the status was changed to 101 ( switching protocols ) so that it will be logged.
	// 응답 객체에 상태가 101(switching protocol)로 변경되었음을 알려주고 기록되도록 합니다.
	if wn.requestsLogger != nil {
		wn.requestsLogger.SetStatusCode(response, http.StatusSwitchingProtocols)
	}

	peer := &wsPeer{
		wsPeerCore:        makePeerCore(wn, trackedRequest.otherPublicAddr, wn.GetRoundTripper(), trackedRequest.remoteHost),
		conn:              conn,
		outgoing:          false,
		InstanceName:      trackedRequest.otherInstanceName,
		incomingMsgFilter: wn.incomingMsgFilter,
		prioChallenge:     challenge,
		createTime:        trackedRequest.created,
		version:           matchingVersion,
	}
	peer.TelemetryGUID = trackedRequest.otherTelemetryGUID
	peer.init(wn.config, wn.outgoingMessagesBufferSize)
	wn.addPeer(peer)
	localAddr, _ := wn.Address()
	wn.log.With("event", "ConnectedIn").With("remote", trackedRequest.otherPublicAddr).With("local", localAddr).Infof("Accepted incoming connection from peer %s", trackedRequest.otherPublicAddr)
	wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerEvent,
		telemetryspec.PeerEventDetails{
			Address:      trackedRequest.remoteHost,
			HostName:     trackedRequest.otherTelemetryGUID,
			Incoming:     true,
			InstanceName: trackedRequest.otherInstanceName,
		})

	// We are careful to encode this prior to starting the server to avoid needing 'messagesOfInterestMu' here.
	// 여기에 'messagesOfInterestMu'가 필요하지 않도록 서버를 시작하기 전에 이를 인코딩하는 데 주의합니다.
	if wn.messagesOfInterestEnc != nil {
		err = peer.Unicast(wn.ctx, wn.messagesOfInterestEnc, protocol.MsgOfInterestTag)
		if err != nil {
			wn.log.Infof("ws send msgOfInterest: %v", err)
		}
	}

	peers.Set(float64(wn.NumPeers()), nil)
	incomingPeers.Set(float64(wn.numIncomingPeers()), nil)
}

func (wn *WebsocketNetwork) messageHandlerThread(peersConnectivityCheckCh <-chan time.Time) {
	defer wn.wg.Done()

	for {
		select {
		case <-wn.ctx.Done():
			return
		case msg := <-wn.readBuffer:
			if msg.processing != nil {
				// The channel send should never block, but just in case..
				select {
				case msg.processing <- struct{}{}:
				default:
					wn.log.Warnf("could not send on msg.processing")
				}
			}
			if wn.config.EnableOutgoingNetworkMessageFiltering && len(msg.Data) >= messageFilterSize {
				wn.sendFilterMessage(msg)
			}
			//wn.log.Debugf("msg handling %#v [%d]byte", msg.Tag, len(msg.Data))
			start := time.Now()

			// now, send to global handlers
			// 이제 전역 핸들러로 보냅니다.
			outmsg := wn.handlers.Handle(msg)
			handled := time.Now()
			bufferNanos := start.UnixNano() - msg.Received
			networkIncomingBufferMicros.AddUint64(uint64(bufferNanos/1000), nil)
			handleTime := handled.Sub(start)
			networkHandleMicros.AddUint64(uint64(handleTime.Nanoseconds()/1000), nil)
			switch outmsg.Action {
			case Disconnect:
				wn.wg.Add(1)
				go wn.disconnectThread(msg.Sender, disconnectBadData)
			case Broadcast:
				err := wn.Broadcast(wn.ctx, msg.Tag, msg.Data, false, msg.Sender)
				if err != nil && err != errBcastQFull {
					wn.log.Warnf("WebsocketNetwork.messageHandlerThread: WebsocketNetwork.Broadcast returned unexpected error %v", err)
				}
			case Respond:
				err := msg.Sender.(*wsPeer).Respond(wn.ctx, msg, outmsg.Topics)
				if err != nil && err != wn.ctx.Err() {
					wn.log.Warnf("WebsocketNetwork.messageHandlerThread: wsPeer.Respond returned unexpected error %v", err)
				}
			default:
			}
		case <-peersConnectivityCheckCh:
			// go over the peers and ensure we have some type of communication going on.
			// 피어를 살펴보고 어떤 유형의 통신이 진행되고 있는지 확인합니다.
			wn.checkPeersConnectivity()
		}
	}
}

// checkPeersConnectivity tests the last timestamp where each of these peers was communicated with, and disconnect the peer if it has been too long since last time.
// peersConnectivity는 각 피어가 통신된 마지막 타임스탬프를 테스트하고 마지막 시간 이후 너무 오래되면 피어의 연결을 끊습니다.
func (wn *WebsocketNetwork) checkPeersConnectivity() {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	currentTime := time.Now()
	for _, peer := range wn.peers {
		lastPacketTime := peer.GetLastPacketTime()
		timeSinceLastPacket := currentTime.Sub(time.Unix(0, lastPacketTime))
		if timeSinceLastPacket > maxPeerInactivityDuration {
			wn.wg.Add(1)
			go wn.disconnectThread(peer, disconnectIdleConn)
			networkIdlePeerDrops.Inc(nil)
		}
	}
}

// checkSlowWritingPeers tests each of the peer's current message timestamp.
// checkSlowWritingPeers는 피어의 현재 메시지 타임스탬프 각각을 테스트합니다.
// if that timestamp is too old, it means that the transmission of that message
// 타임스탬프가 너무 오래된 경우 해당 메시지의 전송이
// takes longer than desired. In that case, it will disconnect the peer, allowing it to reconnect to a faster network endpoint.
// 원하는 것보다 오래 걸립니다. 이 경우 피어의 연결을 끊고 더 빠른 네트워크 끝점에 다시 연결할 수 있습니다.
func (wn *WebsocketNetwork) checkSlowWritingPeers() {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	currentTime := time.Now()
	for _, peer := range wn.peers {
		if peer.CheckSlowWritingPeer(currentTime) {
			wn.wg.Add(1)
			go wn.disconnectThread(peer, disconnectSlowConn)
			networkSlowPeerDrops.Inc(nil)
		}
	}
}

func (wn *WebsocketNetwork) sendFilterMessage(msg IncomingMessage) {
	digest := generateMessageDigest(msg.Tag, msg.Data)
	//wn.log.Debugf("send filter %s(%d) %v", msg.Tag, len(msg.Data), digest)
	err := wn.Broadcast(context.Background(), protocol.MsgDigestSkipTag, digest[:], false, msg.Sender)
	if err != nil && err != errBcastQFull {
		wn.log.Warnf("WebsocketNetwork.sendFilterMessage: WebsocketNetwork.Broadcast returned unexpected error %v", err)
	}
}

func (wn *WebsocketNetwork) broadcastThread() {
	defer wn.wg.Done()

	slowWritingPeerCheckTicker := time.NewTicker(wn.slowWritingPeerMonitorInterval)
	defer slowWritingPeerCheckTicker.Stop()
	peers, lastPeersChangeCounter := wn.peerSnapshot([]*wsPeer{})
	// updatePeers update the peers list if their peer change counter has changed.
	// updatePeers는 피어 변경 카운터가 변경된 경우 피어 목록을 업데이트합니다.
	updatePeers := func() {
		if curPeersChangeCounter := atomic.LoadInt32(&wn.peersChangeCounter); curPeersChangeCounter != lastPeersChangeCounter {
			peers, lastPeersChangeCounter = wn.peerSnapshot(peers)
		}
	}

	// waitForPeers waits until there is at least a single peer connected or pending request expires.
	// waitForPeers는 적어도 하나의 피어가 연결되거나 보류 중인 요청이 만료될 때까지 기다립니다.
	// in any of the above two cases, it returns true.
	// 위의 두 경우 중 하나라도 true를 반환합니다.
	// otherwise, false is returned ( the network context has expired )
	// 그렇지 않으면 false가 반환됩니다(네트워크 컨텍스트가 만료됨)
	waitForPeers := func(request *broadcastRequest) bool {
		// waitSleepTime defines how long we'd like to sleep between consecutive tests that the peers list have been updated.
		// waitSleepTime은 피어 목록이 업데이트된 연속 테스트 사이에 잠자기 시간을 정의합니다.
		const waitSleepTime = 5 * time.Millisecond
		// requestDeadline is the request deadline. If we surpass that deadline, the function returns true.
		// requestDeadline은 요청 기한입니다. 해당 기한을 초과하면 함수가 true를 반환합니다.
		var requestDeadline time.Time
		// sleepDuration is the current iteration sleep time.
		// sleepDuration은 현재 반복 수면 시간입니다.
		var sleepDuration time.Duration
		// initialize the requestDeadline if we have a request.
		// 요청이 있으면 requestDeadline을 초기화합니다.
		if request != nil {
			requestDeadline = request.enqueueTime.Add(maxMessageQueueDuration)
		} else {
			sleepDuration = waitSleepTime
		}

		// wait until the we have at least a single peer connected.
		// 적어도 하나의 피어가 연결될 때까지 기다립니다.
		for len(peers) == 0 {
			// adjust the sleep time in case we have a request
			// 요청이 있는 경우 수면 시간을 조정합니다.
			if request != nil {
				// we want to clamp the sleep time so that we won't sleep beyond the expiration of the request.
				// 우리는 요청 만료 이후에 잠을 자지 않도록 잠자기 시간을 고정하기를 원합니다.
				now := time.Now()
				sleepDuration = requestDeadline.Sub(now)
				if sleepDuration > waitSleepTime {
					sleepDuration = waitSleepTime
				} else if sleepDuration < 0 {
					return true
				}
			}
			select {
			case <-time.After(sleepDuration):
				if (request != nil) && time.Now().After(requestDeadline) {
					// message time have elapsed.
					return true
				}
				updatePeers()
				continue
			case <-wn.ctx.Done():
				return false
			}
		}
		return true
	}

	// load the peers list
	// 피어 목록 로드
	updatePeers()

	// wait until the we have at least a single peer connected.
	// 적어도 하나의 피어가 연결될 때까지 기다립니다.
	if !waitForPeers(nil) {
		return
	}

	for {
		// broadcast from high prio channel as long as we can
		// we want to try and keep this as a single case select with a default, since go compiles a single-case
		// select with a default into a more efficient non-blocking receive, instead of compiling it to the general-purpose selectgo
		// 가능한 한 높은 우선 순위 채널에서 방송
		// go는 단일 케이스를 컴파일하므로 기본값을 사용하여 단일 케이스 선택으로 유지하려고 합니다.
		// 범용 selectgo로 컴파일하는 대신 기본적으로 더 효율적인 비차단 수신으로 선택합니다.
		select {
		case request := <-wn.broadcastQueueHighPrio:
			wn.innerBroadcast(request, true, peers)
			continue
		default:
		}

		// if nothing high prio, try to sample from either queques in a non-blocking fashion.
		// 우선순위가 높은 것이 없으면 두 queque에서 non-blocking 방식으로 샘플링을 시도합니다.
		select {
		case request := <-wn.broadcastQueueHighPrio:
			wn.innerBroadcast(request, true, peers)
			continue
		case request := <-wn.broadcastQueueBulk:
			wn.innerBroadcast(request, false, peers)
			continue
		case <-wn.ctx.Done():
			return
		default:
		}

		// block until we have some request that need to be sent.
		// 보내야 할 요청이 있을 때까지 차단합니다.
		select {
		case request := <-wn.broadcastQueueHighPrio:
			// check if peers need to be updated, since we've been waiting a while.
			// 잠시 기다렸기 때문에 피어 업데이트가 필요한지 확인합니다.
			updatePeers()
			if !waitForPeers(&request) {
				return
			}
			wn.innerBroadcast(request, true, peers)
		case <-slowWritingPeerCheckTicker.C:
			wn.checkSlowWritingPeers()
			continue
		case request := <-wn.broadcastQueueBulk:
			// check if peers need to be updated, since we've been waiting a while.
			// 잠시 기다렸기 때문에 피어 업데이트가 필요한지 확인합니다.
			updatePeers()
			if !waitForPeers(&request) {
				return
			}
			wn.innerBroadcast(request, false, peers)
		case <-wn.ctx.Done():
			return
		}
	}
}

// peerSnapshot returns the currently connected peers as well as the current value of the peersChangeCounter
// peerSnapshot은 현재 연결된 피어와 peersChangeCounter의 현재 값을 반환합니다.
func (wn *WebsocketNetwork) peerSnapshot(dest []*wsPeer) ([]*wsPeer, int32) {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	if cap(dest) >= len(wn.peers) {
		// clear out the unused portion of the peers array to allow the GC to cleanup unused peers.
		// GC가 사용하지 않는 피어를 정리할 수 있도록 peers 배열의 사용되지 않은 부분을 지웁니다.
		remainderPeers := dest[len(wn.peers):cap(dest)]
		for i := range remainderPeers {
			// we want to delete only up to the first nil peer, since we're always writing to this array from the beginning to the end
			// 항상 처음부터 끝까지 이 배열에 쓰기 때문에 첫 번째 nil 피어까지만 삭제하려고 합니다.
			if remainderPeers[i] == nil {
				break
			}
			remainderPeers[i] = nil
		}
		// adjust array size
		// 배열 크기 조정
		dest = dest[:len(wn.peers)]
	} else {
		dest = make([]*wsPeer, len(wn.peers))
	}
	copy(dest, wn.peers)
	peerChangeCounter := atomic.LoadInt32(&wn.peersChangeCounter)
	return dest, peerChangeCounter
}

// prio is set if the broadcast is a high-priority broadcast.
// 방송이 우선순위가 높은 방송이면 prio가 설정됩니다.
func (wn *WebsocketNetwork) innerBroadcast(request broadcastRequest, prio bool, peers []*wsPeer) {
	if request.done != nil {
		defer close(request.done)
	}

	broadcastQueueDuration := time.Now().Sub(request.enqueueTime)
	networkBroadcastQueueMicros.AddUint64(uint64(broadcastQueueDuration.Nanoseconds()/1000), nil)
	if broadcastQueueDuration > maxMessageQueueDuration {
		networkBroadcastsDropped.Inc(nil)
		return
	}

	start := time.Now()

	digests := make([]crypto.Digest, len(request.data), len(request.data))
	data := make([][]byte, len(request.data), len(request.data))
	for i, d := range request.data {
		tbytes := []byte(request.tags[i])
		mbytes := make([]byte, len(tbytes)+len(d))
		copy(mbytes, tbytes)
		copy(mbytes[len(tbytes):], d)
		data[i] = mbytes
		if request.tags[i] != protocol.MsgDigestSkipTag && len(d) >= messageFilterSize {
			digests[i] = crypto.Hash(mbytes)
		}
	}

	// first send to all the easy outbound peers who don't block, get them started.
	// 먼저 차단하지 않는 모든 쉬운 아웃바운드 피어에게 보내고 시작하도록 합니다.
	sentMessageCount := 0
	for _, peer := range peers {
		if wn.config.BroadcastConnectionsLimit >= 0 && sentMessageCount >= wn.config.BroadcastConnectionsLimit {
			break
		}
		if peer == request.except {
			continue
		}
		ok := peer.writeNonBlockMsgs(request.ctx, data, prio, digests, request.enqueueTime)
		if ok {
			sentMessageCount++
			continue
		}
		networkPeerBroadcastDropped.Inc(nil)
	}

	dt := time.Now().Sub(start)
	networkBroadcasts.Inc(nil)
	networkBroadcastSendMicros.AddUint64(uint64(dt.Nanoseconds()/1000), nil)
}

// NumPeers returns number of peers we connect to (all peers incoming and outbound).
// NumPeers는 우리가 연결하는 피어의 수를 반환합니다(모든 피어 수신 및 발신).
func (wn *WebsocketNetwork) NumPeers() int {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	return len(wn.peers)
}

// outgoingPeers returns an array of the outgoing peers.
// outgoingPeers는 나가는 피어의 배열을 반환합니다.
func (wn *WebsocketNetwork) outgoingPeers() (peers []Peer) {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	peers = make([]Peer, 0, len(wn.peers))
	for _, peer := range wn.peers {
		if peer.outgoing {
			peers = append(peers, peer)
		}
	}
	return
}

func (wn *WebsocketNetwork) numOutgoingPeers() int {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	count := 0
	for _, peer := range wn.peers {
		if peer.outgoing {
			count++
		}
	}
	return count
}
func (wn *WebsocketNetwork) numIncomingPeers() int {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	count := 0
	for _, peer := range wn.peers {
		if !peer.outgoing {
			count++
		}
	}
	return count
}

// isConnectedTo returns true if addr matches any connected peer, based on the peer's root url.
// isConnected는 피어의 루트 URL을 기반으로 주소가 연결된 피어와 일치하는 경우 true를 반환합니다.
func (wn *WebsocketNetwork) isConnectedTo(addr string) bool {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	for _, peer := range wn.peers {
		if addr == peer.rootURL {
			return true
		}
	}
	return false
}

// connectedForIP returns number of peers with same host
// connectedForIP는 동일한 호스트를 가진 피어의 수를 반환합니다.
func (wn *WebsocketNetwork) connectedForIP(host string) (totalConnections int) {
	wn.peersLock.RLock()
	defer wn.peersLock.RUnlock()
	totalConnections = 0
	for _, peer := range wn.peers {
		if host == peer.OriginAddress() {
			totalConnections++
		}
	}
	return
}

const meshThreadInterval = time.Minute
const cliqueResolveInterval = 5 * time.Minute

type meshRequest struct {
	disconnect bool
	done       chan struct{}
}

func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// meshThread maintains the network, e.g. that we have sufficient connectivity to peers
// meshThread는 네트워크를 유지합니다. 우리는 동료들과 충분히 연결되어 있습니다.
func (wn *WebsocketNetwork) meshThread() {
	defer wn.wg.Done()
	timer := time.NewTicker(meshThreadInterval)
	defer timer.Stop()
	for {
		var request meshRequest
		select {
		case <-timer.C:
			request.disconnect = false
			request.done = nil
		case request = <-wn.meshUpdateRequests:
		case <-wn.ctx.Done():
			return
		}

		if request.disconnect {
			wn.DisconnectPeers()
		}

		// TODO: only do DNS fetch every N seconds? Honor DNS TTL? Trust DNS library we're using to handle caching and TTL?
		// TODO: N초마다 DNS만 가져오나요? DNS TTL을 존중하시겠습니까? 캐싱 및 TTL을 처리하는 데 사용하는 DNS 라이브러리를 신뢰하시겠습니까?
		dnsBootstrapArray := wn.config.DNSBootstrapArray(wn.NetworkID)
		for _, dnsBootstrap := range dnsBootstrapArray {
			relayAddrs, archiveAddrs := wn.getDNSAddrs(dnsBootstrap)
			if len(relayAddrs) > 0 {
				wn.log.Debugf("got %d relay dns addrs, %#v", len(relayAddrs), relayAddrs[:imin(5, len(relayAddrs))])
				wn.phonebook.ReplacePeerList(relayAddrs, dnsBootstrap, PhoneBookEntryRelayRole)
			} else {
				wn.log.Infof("got no relay DNS addrs for network %s", wn.NetworkID)
			}
			if len(archiveAddrs) > 0 {
				wn.phonebook.ReplacePeerList(archiveAddrs, dnsBootstrap, PhoneBookEntryArchiverRole)
			}
		}

		// as long as the call to checkExistingConnectionsNeedDisconnecting is deleting existing connections, we want to kick off the creation of new connections.
		// checkExistingConnectionsNeedDisconnecting에 대한 호출이 기존 연결을 삭제하는 한 새 연결 생성을 시작하려고 합니다.
		for {
			if wn.checkNewConnectionsNeeded() {
				// new connections were created.
				// 새로운 연결이 생성되었습니다.
				break
			}
			if !wn.checkExistingConnectionsNeedDisconnecting() {
				// no connection were removed.
				// 연결이 제거되지 않았습니다.
				break
			}
		}

		if request.done != nil {
			close(request.done)
		}

		// send the currently connected peers information to the telemetry server; that would allow the telemetry server to construct a cross-node map of all the nodes interconnections.
		// 현재 연결된 피어 정보를 원격 측정 서버로 보냅니다. 이를 통해 원격 측정 서버가 모든 노드 상호 연결의 노드 간 맵을 구성할 수 있습니다.
		wn.sendPeerConnectionsTelemetryStatus()
	}
}

// checkNewConnectionsNeeded checks to see if we need to have more connections to meet the GossipFanout target.
// checkNewConnectionsNeeded는 GossipFanout 대상을 충족하기 위해 더 많은 연결이 필요한지 확인합니다.
// if we do, it will spin async connection go routines.
// 그렇게 하면 비동기 연결 이동 루틴을 회전합니다.
// it returns false if no connections are needed, and true otherwise.
// 연결이 필요하지 않으면 false를 반환하고 그렇지 않으면 true를 반환합니다.
// note that the determination of needed connection could be inaccurate, and it might return false while more connection should be created.
// 필요한 연결의 결정이 부정확할 수 있으며 더 많은 연결을 만들어야 하는 동안 false를 반환할 수 있습니다.
func (wn *WebsocketNetwork) checkNewConnectionsNeeded() bool {
	desired := wn.config.GossipFanout
	numOutgoingTotal := wn.numOutgoingPeers() + wn.numOutgoingPending()
	need := desired - numOutgoingTotal
	if need <= 0 {
		return false
	}
	// get more than we need so that we can ignore duplicates
	// 중복을 무시할 수 있도록 필요한 것보다 더 많이 가져옵니다.
	newAddrs := wn.phonebook.GetAddresses(desired+numOutgoingTotal, PhoneBookEntryRelayRole)
	for _, na := range newAddrs {
		if na == wn.config.PublicAddress {
			// filter out self-public address, so we won't try to connect to outselves.
			// 자체 공개 주소를 필터링하여 우리 자신에게 연결을 시도하지 않습니다.
			continue
		}
		gossipAddr, ok := wn.tryConnectReserveAddr(na)
		if ok {
			wn.wg.Add(1)
			go wn.tryConnect(na, gossipAddr)
			need--
			if need == 0 {
				break
			}
		}
	}
	return true
}

// checkExistingConnectionsNeedDisconnecting check to see if existing connection need to be dropped due to performance issues and/or network being stalled.
// checkExistingConnectionsNeedDisconnecting 성능 문제 및/또는 네트워크 중단으로 인해 기존 연결을 끊어야 하는지 확인합니다.
func (wn *WebsocketNetwork) checkExistingConnectionsNeedDisconnecting() bool {
	// we already connected ( or connecting.. ) to  GossipFanout peers.
	// GossipFanout 피어에 이미 연결(또는 연결.. )했습니다.
	// get the actual peers.
	// 실제 피어를 가져옵니다.
	outgoingPeers := wn.outgoingPeers()
	if len(outgoingPeers) < wn.config.GossipFanout {
		// reset the performance monitor.
		// 성능 모니터를 재설정합니다.
		wn.connPerfMonitor.Reset([]Peer{})
		return wn.checkNetworkAdvanceDisconnect()
	}

	if !wn.connPerfMonitor.ComparePeers(outgoingPeers) {
		// different set of peers. restart monitoring.
		// 다른 피어 집합. 모니터링을 다시 시작합니다.
		wn.connPerfMonitor.Reset(outgoingPeers)
	}

	// same set of peers.
	// 동일한 피어 집합입니다.
	peerStat := wn.connPerfMonitor.GetPeersStatistics()
	if peerStat == nil {
		// performance metrics are not yet ready.
		return wn.checkNetworkAdvanceDisconnect()
	}

	// update peers with the performance metrics we've gathered.
	// 수집한 성능 메트릭으로 피어를 업데이트합니다.
	var leastPerformingPeer *wsPeer = nil
	for _, stat := range peerStat.peerStatistics {
		wsPeer := stat.peer.(*wsPeer)
		wsPeer.peerMessageDelay = stat.peerDelay
		wn.log.Infof("network performance monitor - peer '%s' delay %d first message portion %d%%", wsPeer.GetAddress(), stat.peerDelay, int(stat.peerFirstMessage*100))
		if wsPeer.throttledOutgoingConnection && leastPerformingPeer == nil {
			leastPerformingPeer = wsPeer
		}
	}
	if leastPerformingPeer == nil {
		return wn.checkNetworkAdvanceDisconnect()
	}
	wn.disconnect(leastPerformingPeer, disconnectLeastPerformingPeer)
	wn.connPerfMonitor.Reset([]Peer{})

	return true
}

// checkNetworkAdvanceDisconnect is using the lastNetworkAdvance indicator to see if the network is currently "stuck".
// checkNetworkAdvanceDisconnect는 lastNetworkAdvance 표시기를 사용하여 네트워크가 현재 "고착"되었는지 확인합니다.
// if it's seems to be "stuck", a randomally picked peer would be disconnected.
// "고정"된 것 같으면 무작위로 선택된 피어의 연결이 끊어집니다.
func (wn *WebsocketNetwork) checkNetworkAdvanceDisconnect() bool {
	lastNetworkAdvance := wn.getLastNetworkAdvance()
	if time.Now().UTC().Sub(lastNetworkAdvance) < cliqueResolveInterval {
		return false
	}
	outgoingPeers := wn.outgoingPeers()
	if len(outgoingPeers) == 0 {
		return false
	}
	if wn.numOutgoingPending() > 0 {
		// we're currently trying to extend the list of outgoing connections. no need to
		// disconnect any existing connection to free up room for another connection.
		return false
	}
	var peer *wsPeer
	disconnectPeerIdx := crypto.RandUint63() % uint64(len(outgoingPeers))
	peer = outgoingPeers[disconnectPeerIdx].(*wsPeer)

	wn.disconnect(peer, disconnectCliqueResolve)
	wn.connPerfMonitor.Reset([]Peer{})
	wn.OnNetworkAdvance()
	return true
}

func (wn *WebsocketNetwork) getLastNetworkAdvance() time.Time {
	wn.lastNetworkAdvanceMu.Lock()
	defer wn.lastNetworkAdvanceMu.Unlock()
	return wn.lastNetworkAdvance
}

// OnNetworkAdvance notifies the network library that the agreement protocol was able to make a notable progress.
// OnNetworkAdvance는 합의 프로토콜이 주목할 만한 진전을 이룰 수 있었음을 네트워크 라이브러리에 알립니다.
// this is the only indication that we have that we haven't formed a clique, where all incoming messages arrive very quickly, but might be missing some votes.
// 이것은 모든 수신 메시지가 매우 빠르게 도착하지만 일부 투표가 누락될 수 있는 파벌을 형성하지 않았다는 유일한 표시입니다.
// The usage of this call is expected to have similar
// 이 호출의 사용법은 비슷할 것으로 예상됩니다.
// characteristics as with a watchdog timer.
// 감시 타이머와 같은 특성.
func (wn *WebsocketNetwork) OnNetworkAdvance() {
	wn.lastNetworkAdvanceMu.Lock()
	defer wn.lastNetworkAdvanceMu.Unlock()
	wn.lastNetworkAdvance = time.Now().UTC()
}

// sendPeerConnectionsTelemetryStatus sends a snapshot of the currently connected peers to the telemetry server.
// sendPeerConnectionsTelemetryStatus는 현재 연결된 피어의 스냅샷을 원격 분석 서버로 보냅니다.
// Internally, it's using a timer to ensure that it would only send the information once every hour ( configurable via PeerConnectionsUpdateInterval )
// 내부적으로 타이머를 사용하여 1시간에 한 번만 정보를 보내도록 합니다( PeerConnectionsUpdateInterval을 통해 구성 가능).
func (wn *WebsocketNetwork) sendPeerConnectionsTelemetryStatus() {
	now := time.Now()
	if wn.lastPeerConnectionsSent.Add(time.Duration(wn.config.PeerConnectionsUpdateInterval)*time.Second).After(now) || wn.config.PeerConnectionsUpdateInterval <= 0 {
		// it's not yet time to send the update.
		// 아직 업데이트를 보낼 시간이 아닙니다.
		return
	}
	wn.lastPeerConnectionsSent = now
	var peers []*wsPeer
	peers, _ = wn.peerSnapshot(peers)
	var connectionDetails telemetryspec.PeersConnectionDetails
	for _, peer := range peers {
		connDetail := telemetryspec.PeerConnectionDetails{
			ConnectionDuration: uint(now.Sub(peer.createTime).Seconds()),
			HostName:           peer.TelemetryGUID,
			InstanceName:       peer.InstanceName,
		}
		if peer.outgoing {
			connDetail.Address = justHost(peer.conn.RemoteAddr().String())
			connDetail.Endpoint = peer.GetAddress()
			connDetail.MessageDelay = peer.peerMessageDelay
			connectionDetails.OutgoingPeers = append(connectionDetails.OutgoingPeers, connDetail)
		} else {
			connDetail.Address = peer.OriginAddress()
			connectionDetails.IncomingPeers = append(connectionDetails.IncomingPeers, connDetail)
		}
	}

	wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.PeerConnectionsEvent, connectionDetails)
}

// prioWeightRefreshTime controls how often we refresh the weights of connected peers.
// prioWeightRefreshTime은 연결된 피어의 가중치를 새로 고치는 빈도를 제어합니다.
const prioWeightRefreshTime = time.Minute

// prioWeightRefresh periodically refreshes the weights of connected peers.
// prioWeightRefresh는 연결된 피어의 가중치를 주기적으로 새로 고칩니다.
func (wn *WebsocketNetwork) prioWeightRefresh() {
	defer wn.wg.Done()
	ticker := time.NewTicker(prioWeightRefreshTime)
	defer ticker.Stop()
	var peers []*wsPeer
	// the lastPeersChangeCounter is initialized with -1 in order to force the peers to be loaded on the first iteration. then, it would get reloaded on per-need basis.
	// lastPeersChangeCounter는 피어가 첫 번째 반복에서 로드되도록 하기 위해 -1로 초기화됩니다. 그런 다음 필요에 따라 다시 로드됩니다.
	lastPeersChangeCounter := int32(-1)
	for {
		select {
		case <-ticker.C:
		case <-wn.ctx.Done():
			return
		}

		if curPeersChangeCounter := atomic.LoadInt32(&wn.peersChangeCounter); curPeersChangeCounter != lastPeersChangeCounter {
			peers, lastPeersChangeCounter = wn.peerSnapshot(peers)
		}

		for _, peer := range peers {
			wn.peersLock.RLock()
			addr := peer.prioAddress
			weight := peer.prioWeight
			wn.peersLock.RUnlock()

			newWeight := wn.prioScheme.GetPrioWeight(addr)
			if newWeight != weight {
				wn.peersLock.Lock()
				wn.prioTracker.setPriority(peer, addr, newWeight)
				wn.peersLock.Unlock()
			}
		}
	}
}

func (wn *WebsocketNetwork) getDNSAddrs(dnsBootstrap string) (relaysAddresses []string, archiverAddresses []string) {
	var err error
	relaysAddresses, err = tools_network.ReadFromSRV("algobootstrap", "tcp", dnsBootstrap, wn.config.FallbackDNSResolverAddress, wn.config.DNSSecuritySRVEnforced())
	if err != nil {
		// only log this warning on testnet or devnet
		// testnet 또는 devnet에만 이 경고를 기록합니다.
		if wn.NetworkID == config.Devnet || wn.NetworkID == config.Testnet {
			wn.log.Warnf("Cannot lookup algobootstrap SRV record for %s: %v", dnsBootstrap, err)
		}
		relaysAddresses = nil
	}
	if wn.config.EnableCatchupFromArchiveServers || wn.config.EnableBlockServiceFallbackToArchiver {
		archiverAddresses, err = tools_network.ReadFromSRV("archive", "tcp", dnsBootstrap, wn.config.FallbackDNSResolverAddress, wn.config.DNSSecuritySRVEnforced())
		if err != nil {
			// only log this warning on testnet or devnet
			// testnet 또는 devnet에만 이 경고를 기록합니다.
			if wn.NetworkID == config.Devnet || wn.NetworkID == config.Testnet {
				wn.log.Warnf("Cannot lookup archive SRV record for %s: %v", dnsBootstrap, err)
			}
			archiverAddresses = nil
		}
	}
	return
}

// ===

// ProtocolVersionHeader HTTP header for protocol version.
// ProtocolVersionHeader 프로토콜 버전에 대한 HTTP 헤더입니다.
const ProtocolVersionHeader = "X-Algorand-Version"

// ProtocolAcceptVersionHeader HTTP header for accept protocol version. Client use this to advertise supported protocol versions.
// ProtocolAcceptVersionHeader 프로토콜 버전을 수락하기 위한 HTTP 헤더입니다. 클라이언트는 이것을 사용하여 지원되는 프로토콜 버전을 광고합니다.
const ProtocolAcceptVersionHeader = "X-Algorand-Accept-Version"

// SupportedProtocolVersions contains the list of supported protocol versions by this node ( in order of preference ).
// SupportedProtocolVersions에는 이 노드가 지원하는 프로토콜 버전 목록이 포함되어 있습니다(기본 설정 순서).
var SupportedProtocolVersions = []string{"2.1"}

// ProtocolVersion is the current version attached to the ProtocolVersionHeader header
/* Version history:
 *  1   Catchup service over websocket connections with unicast messages between peers
 *  2.1 Introduced topic key/data pairs and enabled services over the gossip connections
 */
// ProtocolVersion은 ProtocolVersionHeader 헤더에 첨부된 현재 버전입니다.
/* 버전 기록:
 * 1 피어 간의 유니캐스트 메시지로 웹 소켓 연결을 통한 캐치업 서비스
 * 2.1 가십 연결을 통해 주제 키/데이터 쌍 및 활성화된 서비스 도입
 */
const ProtocolVersion = "2.1"

// TelemetryIDHeader HTTP header for telemetry-id for logging
// TelemetryIDHeader 로깅을 위한 원격 측정 ID용 HTTP 헤더
const TelemetryIDHeader = "X-Algorand-TelId"

// GenesisHeader HTTP header for genesis id to make sure we're on the same chain
// GenesisHeader 동일한 체인에 있는지 확인하기 위한 Genesis id용 HTTP 헤더
const GenesisHeader = "X-Algorand-Genesis"

// NodeRandomHeader HTTP header that a node uses to make sure it's not talking to itself
// NodeRandomHeader 노드가 자신과 통신하지 않는지 확인하기 위해 사용하는 HTTP 헤더
const NodeRandomHeader = "X-Algorand-NodeRandom"

// AddressHeader HTTP header by which an inbound connection reports its public address
// AddressHeader 인바운드 연결이 공개 주소를 보고하는 HTTP 헤더
const AddressHeader = "X-Algorand-Location"

// InstanceNameHeader HTTP header by which an inbound connection reports an ID to distinguish multiple local nodes.
// InstanceNameHeader 인바운드 연결이 여러 로컬 노드를 구별하기 위해 ID를 보고하는 HTTP 헤더입니다.
const InstanceNameHeader = "X-Algorand-InstanceName"

// PriorityChallengeHeader HTTP header informs a client about the challenge it should sign to increase network priority.
// PriorityChallengeHeader HTTP 헤더는 클라이언트에게 네트워크 우선 순위를 높이기 위해 서명해야 하는 챌린지를 알려줍니다.
const PriorityChallengeHeader = "X-Algorand-PriorityChallenge"

// TooManyRequestsRetryAfterHeader HTTP header let the client know when to make the next connection attempt
// TooManyRequestsRetryAfterHeader HTTP 헤더는 클라이언트가 다음 연결을 시도할 시기를 알려줍니다.
const TooManyRequestsRetryAfterHeader = "Retry-After"

// UserAgentHeader is the HTTP header identify the user agent.
// UserAgentHeader는 사용자 에이전트를 식별하는 HTTP 헤더입니다.
const UserAgentHeader = "User-Agent"

var websocketsScheme = map[string]string{"http": "ws", "https": "wss"}

var errBadAddr = errors.New("bad address")

var errNetworkClosing = errors.New("WebsocketNetwork shutting down")

var errBcastCallerCancel = errors.New("caller cancelled broadcast")

var errBcastInvalidArray = errors.New("invalid broadcast array")

var errBcastQFull = errors.New("broadcast queue full")

var errURLNoHost = errors.New("could not parse a host from url")

var errURLColonHost = errors.New("host name starts with a colon")

// HostColonPortPattern matches "^[-a-zA-Z0-9.]+:\\d+$" e.g. "foo.com.:1234"
var HostColonPortPattern = regexp.MustCompile("^[-a-zA-Z0-9.]+:\\d+$")

// ParseHostOrURL handles "host:port" or a full URL.
// ParseHostOrURL은 "host:port" 또는 전체 URL을 처리합니다.
// Standard library net/url.Parse chokes on "host:port".
// 표준 라이브러리 net/url.Parse는 "host:port"에서 chokes 합니다.
func ParseHostOrURL(addr string) (*url.URL, error) {
	// If the entire addr is "host:port" grab that right away.
	// 전체 addr이 "host:port"라면 바로 잡아라.
	// Don't try url.Parse() because that will grab "host:" as if it were "scheme:"
	// "scheme:"인 것처럼 "host:"를 잡을 것이기 때문에 url.Parse()를 시도하지 마십시오.
	if HostColonPortPattern.MatchString(addr) {
		return &url.URL{Scheme: "http", Host: addr}, nil
	}
	parsed, err := url.Parse(addr)
	if err == nil {
		if parsed.Host == "" {
			return nil, errURLNoHost
		}
		return parsed, nil
	}
	if strings.HasPrefix(addr, "http:") || strings.HasPrefix(addr, "https:") || strings.HasPrefix(addr, "ws:") || strings.HasPrefix(addr, "wss:") || strings.HasPrefix(addr, "://") || strings.HasPrefix(addr, "//") {
		return parsed, err
	}
	// This turns "[::]:4601" into "http://[::]:4601" which url.Parse can do
	parsed, e2 := url.Parse("http://" + addr)
	if e2 == nil {
		// https://datatracker.ietf.org/doc/html/rfc1123#section-2
		// first character is relaxed to allow either a letter or a digit
		// 첫 번째 문자는 문자나 숫자를 허용하도록 완화됩니다.
		if parsed.Host[0] == ':' && (len(parsed.Host) < 2 || parsed.Host[1] != ':') {
			return nil, errURLColonHost
		}
		return parsed, nil
	}
	return parsed, err /* return original err, not our prefix altered try */ /* 변경된 접두어가 아닌 원래 오류를 반환합니다. */
}

// addrToGossipAddr parses host:port or a URL and returns the URL to the websocket interface at that address.
// addrToGossipAddr은 host:port 또는 URL을 구문 분석하고 해당 주소의 websocket 인터페이스에 대한 URL을 반환합니다.
func (wn *WebsocketNetwork) addrToGossipAddr(addr string) (string, error) {
	parsedURL, err := ParseHostOrURL(addr)
	if err != nil {
		wn.log.Warnf("could not parse addr %#v: %s", addr, err)
		return "", errBadAddr
	}
	parsedURL.Scheme = websocketsScheme[parsedURL.Scheme]
	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "ws"
	}
	parsedURL.Path = strings.Replace(path.Join(parsedURL.Path, GossipNetworkPath), "{genesisID}", wn.GenesisID, -1)
	return parsedURL.String(), nil
}

// tryConnectReserveAddr synchronously checks that addr is not already being connected to, returns (websocket URL or "", true if connection may proceed)
// tryConnectReserveAddr은 addr이 아직 연결되어 있지 않은지 동기적으로 확인하고 반환(websocket URL 또는 "", 연결이 계속될 경우 true)
func (wn *WebsocketNetwork) tryConnectReserveAddr(addr string) (gossipAddr string, ok bool) {
	wn.tryConnectLock.Lock()
	defer wn.tryConnectLock.Unlock()
	_, exists := wn.tryConnectAddrs[addr]
	if exists {
		return "", false
	}
	gossipAddr, err := wn.addrToGossipAddr(addr)
	if err != nil {
		return "", false
	}
	_, exists = wn.tryConnectAddrs[gossipAddr]
	if exists {
		return "", false
	}
	// WARNING: isConnectedTo takes wn.peersLock; to avoid deadlock, never try to take wn.peersLock outside an attempt to lock wn.tryConnectLock
	// 경고: isConnectedTo는 wn.peersLock을 사용합니다. 교착 상태를 피하려면 wn.tryConnectLock을 잠그려는 시도 외부에서 wn.peersLock을 사용하지 마십시오.
	if wn.isConnectedTo(addr) {
		return "", false
	}
	now := time.Now().Unix()
	wn.tryConnectAddrs[addr] = now
	wn.tryConnectAddrs[gossipAddr] = now
	return gossipAddr, true
}

// tryConnectReleaseAddr should be called when connection succeeds and becomes a peer or fails and is no longer being attempted
// 연결이 성공하여 피어가 되거나 실패하여 더 이상 시도되지 않을 때 tryConnectReleaseAddr을 호출해야 합니다.
func (wn *WebsocketNetwork) tryConnectReleaseAddr(addr, gossipAddr string) {
	wn.tryConnectLock.Lock()
	defer wn.tryConnectLock.Unlock()
	delete(wn.tryConnectAddrs, addr)
	delete(wn.tryConnectAddrs, gossipAddr)
}

func (wn *WebsocketNetwork) numOutgoingPending() int {
	wn.tryConnectLock.Lock()
	defer wn.tryConnectLock.Unlock()
	return len(wn.tryConnectAddrs)
}

// GetRoundTripper returns an http.Transport that limits the number of connection to comply with connectionsRateLimitingCount.
// GetRoundTripper는 connectionsRateLimitingCount 를 준수하도록 연결 수를 제한하는 http.Transport를 반환합니다.
func (wn *WebsocketNetwork) GetRoundTripper() http.RoundTripper {
	return &wn.transport
}

// filterASCII filter out the non-ascii printable characters out of the given input string and replace these with unprintableCharacterGlyph.
// filterASCII는 주어진 입력 문자열에서 ASCII가 아닌 인쇄 가능한 문자를 걸러내고 이를 unprintableCharacterGlyph로 바꿉니다.
// It's used as a security qualifier before logging a network-provided data.
// 네트워크에서 제공하는 데이터를 로깅하기 전에 보안 한정자로 사용됩니다.
// The function allows only characters in the range of [32..126], which excludes all the control character, new lines, deletion, etc.
// 이 함수는 [32..126] 범위의 문자만 허용하며 모든 제어 문자, 줄 바꿈, 삭제 등을 제외합니다.
// All the alpha numeric and punctuation characters are included in this range.
// 모든 영숫자와 구두점이 이 범위에 포함됩니다.
func filterASCII(unfilteredString string) (filteredString string) {
	for i, r := range unfilteredString {
		if int(r) >= 0x20 && int(r) <= 0x7e {
			filteredString += string(unfilteredString[i])
		} else {
			filteredString += unprintableCharacterGlyph
		}
	}
	return
}

// tryConnect opens websocket connection and checks initial connection parameters.
// tryConnect는 웹 소켓 연결을 열고 초기 연결 매개변수를 확인합니다.
// addr should be 'host:port' or a URL, gossipAddr is the websocket endpoint URL
// addr은 'host:port' 또는 URL이어야 합니다. gossipAddr은 웹 소켓 끝점 URL입니다.
func (wn *WebsocketNetwork) tryConnect(addr, gossipAddr string) {
	defer wn.tryConnectReleaseAddr(addr, gossipAddr)
	defer func() {
		if xpanic := recover(); xpanic != nil {
			wn.log.Errorf("panic in tryConnect: %v", xpanic)
		}
	}()
	defer wn.wg.Done()
	requestHeader := make(http.Header)
	wn.setHeaders(requestHeader)
	for _, supportedProtocolVersion := range SupportedProtocolVersions {
		requestHeader.Add(ProtocolAcceptVersionHeader, supportedProtocolVersion)
	}
	// for backward compatibility, include the ProtocolVersion header as well.
	// 이전 버전과의 호환성을 위해 ProtocolVersion 헤더도 포함합니다.
	requestHeader.Set(ProtocolVersionHeader, ProtocolVersion)
	SetUserAgentHeader(requestHeader)
	myInstanceName := wn.log.GetInstanceName()
	requestHeader.Set(InstanceNameHeader, myInstanceName)
	var websocketDialer = websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  45 * time.Second,
		EnableCompression: false,
		NetDialContext:    wn.dialer.DialContext,
		NetDial:           wn.dialer.Dial,
	}

	conn, response, err := websocketDialer.DialContext(wn.ctx, gossipAddr, requestHeader)
	if err != nil {
		if err == websocket.ErrBadHandshake {
			// reading here from ioutil is safe only because it came from DialContext above, which alredy finsihed reading all the data from the network and placed it all in a ioutil.NopCloser reader.
			// ioutil에서 여기를 읽는 것은 위의 DialContext에서 가져왔기 때문에 안전합니다.
			// DialContext는 이미 네트워크에서 모든 데이터 읽기를 완료하고 ioutil.NopCloser 판독기에 모두 배치했습니다.
			bodyBytes, _ := ioutil.ReadAll(response.Body)
			errString := string(bodyBytes)
			if len(errString) > 128 {
				errString = errString[:128]
			}
			errString = filterASCII(errString)

			// we're guaranteed to have a valid response object.
			// 유효한 응답 객체가 있음을 보장합니다.
			switch response.StatusCode {
			case http.StatusPreconditionFailed:
				wn.log.Warnf("ws connect(%s) fail - bad handshake, precondition failed : '%s'", gossipAddr, errString)
			case http.StatusLoopDetected:
				wn.log.Infof("ws connect(%s) aborted due to connecting to self", gossipAddr)
			case http.StatusTooManyRequests:
				wn.log.Infof("ws connect(%s) aborted due to connecting too frequently", gossipAddr)
				retryAfterHeader := response.Header.Get(TooManyRequestsRetryAfterHeader)
				if retryAfter, retryParseErr := strconv.ParseUint(retryAfterHeader, 10, 32); retryParseErr == nil {
					// we've got a retry-after header.
					// 재시도 후 헤더가 있습니다.
					// convert it to a timestamp so that we could use it.
					// 사용할 수 있도록 타임스탬프로 변환합니다.
					retryAfterTime := time.Now().Add(time.Duration(retryAfter) * time.Second)
					wn.phonebook.UpdateRetryAfter(addr, retryAfterTime)
				}
			default:
				wn.log.Warnf("ws connect(%s) fail - bad handshake, Status code = %d, Headers = %#v, Body = %s", gossipAddr, response.StatusCode, response.Header, errString)
			}
		} else {
			wn.log.Warnf("ws connect(%s) fail: %s", gossipAddr, err)
		}
		return
	}

	// no need to test the response.StatusCode since we know it's going to be http.StatusSwitchingProtocols, as it's already being tested inside websocketDialer.DialContext.
	// 이미 websocketDialer.DialContext 내에서 테스트 중이므로 http.StatusSwitchingProtocols가 될 것이라는 것을 알고 있으므로 response.StatusCode를 테스트할 필요가 없습니다.
	// we need to examine the headers here to extract which protocol version we should be using.
	// 사용해야 하는 프로토콜 버전을 추출하기 위해 여기에서 헤더를 검사해야 합니다.
	responseHeaderOk, matchingVersion := wn.checkServerResponseVariables(response.Header, gossipAddr)
	if !responseHeaderOk {
		// The error was already logged, so no need to log again.
		return
	}

	throttledConnection := false
	if atomic.AddInt32(&wn.throttledOutgoingConnections, int32(-1)) >= 0 {
		throttledConnection = true
	} else {
		atomic.AddInt32(&wn.throttledOutgoingConnections, int32(1))
	}

	peer := &wsPeer{
		wsPeerCore:                  makePeerCore(wn, addr, wn.GetRoundTripper(), "" /* origin */),
		conn:                        conn,
		outgoing:                    true,
		incomingMsgFilter:           wn.incomingMsgFilter,
		createTime:                  time.Now(),
		connMonitor:                 wn.connPerfMonitor,
		throttledOutgoingConnection: throttledConnection,
		version:                     matchingVersion,
	}
	peer.TelemetryGUID, peer.InstanceName, _ = getCommonHeaders(response.Header)
	peer.init(wn.config, wn.outgoingMessagesBufferSize)
	wn.addPeer(peer)
	localAddr, _ := wn.Address()
	wn.log.With("event", "ConnectedOut").With("remote", addr).With("local", localAddr).Infof("Made outgoing connection to peer %v", addr)
	wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerEvent,
		telemetryspec.PeerEventDetails{
			Address:      justHost(conn.RemoteAddr().String()),
			HostName:     peer.TelemetryGUID,
			Incoming:     false,
			InstanceName: peer.InstanceName,
			Endpoint:     peer.GetAddress(),
		})

	peers.Set(float64(wn.NumPeers()), nil)
	outgoingPeers.Set(float64(wn.numOutgoingPeers()), nil)

	if wn.prioScheme != nil {
		challenge := response.Header.Get(PriorityChallengeHeader)
		if challenge != "" {
			resp := wn.prioScheme.MakePrioResponse(challenge)
			if resp != nil {
				mbytes := append([]byte(protocol.NetPrioResponseTag), resp...)
				sent := peer.writeNonBlock(context.Background(), mbytes, true, crypto.Digest{}, time.Now())
				if !sent {
					wn.log.With("remote", addr).With("local", localAddr).Warnf("could not send priority response to %v", addr)
				}
			}
		}
	}
}

// GetPeerData returns the peer data associated with a particular key.
// GetPeerData는 특정 키와 연결된 피어 데이터를 반환합니다.
func (wn *WebsocketNetwork) GetPeerData(peer Peer, key string) interface{} {
	switch p := peer.(type) {
	case *wsPeer:
		return p.getPeerData(key)
	default:
		return nil
	}
}

// SetPeerData sets the peer data associated with a particular key.
// SetPeerData는 특정 키와 관련된 피어 데이터를 설정합니다.
func (wn *WebsocketNetwork) SetPeerData(peer Peer, key string, value interface{}) {
	switch p := peer.(type) {
	case *wsPeer:
		p.setPeerData(key, value)
	default:
		return
	}
}

// NewWebsocketNetwork constructor for websockets based gossip network
// 웹 소켓 기반 가십 네트워크용 NewWebsocketNetwork 생성자
func NewWebsocketNetwork(log logging.Logger, config config.Local, phonebookAddresses []string, genesisID string, networkID protocol.NetworkID) (wn *WebsocketNetwork, err error) {
	phonebook := MakePhonebook(config.ConnectionsRateLimitingCount,
		time.Duration(config.ConnectionsRateLimitingWindowSeconds)*time.Second)
	phonebook.ReplacePeerList(phonebookAddresses, config.DNSBootstrapID, PhoneBookEntryRelayRole)
	wn = &WebsocketNetwork{
		log:       log,
		config:    config,
		phonebook: phonebook,
		GenesisID: genesisID,
		NetworkID: networkID,
	}

	wn.setup()
	return wn, nil
}

// NewWebsocketGossipNode constructs a websocket network node and returns it as a GossipNode interface implementation
// NewWebsocketGossipNode는 웹 소켓 네트워크 노드를 구성하고 GossipNode 인터페이스 구현으로 반환합니다.
func NewWebsocketGossipNode(log logging.Logger, config config.Local, phonebookAddresses []string, genesisID string, networkID protocol.NetworkID) (gn GossipNode, err error) {
	return NewWebsocketNetwork(log, config, phonebookAddresses, genesisID, networkID)
}

// SetPrioScheme specifies the network priority scheme for a network node
// SetPrioScheme은 네트워크 노드에 대한 네트워크 우선 순위 체계를 지정합니다.
func (wn *WebsocketNetwork) SetPrioScheme(s NetPrioScheme) {
	wn.prioScheme = s
}

// called from wsPeer to report that it has closed
// 닫혔다고 보고하기 위해 wsPeer에서 호출됨
func (wn *WebsocketNetwork) peerRemoteClose(peer *wsPeer, reason disconnectReason) {
	wn.removePeer(peer, reason)
}

func (wn *WebsocketNetwork) removePeer(peer *wsPeer, reason disconnectReason) {
	// first logging, then take the lock and do the actual accounting.
	// 먼저 로깅한 다음 잠금을 설정하고 실제 계정을 수행합니다.
	// definitely don't change this to do the logging while holding the lock.
	// 잠금을 유지하는 동안 로깅을 수행하도록 변경하지 마십시오.
	localAddr, _ := wn.Address()
	logEntry := wn.log.With("event", "Disconnected").With("remote", peer.rootURL).With("local", localAddr)
	if peer.outgoing && peer.peerMessageDelay > 0 {
		logEntry = logEntry.With("messageDelay", peer.peerMessageDelay)
	}
	logEntry.Infof("Peer %s disconnected: %s", peer.rootURL, reason)
	peerAddr := peer.OriginAddress()
	// we might be able to get addr out of conn, or it might be closed
	// conn에서 add를 가져오거나 닫을 수 있습니다.
	if peerAddr == "" && peer.conn != nil {
		paddr := peer.conn.RemoteAddr()
		if paddr != nil {
			peerAddr = justHost(paddr.String())
		}
	}
	if peerAddr == "" {
		// didn't get addr from peer, try from url
		// 피어로부터 addr을 받지 못함, url에서 시도
		url, err := url.Parse(peer.rootURL)
		if err == nil {
			peerAddr = justHost(url.Host)
		} else {
			// use whatever it is
			// 무엇이든 사용
			peerAddr = justHost(peer.rootURL)
		}
	}
	eventDetails := telemetryspec.PeerEventDetails{
		Address:      peerAddr,
		HostName:     peer.TelemetryGUID,
		Incoming:     !peer.outgoing,
		InstanceName: peer.InstanceName,
	}
	if peer.outgoing {
		eventDetails.Endpoint = peer.GetAddress()
		eventDetails.MessageDelay = peer.peerMessageDelay
	}
	wn.log.EventWithDetails(telemetryspec.Network, telemetryspec.DisconnectPeerEvent,
		telemetryspec.DisconnectPeerEventDetails{
			PeerEventDetails: eventDetails,
			Reason:           string(reason),
		})

	peers.Set(float64(wn.NumPeers()), nil)
	incomingPeers.Set(float64(wn.numIncomingPeers()), nil)
	outgoingPeers.Set(float64(wn.numOutgoingPeers()), nil)

	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	if peer.peerIndex < len(wn.peers) && wn.peers[peer.peerIndex] == peer {
		heap.Remove(peersHeap{wn}, peer.peerIndex)
		wn.prioTracker.removePeer(peer)
		if peer.throttledOutgoingConnection {
			atomic.AddInt32(&wn.throttledOutgoingConnections, int32(1))
		}
		atomic.AddInt32(&wn.peersChangeCounter, 1)
	}
	wn.countPeersSetGauges()
}

func (wn *WebsocketNetwork) addPeer(peer *wsPeer) {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	for _, p := range wn.peers {
		if p == peer {
			wn.log.Errorf("dup peer added %#v", peer)
			return
		}
	}
	heap.Push(peersHeap{wn}, peer)
	wn.prioTracker.setPriority(peer, peer.prioAddress, peer.prioWeight)
	atomic.AddInt32(&wn.peersChangeCounter, 1)
	wn.countPeersSetGauges()
	if len(wn.peers) >= wn.config.GossipFanout {
		// we have a quorum of connected peers, if we weren't ready before, we are now
		// 연결된 피어의 쿼럼이 있습니다. 이전에 준비되지 않았다면 이제 ㅡㅡ
		if atomic.CompareAndSwapInt32(&wn.ready, 0, 1) {
			wn.log.Debug("ready")
			close(wn.readyChan)
		}
	} else if atomic.LoadInt32(&wn.ready) == 0 {
		// but if we're not ready in a minute, call whatever peers we've got as good enough
		// 하지만 1분 안에 준비가 되지 않으면 충분히 좋은 동료를 호출합니다.
		wn.wg.Add(1)
		go wn.eventualReady()
	}
}

func (wn *WebsocketNetwork) eventualReady() {
	defer wn.wg.Done()
	minute := time.NewTimer(wn.eventualReadyDelay)
	select {
	case <-wn.ctx.Done():
	case <-minute.C:
		if atomic.CompareAndSwapInt32(&wn.ready, 0, 1) {
			wn.log.Debug("ready")
			close(wn.readyChan)
		}
	}
}

// should be run from inside a context holding wn.peersLock
// wn.peersLock을 보유하고 있는 컨텍스트 내부에서 실행되어야 합니다.
func (wn *WebsocketNetwork) countPeersSetGauges() {
	numIn := 0
	numOut := 0
	for _, xp := range wn.peers {
		if xp.outgoing {
			numOut++
		} else {
			numIn++
		}
	}
	networkIncomingConnections.Set(float64(numIn), nil)
	networkOutgoingConnections.Set(float64(numOut), nil)
}

func justHost(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return host
}

// SetUserAgentHeader adds the User-Agent header to the provided heades map.
// SetUserAgentHeader는 제공된 헤더 맵에 User-Agent 헤더를 추가합니다.
func SetUserAgentHeader(header http.Header) {
	version := config.GetCurrentVersion()
	ua := fmt.Sprintf("algod/%d.%d (%s; commit=%s; %d) %s(%s)", version.Major, version.Minor, version.Channel, version.CommitHash, version.BuildNumber, runtime.GOOS, runtime.GOARCH)
	header.Set(UserAgentHeader, ua)
}

// RegisterMessageInterest notifies the network library that this node wants to receive messages with the specified tag.
// RegisterMessageInterest는 이 노드가 지정된 태그가 있는 메시지를 수신하기를 원한다는 것을 네트워크 라이브러리에 알립니다.
// This will cause this node to send corresponding MsgOfInterest notifications to any newly connecting peers.
// 이것은 이 노드가 대응하는 MsgOfInterest 알림을 새로 연결하는 피어에게 보내도록 합니다.
// This should be called before the network is started.
// 이것은 네트워크가 시작되기 전에 호출되어야 합니다.
func (wn *WebsocketNetwork) RegisterMessageInterest(t protocol.Tag) error {
	wn.messagesOfInterestMu.Lock()
	defer wn.messagesOfInterestMu.Unlock()

	if wn.messagesOfInterestEncoded {
		return fmt.Errorf("network already started")
	}

	if wn.messagesOfInterest == nil {
		wn.messagesOfInterest = make(map[protocol.Tag]bool)
		for tag, flag := range defaultSendMessageTags {
			wn.messagesOfInterest[tag] = flag
		}
	}

	wn.messagesOfInterest[t] = true
	return nil
}

// SubstituteGenesisID substitutes the "{genesisID}" with their network-specific genesisID.
// SubstituteGenesisID는 "{genesisID}"를 네트워크별 genesisID로 대체합니다.
func (wn *WebsocketNetwork) SubstituteGenesisID(rawURL string) string {
	return strings.Replace(rawURL, "{genesisID}", wn.GenesisID, -1)
}
