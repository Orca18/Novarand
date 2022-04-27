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
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
)

const (
	// maxHeaderReadTimeout is the time limit where items would remain in the acceptedConnections cache before being pruned.
	// maxHeaderReadTimeout은 항목이 정리되기 전에 acceptedConnections 캐시에 남아 있는 시간 제한입니다.
	// certain malicious connections would never get to the http handler, and therefore must be pruned every so often.
	// 특정 악의적인 연결은 절대로 http 처리기에 도달하지 않으므로 자주 제거해야 합니다.
	maxHeaderReadTimeout = 30 * time.Second
)

// TrackerRequest hold the tracking data associated with a single request.
// TrackerRequest는 단일 요청과 관련된 추적 데이터를 보유합니다.
type TrackerRequest struct {
	created            time.Time
	remoteHost         string
	remotePort         string
	remoteAddr         string
	request            *http.Request
	otherTelemetryGUID string
	otherInstanceName  string
	otherPublicAddr    string
	connection         net.Conn
	noPrune            bool
}

// makeTrackerRequest creates a new TrackerRequest.
// makeTrackerRequest는 새로운 TrackerRequest를 생성합니다.
func makeTrackerRequest(remoteAddr, remoteHost, remotePort string, createTime time.Time, conn net.Conn) *TrackerRequest {
	if remoteHost == "" {
		remoteHost, remotePort, _ = net.SplitHostPort(remoteAddr)
	}

	return &TrackerRequest{
		created:    createTime,
		remoteAddr: remoteAddr,
		remoteHost: remoteHost,
		remotePort: remotePort,
		connection: conn,
	}
}

// hostIncomingRequests holds all the requests that are originating from a single host.
// hostIncomingRequests는 단일 호스트에서 시작된 모든 요청을 보유합니다.
type hostIncomingRequests struct {
	remoteHost             string
	requests               []*TrackerRequest            // this is an ordered list, according to the requestsHistory.created // 이것은 requestsHistory.created에 따라 정렬된 목록입니다.
	additionalHostRequests map[*TrackerRequest]struct{} // additional requests that aren't included in the "requests", and always assumed to be "alive". // "요청"에 포함되지 않고 항상 "활성"으로 간주되는 추가 요청.
}

// findTimestampIndex finds the first an index (i) in the sorted requests array, where requests[i].created is greater than t.
// if no such item exists, it returns the index where the item should
// findTimestampIndex는 정렬된 요청 배열에서 첫 번째 인덱스(i)를 찾습니다. 여기서 requests[i].created는 t보다 큽니다.
// 그러한 항목이 없으면 항목이 있어야 하는 인덱스를 반환합니다.
func (ard *hostIncomingRequests) findTimestampIndex(t time.Time) int {
	if len(ard.requests) == 0 {
		return 0
	}
	i := sort.Search(len(ard.requests), func(i int) bool {
		return ard.requests[i].created.After(t)
	})
	return i
}

// convertToAdditionalRequest converts the given trackerRequest into a "additional request".
// unlike regular tracker requests, additional requests does not get pruned.
// convertToAdditionalRequest는 주어진 trackerRequest를 "추가 요청"으로 변환합니다.
// 일반 추적기 요청과 달리 추가 요청은 정리되지 않습니다.
func (ard *hostIncomingRequests) convertToAdditionalRequest(trackerRequest *TrackerRequest) {
	if _, has := ard.additionalHostRequests[trackerRequest]; has {
		return
	}

	i := sort.Search(len(ard.requests), func(i int) bool {
		return ard.requests[i].created.After(trackerRequest.created)
	})
	i--
	if i < 0 {
		return
	}
	// we could have several entries with the same timestamp, so we need to consider all of them.
	// 타임스탬프가 같은 항목이 여러 개 있을 수 있으므로 모두 고려해야 합니다.
	for ; i >= 0; i-- {
		if ard.requests[i] == trackerRequest {
			break
		}
		if ard.requests[i].created != trackerRequest.created {
			// we can't find the item in the list.
			return
		}
	}
	if i < 0 {
		return
	}
	// ok, item was found at index i.
	// 좋아, 항목이 인덱스 i에서 발견되었습니다.
	copy(ard.requests[i:], ard.requests[i+1:])
	ard.requests[len(ard.requests)-1] = nil
	ard.requests = ard.requests[:len(ard.requests)-1]
	ard.additionalHostRequests[trackerRequest] = struct{}{}
}

// removeTrackedConnection removes a trackerRequest from the additional requests map
// removeTrackedConnection은 추가 요청 맵에서 trackerRequest를 제거합니다.
func (ard *hostIncomingRequests) removeTrackedConnection(trackerRequest *TrackerRequest) {
	delete(ard.additionalHostRequests, trackerRequest)
}

// add adds the trackerRequest at the correct index within the sorted array.
// add는 정렬된 배열 내의 올바른 인덱스에 trackerRequest를 추가합니다.
func (ard *hostIncomingRequests) add(trackerRequest *TrackerRequest) {
	// find the new item index.
	// 새 항목 인덱스를 찾습니다.
	itemIdx := ard.findTimestampIndex(trackerRequest.created)
	if itemIdx >= len(ard.requests) {
		// it's going to be added as the last item on the list.
		// 목록의 마지막 항목으로 추가됩니다.
		ard.requests = append(ard.requests, trackerRequest)
		return
	}
	if itemIdx == 0 {
		// it's going to be added as the first item on the list.
		// 목록의 첫 번째 항목으로 추가됩니다.
		ard.requests = append([]*TrackerRequest{trackerRequest}, ard.requests...)
		return
	}
	// it's going to be added somewhere in the middle.
	// 중간 어딘가에 추가될 것입니다.
	ard.requests = append(ard.requests[:itemIdx], append([]*TrackerRequest{trackerRequest}, ard.requests[itemIdx:]...)...)
	return
}

// countConnections counts the number of connection that we have that occurred after the provided specified time
// countConnections는 제공된 지정된 시간 이후에 발생한 연결 수를 계산합니다.
func (ard *hostIncomingRequests) countConnections(rateLimitingWindowStartTime time.Time) (count uint) {
	i := ard.findTimestampIndex(rateLimitingWindowStartTime)
	return uint(len(ard.requests) - i + len(ard.additionalHostRequests))
}

type hostsIncomingMap map[string]*hostIncomingRequests

// pruneRequests cleans stale items from the hostRequests maps
// pruneRequests는 hostRequests 맵에서 오래된 항목을 정리합니다.
func (him *hostsIncomingMap) pruneRequests(rateLimitingWindowStartTime time.Time) {
	// try to eliminate as many entries from a *single* connection. the goal here is not to wipe it clean
	// but rather to make a progressive cleanup.
	// *단일* 연결에서 최대한 많은 항목을 제거하려고 시도합니다. 여기의 목표는 깨끗하게 닦지 않는 것입니다
	// 오히려 점진적인 정리를 수행합니다.
	var removeHost string

	for host, requestData := range *him {
		i := requestData.findTimestampIndex(rateLimitingWindowStartTime)
		if i == 0 {
			continue
		}

		requestData.requests = requestData.requests[i:]
		if len(requestData.requests) == 0 {
			// remove the entire key.
			removeHost = host
		}
		break
	}
	if removeHost != "" {
		delete(*him, removeHost)
	}
}

// addRequest adds an entry to the hostRequests map, or update the item within the map
// addRequest는 hostRequests 맵에 항목을 추가하거나 맵 내의 항목을 업데이트합니다.
func (him *hostsIncomingMap) addRequest(trackerRequest *TrackerRequest) {
	requestData, has := (*him)[trackerRequest.remoteHost]
	if !has {
		requestData = &hostIncomingRequests{
			remoteHost:             trackerRequest.remoteHost,
			requests:               make([]*TrackerRequest, 0, 1),
			additionalHostRequests: make(map[*TrackerRequest]struct{}),
		}
		(*him)[trackerRequest.remoteHost] = requestData
	}

	requestData.add(trackerRequest)
}

// countOriginConnections counts the number of connection that were seen since rateLimitingWindowStartTime coming from the host rateLimitingWindowStartTime
// countOriginConnections는 호스트 rateLimitingWindowStartTime에서 오는 rateLimitingWindowStartTime 이후에 본 연결 수를 계산합니다.
func (him *hostsIncomingMap) countOriginConnections(remoteHost string, rateLimitingWindowStartTime time.Time) uint {
	if requestData, has := (*him)[remoteHost]; has {
		return requestData.countConnections(rateLimitingWindowStartTime)
	}
	return 0
}

// convertToAdditionalRequest converts the given trackerRequest into a "additional request".
// convertToAdditionalRequest는 주어진 trackerRequest를 "추가 요청"으로 변환합니다.
func (him *hostsIncomingMap) convertToAdditionalRequest(trackerRequest *TrackerRequest) {
	requestData, has := (*him)[trackerRequest.remoteHost]
	if !has {
		return
	}
	requestData.convertToAdditionalRequest(trackerRequest)
}

// removeTrackedConnection removes a trackerRequest from the additional requests map
// removeTrackedConnection은 추가 요청 맵에서 trackerRequest를 제거합니다.
func (him *hostsIncomingMap) removeTrackedConnection(trackerRequest *TrackerRequest) {
	requestData, has := (*him)[trackerRequest.remoteHost]
	if !has {
		return
	}
	requestData.removeTrackedConnection(trackerRequest)
}

// RequestTracker tracks the incoming request connections
// RequestTracker는 들어오는 요청 연결을 추적합니다.
type RequestTracker struct {
	downstreamHandler http.Handler
	log               logging.Logger
	config            config.Local
	// once we detect that we have a misconfigured UseForwardedForAddress, we set this and write an warning message.
	// UseForwardedForAddress가 잘못 구성된 것을 감지하면 이를 설정하고 경고 메시지를 작성합니다.
	misconfiguredUseForwardedForAddress bool

	listener net.Listener // this is the downsteam listener // 이것은 다운스트림 리스너입니다.

	hostRequests        hostsIncomingMap             // maps a request host to a request data (i.e. "1.2.3.4" -> *hostIncomingRequests )
	acceptedConnections map[net.Addr]*TrackerRequest // maps a local address interface  to a tracked request data (i.e. "1.2.3.4:1560" -> *TrackerRequest ); used to associate connection between the Accept and the ServeHTTP
	hostRequestsMu      deadlock.Mutex               // used to syncronize access to the hostRequests and acceptedConnections variables
	// 요청 호스트를 요청 데이터에 매핑합니다(즉, "1.2.3.4" -> *hostIncomingRequests ).
	// 로컬 주소 인터페이스를 추적된 요청 데이터에 매핑합니다(예: "1.2.3.4:1560" -> *TrackerRequest ). Accept와 ServeHTTP 간의 연결을 연결하는 데 사용됩니다.
	// hostRequests 및 acceptedConnections 변수에 대한 액세스를 동기화하는 데 사용됩니다.

	httpHostRequests  hostsIncomingMap             // maps a request host to a request data (i.e. "1.2.3.4" -> *hostIncomingRequests )
	httpConnections   map[net.Addr]*TrackerRequest // maps a local address interface  to a tracked request data (i.e. "1.2.3.4:1560" -> *TrackerRequest ); used to associate connection between the Accept and the ServeHTTP
	httpConnectionsMu deadlock.Mutex               // used to syncronize access to the httpHostRequests and httpConnections variables
	// 요청 호스트를 요청 데이터에 매핑합니다(즉, "1.2.3.4" -> *hostIncomingRequests ).
	// 로컬 주소 인터페이스를 추적된 요청 데이터에 매핑합니다(예: "1.2.3.4:1560" -> *TrackerRequest ). Accept와 ServeHTTP 간의 연결을 연결하는 데 사용됩니다.
	// httpHostRequests 및 httpConnections 변수에 대한 액세스를 동기화하는 데 사용됩니다.
}

// makeRequestsTracker creates a request tracker object.
// makeRequestsTracker는 요청 추적기 객체를 생성합니다.
func makeRequestsTracker(downstreamHandler http.Handler, log logging.Logger, config config.Local) *RequestTracker {
	return &RequestTracker{
		downstreamHandler:   downstreamHandler,
		log:                 log,
		config:              config,
		hostRequests:        make(map[string]*hostIncomingRequests, 0),
		acceptedConnections: make(map[net.Addr]*TrackerRequest, 0),
		httpConnections:     make(map[net.Addr]*TrackerRequest, 0),
		httpHostRequests:    make(map[string]*hostIncomingRequests, 0),
	}
}

// requestTrackedConnection used to track the active connections.
// In particular, it used to remove the tracked connection entry from the RequestTracker once a connection is closed.
// 활성 연결을 추적하는 데 사용되는 requestTrackedConnection.
// 특히 연결이 닫히면 RequestTracker에서 추적된 연결 항목을 제거하는 데 사용됩니다.
type requestTrackedConnection struct {
	net.Conn
	tracker *RequestTracker
}

// Close removes the connection from the tracker's connections map and call the underlaying Close function.
// Close는 추적기의 연결 맵에서 연결을 제거하고 밑에 있는 Close 함수를 호출합니다.
func (c *requestTrackedConnection) Close() error {
	c.tracker.hostRequestsMu.Lock()
	trackerRequest := c.tracker.acceptedConnections[c.Conn.LocalAddr()]
	delete(c.tracker.acceptedConnections, c.Conn.LocalAddr())
	if trackerRequest != nil {
		c.tracker.hostRequests.removeTrackedConnection(trackerRequest)
	}
	c.tracker.hostRequestsMu.Unlock()
	return c.Conn.Close()
}

// Accept waits for and returns the next connection to the listener.
// Accept는 리스너에 대한 다음 연결을 기다리고 반환합니다.
func (rt *RequestTracker) Accept() (conn net.Conn, err error) {
	// the following for loop is a bit tricky :
	// in the normal use case, we accept the connection and exit right away.
	// the only case where the for loop is being iterated is when we are rejecting a connection.
	// 다음 for 루프는 약간 까다롭습니다.
	// 일반적인 사용 사례에서는 연결을 수락하고 즉시 종료합니다.
	// for 루프가 반복되는 유일한 경우는 연결을 거부하는 경우입니다.
	for {
		conn, err = rt.listener.Accept()
		if err != nil || conn == nil {
			return
		}

		trackerRequest := makeTrackerRequest(conn.RemoteAddr().String(), "", "", time.Now(), conn)
		rateLimitingWindowStartTime := trackerRequest.created.Add(-time.Duration(rt.config.ConnectionsRateLimitingWindowSeconds) * time.Second)

		rt.hostRequestsMu.Lock()
		rt.hostRequests.addRequest(trackerRequest)
		rt.hostRequests.pruneRequests(rateLimitingWindowStartTime)
		originConnections := rt.hostRequests.countOriginConnections(trackerRequest.remoteHost, rateLimitingWindowStartTime)

		rateLimitedRemoteHost := (!rt.config.DisableLocalhostConnectionRateLimit) || (!isLocalhost(trackerRequest.remoteHost))
		connectionLimitEnabled := rt.config.ConnectionsRateLimitingWindowSeconds > 0 && rt.config.ConnectionsRateLimitingCount > 0

		// check the number of connections
		if originConnections > rt.config.ConnectionsRateLimitingCount && connectionLimitEnabled && rateLimitedRemoteHost {
			rt.hostRequestsMu.Unlock()
			networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "incoming_connection_per_ip_tcp_rate_limit"})
			rt.log.With("connection", "tcp").With("count", originConnections).Debugf("Rejected connection due to excessive connections attempt rate")
			rt.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerFailEvent,
				telemetryspec.ConnectPeerFailEventDetails{
					Address:  trackerRequest.remoteHost,
					Incoming: true,
					Reason:   "Remote IP Connection TCP Rate Limit",
				})

			// we've already *doubled* the amount of allowed connections; disconnect right away.
			// we don't want to create more go routines beyond this point.
			// 우리는 이미 허용된 연결의 양을 *두 배* 늘렸습니다. 즉시 연결을 끊습니다.
			// 우리는 이 지점을 넘어서 더 많은 go 루틴을 만들고 싶지 않습니다.
			if originConnections > rt.config.ConnectionsRateLimitingCount*2 {
				err := conn.Close()
				if err != nil {
					rt.log.With("connection", "tcp").With("count", originConnections).Debugf("Failed to close connection : %v", err)
				}
			} else {
				// we want to make an attempt to read the connection reqest and send a response, but not within this go routine -
				// this go routine is used single-threaded and should not get blocked.
				// 연결 요청을 읽고 응답을 보내려는 시도를 하고 싶지만 이 go 루틴 내에서는 아닙니다 -
				// 이 go 루틴은 단일 스레드로 사용되며 차단되어서는 안 됩니다.
				go rt.sendBlockedConnectionResponse(conn, trackerRequest.created)
			}
			continue
		}

		rt.pruneAcceptedConnections(trackerRequest.created.Add(-maxHeaderReadTimeout))
		// add an entry to the acceptedConnections so that the ServeHTTP could find the connection quickly.
		// ServeHTTP가 연결을 빠르게 찾을 수 있도록 AcceptedConnections에 항목을 추가합니다.
		rt.acceptedConnections[conn.LocalAddr()] = trackerRequest
		rt.hostRequestsMu.Unlock()
		conn = &requestTrackedConnection{Conn: conn, tracker: rt}
		return
	}
}

// sendBlockedConnectionResponse reads the incoming connection request followed by sending a "too many requests" response.
// sendBlockedConnectionResponse는 들어오는 연결 요청을 읽고 "너무 많은 요청" 응답을 보냅니다.
func (rt *RequestTracker) sendBlockedConnectionResponse(conn net.Conn, requestTime time.Time) {
	defer func() {
		err := conn.Close()
		if err != nil {
			rt.log.With("connection", "tcp").Debugf("Failed to close connection of blocked connection response: %v", err)
		}
	}()
	err := conn.SetReadDeadline(requestTime.Add(500 * time.Millisecond))
	if err != nil {
		rt.log.With("connection", "tcp").Debugf("Failed to set a read deadline of blocked connection response: %v", err)
		return
	}
	err = conn.SetWriteDeadline(requestTime.Add(500 * time.Millisecond))
	if err != nil {
		rt.log.With("connection", "tcp").Debugf("Failed to set a write deadline of blocked connection response: %v", err)
		return
	}
	var dummyBuffer [1024]byte
	var readingErr error
	for readingErr == nil {
		_, readingErr = conn.Read(dummyBuffer[:])
	}
	// this is not a normal - usually we want to wait for the HTTP handler to give the response; however, it seems that we're either getting requests faster than the
	// http handler can handle, or getting requests that fails before the header retrieval is complete.
	// in this case, we want to send our response right away and disconnect. If the client is currently still sending it's request, it might not know how to handle
	// this correctly. This use case is similar to the issue handled by the go-server in the same manner. ( see "431 Request Header Fields Too Large" in the server.go )
	// 이것은 정상이 아닙니다. 일반적으로 HTTP 핸들러가 응답을 줄 때까지 기다리기를 원합니다. 그러나 우리는 보다 빨리 요청을 받고있는 것 같습니다
	// http 핸들러는 헤더 검색이 완료되기 전에 실패한 요청을 처리하거나 받을 수 있습니다.
	// 이 경우 우리는 즉시 응답을 보내고 연결을 끊고 싶습니다. 클라이언트가 현재 요청을 계속 보내고 있는 경우 처리 방법을 모를 수 있습니다.
	// 이것은 올바르게. 이 사용 사례는 동일한 방식으로 go-server에서 처리되는 문제와 유사합니다. (server.go의 "431 요청 헤더 필드가 너무 큼" 참조)
	_, err = conn.Write([]byte(
		fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n%s: %d\r\n\r\n", http.StatusTooManyRequests, http.StatusText(http.StatusTooManyRequests), TooManyRequestsRetryAfterHeader, rt.config.ConnectionsRateLimitingWindowSeconds)))
	if err != nil {
		rt.log.With("connection", "tcp").Debugf("Failed to write response to a blocked connection response: %v", err)
		return
	}
}

// pruneAcceptedConnections clean stale items form the acceptedConnections map; it's syncornized via the acceptedConnectionsMu mutex which is expected to be taken by the caller.
// in case the created is 0, the pruning is disabled for this connection. The HTTP handlers would call Close to have this entry cleared out.
// pruneAcceptedConnections는 acceptedConnections 맵에서 오래된 항목을 정리합니다. 호출자가 가져갈 것으로 예상되는 acceptedConnectionsMu 뮤텍스를 통해 동기화됩니다.
// 생성된 값이 0인 경우 이 연결에 대해 가지 치기가 비활성화됩니다. HTTP 처리기는 이 항목을 지우기 위해 Close를 호출합니다.
func (rt *RequestTracker) pruneAcceptedConnections(pruneStartDate time.Time) {
	localAddrToRemove := []net.Addr{}
	for localAddr, request := range rt.acceptedConnections {
		if request.noPrune == false && request.created.Before(pruneStartDate) {
			localAddrToRemove = append(localAddrToRemove, localAddr)
		}
	}
	for _, localAddr := range localAddrToRemove {
		delete(rt.acceptedConnections, localAddr)
	}
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
// 닫기는 리스너를 닫습니다.
// 차단된 모든 수락 작업은 차단 해제되고 오류를 반환합니다.
func (rt *RequestTracker) Close() error {
	return rt.listener.Close()
}

func (rt *RequestTracker) getWaitUntilNoConnectionsChannel(checkInterval time.Duration) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		checkEmpty := func(rt *RequestTracker) bool {
			rt.httpConnectionsMu.Lock()
			defer rt.httpConnectionsMu.Unlock()
			return len(rt.httpConnections) == 0
		}

		for true {
			if checkEmpty(rt) {
				close(done)
				return
			}

			time.Sleep(checkInterval)
		}
	}()

	return done
}

// Addr returns the listener's network address.
// Addr은 리스너의 네트워크 주소를 반환합니다.
func (rt *RequestTracker) Addr() net.Addr {
	return rt.listener.Addr()
}

// Listener initialize the underlaying listener, and return the request tracker wrapping listener
// 리스너는 기본 리스너를 초기화하고 리스너를 래핑하는 요청 추적기를 반환합니다.
func (rt *RequestTracker) Listener(listener net.Listener) net.Listener {
	rt.listener = listener
	return rt
}

// GetTrackedRequest return the tracked request
// GetTrackedRequest는 추적된 요청을 반환합니다.
func (rt *RequestTracker) GetTrackedRequest(request *http.Request) (trackedRequest *TrackerRequest) {
	rt.httpConnectionsMu.Lock()
	defer rt.httpConnectionsMu.Unlock()
	localAddr := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return rt.httpConnections[localAddr]
}

// GetRequestConnection return the underlying connection for the given request
// GetRequestConnection은 주어진 요청에 대한 기본 연결을 반환합니다.
func (rt *RequestTracker) GetRequestConnection(request *http.Request) net.Conn {
	rt.httpConnectionsMu.Lock()
	defer rt.httpConnectionsMu.Unlock()
	localAddr := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return rt.httpConnections[localAddr].connection
}

func (rt *RequestTracker) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	// this function is called only after we've fetched all the headers. on some malicious clients, this could get delayed, so we can't rely on the tcp-connection established time to align with current time.
	// 이 함수는 모든 헤더를 가져온 후에만 호출됩니다. 일부 악의적인 클라이언트에서는 이 작업이 지연될 수 있으므로 현재 시간과 맞추기 위해 tcp-connection 설정 시간에 의존할 수 없습니다.
	rateLimitingWindowStartTime := time.Now().Add(-time.Duration(rt.config.ConnectionsRateLimitingWindowSeconds) * time.Second)

	// get the connection local address. Note that it's the interface of a immutable object, so it will be unique and matching the original connection interface.
	// 연결 로컬 주소를 얻습니다. 이는 변경할 수 없는 개체의 인터페이스이므로 고유하고 원래 연결 인터페이스와 일치합니다.
	localAddr := request.Context().Value(http.LocalAddrContextKey).(net.Addr)

	rt.hostRequestsMu.Lock()
	trackedRequest := rt.acceptedConnections[localAddr]
	if trackedRequest != nil {
		// update the original tracker request so that it won't get pruned.
		// 제거되지 않도록 원래 추적기 요청을 업데이트합니다.
		if trackedRequest.noPrune == false {
			trackedRequest.noPrune = true
			rt.hostRequests.convertToAdditionalRequest(trackedRequest)
		}
		// create a copy, so we can unlock
		// 잠금을 해제할 수 있도록 복사본을 만듭니다.
		trackedRequest = makeTrackerRequest(trackedRequest.remoteAddr, trackedRequest.remoteHost, trackedRequest.remotePort, trackedRequest.created, trackedRequest.connection)
	}
	rt.hostRequestsMu.Unlock()

	// we have no request tracker ? no problem; create one on the fly.
	// 요청 추적기가 없습니까? 문제 없어요; 즉석에서 하나를 만듭니다.
	if trackedRequest == nil {
		trackedRequest = makeTrackerRequest(request.RemoteAddr, "", "", time.Now(), nil)
	}

	// update the origin address.
	rt.updateRequestRemoteAddr(trackedRequest, request)

	rt.httpConnectionsMu.Lock()
	trackedRequest.request = request
	trackedRequest.otherTelemetryGUID, trackedRequest.otherInstanceName, trackedRequest.otherPublicAddr = getCommonHeaders(request.Header)
	rt.httpHostRequests.addRequest(trackedRequest)
	rt.httpHostRequests.pruneRequests(rateLimitingWindowStartTime)
	originConnections := rt.httpHostRequests.countOriginConnections(trackedRequest.remoteHost, rateLimitingWindowStartTime)
	rt.httpConnections[localAddr] = trackedRequest
	rt.httpConnectionsMu.Unlock()

	defer func() {
		rt.httpConnectionsMu.Lock()
		defer rt.httpConnectionsMu.Unlock()
		// now that we're done with it, we can remove the trackedRequest from the httpConnections.
		// 이제 완료되었으므로 httpConnections에서 trackedRequest를 제거할 수 있습니다.
		delete(rt.httpConnections, localAddr)
	}()

	rateLimitedRemoteHost := (!rt.config.DisableLocalhostConnectionRateLimit) || (!isLocalhost(trackedRequest.remoteHost))
	connectionLimitEnabled := rt.config.ConnectionsRateLimitingWindowSeconds > 0 && rt.config.ConnectionsRateLimitingCount > 0

	if originConnections > rt.config.ConnectionsRateLimitingCount && connectionLimitEnabled && rateLimitedRemoteHost {
		networkConnectionsDroppedTotal.Inc(map[string]string{"reason": "incoming_connection_per_ip_rate_limit"})
		rt.log.With("connection", "http").With("count", originConnections).Debugf("Rejected connection due to excessive connections attempt rate")
		rt.log.EventWithDetails(telemetryspec.Network, telemetryspec.ConnectPeerFailEvent,
			telemetryspec.ConnectPeerFailEventDetails{
				Address:      trackedRequest.remoteHost,
				HostName:     trackedRequest.otherTelemetryGUID,
				Incoming:     true,
				InstanceName: trackedRequest.otherInstanceName,
				Reason:       "Remote IP Connection Rate Limit",
			})
		response.Header().Add(TooManyRequestsRetryAfterHeader, fmt.Sprintf("%d", rt.config.ConnectionsRateLimitingWindowSeconds))
		response.WriteHeader(http.StatusTooManyRequests)
		return
	}

	// send the request downstream; in our case, it would go to the router.
	// 요청을 다운스트림으로 보냅니다. 우리의 경우 라우터로 이동합니다.
	rt.downstreamHandler.ServeHTTP(response, request)

}

// updateRequestRemoteAddr updates the origin IP address in both the trackedRequest as well as in the request.RemoteAddr string
// updateRequestRemoteAddr은 trackedRequest와 request.RemoteAddr 문자열 모두에서 원본 IP 주소를 업데이트합니다.
func (rt *RequestTracker) updateRequestRemoteAddr(trackedRequest *TrackerRequest, request *http.Request) {
	originIP := rt.getForwardedConnectionAddress(request.Header)
	if originIP == nil {
		return
	}
	request.RemoteAddr = originIP.String() + ":" + trackedRequest.remotePort
	trackedRequest.remoteHost = originIP.String()
}

// retrieve the origin ip address from the http header, if such exists and it's a valid ip address.
// 존재하고 유효한 IP 주소인 경우 http 헤더에서 원본 IP 주소를 검색합니다.
func (rt *RequestTracker) getForwardedConnectionAddress(header http.Header) (ip net.IP) {
	if rt.config.UseXForwardedForAddressField == "" {
		return
	}
	forwardedForString := header.Get(rt.config.UseXForwardedForAddressField)
	if forwardedForString == "" {
		rt.httpConnectionsMu.Lock()
		defer rt.httpConnectionsMu.Unlock()
		if !rt.misconfiguredUseForwardedForAddress {
			rt.log.Warnf("UseForwardedForAddressField is configured as '%s', but no value was retrieved from header", rt.config.UseXForwardedForAddressField)
			rt.misconfiguredUseForwardedForAddress = true
		}
		return
	}
	ip = net.ParseIP(forwardedForString)
	if ip == nil {
		// if origin isn't a valid IP Address, log this.,
		// 출처가 유효한 IP 주소가 아니면 이것을 기록합니다.,
		rt.log.Warnf("unable to parse origin address: '%s'", forwardedForString)
	}
	return
}

// isLocalhost returns true if the given host is a localhost address.
// isLocalhost는 주어진 호스트가 localhost 주소이면 true를 반환합니다.
func isLocalhost(host string) bool {
	for _, v := range []string{"localhost", "127.0.0.1", "[::1]", "::1", "[::]"} {
		if host == v {
			return true
		}
	}
	return false
}
