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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/test/partitiontest"
	"github.com/Orca18/novarand/util"
	"github.com/Orca18/novarand/util/metrics"
)

const sendBufferLength = 1000

func TestMain(m *testing.M) {
	logging.Base().SetLevel(logging.Debug)
	os.Exit(m.Run())
}

func debugMetrics(t *testing.T) {
	if t.Failed() {
		var buf strings.Builder
		metrics.DefaultRegistry().WriteMetrics(&buf, "")
		t.Log(buf.String())
	}
}

type emptyPhonebook struct{}

func (e *emptyPhonebook) GetAddresses(n int) []string {
	return []string{}
}

func (e *emptyPhonebook) UpdateRetryAfter(addr string, retryAfter time.Time) {
}

var emptyPhonebookSingleton = &emptyPhonebook{}

type oneEntryPhonebook struct {
	addr       string
	retryAfter time.Time
}

func (e *oneEntryPhonebook) GetAddresses(n int) []string {
	return []string{e.addr}
}

func (e *oneEntryPhonebook) UpdateRetryAfter(addr string, retryAfter time.Time) {
	if e.addr == addr {
		e.retryAfter = retryAfter
	}
}

func (e *oneEntryPhonebook) GetConnectionWaitTime(addr string) (addrInPhonebook bool,
	waitTime time.Duration, provisionalTime time.Time) {
	var t time.Time
	return false, 0, t
}

func (e *oneEntryPhonebook) UpdateConnectionTime(addr string, t time.Time) bool {
	return false
}

var defaultConfig config.Local

func init() {
	defaultConfig = config.GetDefaultLocal()
	defaultConfig.Archival = false
	defaultConfig.GossipFanout = 4
	defaultConfig.NetAddress = "127.0.0.1:0"
	defaultConfig.BaseLoggerDebugLevel = uint32(logging.Debug)
	defaultConfig.DNSBootstrapID = ""
	defaultConfig.MaxConnectionsPerIP = 30
}

func makeTestWebsocketNodeWithConfig(t testing.TB, conf config.Local) *WebsocketNetwork {
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(conf.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:       log,
		config:    conf,
		phonebook: MakePhonebook(1, 1*time.Millisecond),
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second
	return wn
}

func makeTestWebsocketNode(t testing.TB) *WebsocketNetwork {
	return makeTestWebsocketNodeWithConfig(t, defaultConfig)
}

type messageCounterHandler struct {
	target  int
	limit   int
	count   int
	lock    deadlock.Mutex
	done    chan struct{}
	t       testing.TB
	action  ForwardingPolicy
	verbose bool

	// For deterministically simulating slow handlers, block until test code says to go.
	release    sync.Cond
	shouldWait int32
	waitcount  int
}

func (mch *messageCounterHandler) Handle(message IncomingMessage) OutgoingMessage {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	if mch.verbose && len(message.Data) == 8 {
		now := time.Now().UnixNano()
		sent := int64(binary.LittleEndian.Uint64(message.Data))
		dnanos := now - sent
		mch.t.Logf("msg trans time %dns", dnanos)
	}
	if atomic.LoadInt32(&mch.shouldWait) > 0 {
		mch.waitcount++
		mch.release.Wait()
		mch.waitcount--
	}
	mch.count++
	//mch.t.Logf("msg %d %#v", mch.count, message)
	if mch.target != 0 && mch.done != nil && mch.count >= mch.target {
		//mch.t.Log("mch target")
		close(mch.done)
		mch.done = nil
	}
	if mch.limit > 0 && mch.done != nil && mch.count > mch.limit {
		close(mch.done)
		mch.done = nil
	}
	return OutgoingMessage{Action: mch.action}
}

func (mch *messageCounterHandler) numWaiters() int {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	return mch.waitcount
}
func (mch *messageCounterHandler) Count() int {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	return mch.count
}
func (mch *messageCounterHandler) Signal() {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	mch.release.Signal()
}
func (mch *messageCounterHandler) Broadcast() {
	mch.lock.Lock()
	defer mch.lock.Unlock()
	mch.release.Broadcast()
}

func newMessageCounter(t testing.TB, target int) *messageCounterHandler {
	return &messageCounterHandler{target: target, done: make(chan struct{}), t: t}
}

func TestWebsocketNetworkStartStop(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.Start()
	netA.Stop()
}

func waitReady(t testing.TB, wn *WebsocketNetwork, timeout <-chan time.Time) bool {
	select {
	case <-wn.Ready():
		return true
	case <-timeout:
		_, file, line, _ := runtime.Caller(1)
		t.Fatalf("%s:%d timeout waiting for ready", file, line)
		return false
	}
}

// Set up two nodes, test that a.Broadcast is received by B
// 두 개의 노드를 설정하고 a.Broadcast가 B에서 수신되는지 테스트합니다.
func TestWebsocketNetworkBasic(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	netA.Broadcast(context.Background(), protocol.TxnTag, []byte("foo"), false, nil)
	netA.Broadcast(context.Background(), protocol.TxnTag, []byte("bar"), false, nil)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

// Repeat basic, but test a unicast
// 기본을 반복하지만 유니캐스트를 테스트합니다.
func TestWebsocketNetworkUnicast(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	require.Equal(t, 1, len(netA.peers))
	require.Equal(t, 1, len(netA.GetPeers(PeersConnectedIn)))
	peerB := netA.peers[0]
	err := peerB.Unicast(context.Background(), []byte("foo"), protocol.TxnTag)
	assert.NoError(t, err)
	err = peerB.Unicast(context.Background(), []byte("bar"), protocol.TxnTag)
	assert.NoError(t, err)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

// Like a basic test, but really we just want to have SetPeerData()/GetPeerData()
// 기본 테스트와 비슷하지만 실제로는 SetPeerData()/GetPeerData()가 필요합니다.
func TestWebsocketPeerData(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	require.Equal(t, 1, len(netA.peers))
	require.Equal(t, 1, len(netA.GetPeers(PeersConnectedIn)))
	peerB := netA.peers[0]

	require.Equal(t, nil, netA.GetPeerData(peerB, "not there"))
	netA.SetPeerData(peerB, "foo", "bar")
	require.Equal(t, "bar", netA.GetPeerData(peerB, "foo"))
	netA.SetPeerData(peerB, "foo", "qux")
	require.Equal(t, "qux", netA.GetPeerData(peerB, "foo"))
	netA.SetPeerData(peerB, "foo", nil)
	require.Equal(t, nil, netA.GetPeerData(peerB, "foo"))
}

// Test sending array of messages
// 메시지 배열을 보내는 테스트
func TestWebsocketNetworkArray(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 3)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	tags := []protocol.Tag{protocol.TxnTag, protocol.TxnTag, protocol.TxnTag}
	data := [][]byte{[]byte("foo"), []byte("bar"), []byte("algo")}
	netA.BroadcastArray(context.Background(), tags, data, false, nil)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

// Test cancelling message sends
// 테스트 취소 메시지 전송
func TestWebsocketNetworkCancel(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 100)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	tags := make([]protocol.Tag, 100)
	data := make([][]byte, 100)
	for i := range data {
		tags[i] = protocol.TxnTag
		data[i] = []byte(string(rune(i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// try calling BroadcastArray
	netA.BroadcastArray(ctx, tags, data, true, nil)

	select {
	case <-counterDone:
		t.Errorf("All messages were sent, send not cancelled")
	case <-time.After(2 * time.Second):
	}
	assert.Equal(t, 0, counter.Count())

	// try calling innerBroadcast
	request := broadcastRequest{tags: tags, data: data, enqueueTime: time.Now(), ctx: ctx}
	peers, _ := netA.peerSnapshot([]*wsPeer{})
	netA.innerBroadcast(request, true, peers)

	select {
	case <-counterDone:
		t.Errorf("All messages were sent, send not cancelled")
	case <-time.After(2 * time.Second):
	}
	assert.Equal(t, 0, counter.Count())

	// try calling writeLoopSend
	msgs := make([]sendMessage, 0, len(data))
	enqueueTime := time.Now()
	for i, msg := range data {
		tbytes := []byte(tags[i])
		mbytes := make([]byte, len(tbytes)+len(msg))
		copy(mbytes, tbytes)
		copy(mbytes[len(tbytes):], msg)
		msgs = append(msgs, sendMessage{data: mbytes, enqueued: time.Now(), peerEnqueued: enqueueTime, hash: crypto.Hash(mbytes), ctx: context.Background()})
	}

	msgs[50].ctx = ctx

	for _, peer := range peers {
		peer.sendBufferHighPrio <- sendMessages{msgs}
	}

	select {
	case <-counterDone:
		t.Errorf("All messages were sent, send not cancelled")
	case <-time.After(2 * time.Second):
	}
	assert.Equal(t, 50, counter.Count())
}

// Set up two nodes, test that a.Broadcast is received by B, when B has no address.
// 두 개의 노드를 설정하고 B에 주소가 없을 때 B가 a.Broadcast를 수신하는지 테스트합니다.
func TestWebsocketNetworkNoAddress(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 2)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	netA.Broadcast(context.Background(), protocol.TxnTag, []byte("foo"), false, nil)
	netA.Broadcast(context.Background(), protocol.TxnTag, []byte("bar"), false, nil)

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		t.Errorf("timeout, count=%d, wanted 2", counter.count)
	}
}

func lineNetwork(t *testing.T, numNodes int) (nodes []*WebsocketNetwork, counters []messageCounterHandler) {
	nodes = make([]*WebsocketNetwork, numNodes)
	counters = make([]messageCounterHandler, numNodes)
	for i := range nodes {
		nodes[i] = makeTestWebsocketNode(t)
		nodes[i].log = nodes[i].log.With("node", i)
		nodes[i].config.GossipFanout = 2
		if i == 0 || i == len(nodes)-1 {
			nodes[i].config.GossipFanout = 1
		}
		if i > 0 {
			addrPrev, postListen := nodes[i-1].Address()
			require.True(t, postListen)
			nodes[i].phonebook.ReplacePeerList([]string{addrPrev}, "default", PhoneBookEntryRelayRole)
			nodes[i].RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: &counters[i]}})
		}
		nodes[i].Start()
		counters[i].t = t
		counters[i].action = Broadcast
	}
	return
}

func closeNodeWG(node *WebsocketNetwork, wg *sync.WaitGroup) {
	node.Stop()
	wg.Done()
}

func closeNodes(nodes []*WebsocketNetwork) {
	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	for _, node := range nodes {
		go closeNodeWG(node, &wg)
	}
	wg.Wait()
}

func waitNodesReady(t *testing.T, nodes []*WebsocketNetwork, timeout time.Duration) {
	tc := time.After(timeout)
	for i, node := range nodes {
		select {
		case <-node.Ready():
		case <-tc:
			t.Fatalf("node[%d] not ready at timeout", i)
		}
	}
}

const lineNetworkLength = 20
const lineNetworkNumMessages = 5

// Set up a network where each node connects to the previous; test that .Broadcast from one end gets to the other.
// Bonus! Measure how long that takes.
// TODO: also make a Benchmark version of this that reports per-node broadcast hop speed.
// 각 노드가 이전 노드에 연결되는 네트워크를 설정합니다. .Broadcast가 한쪽 끝에서 다른 쪽 끝으로 전달되는지 테스트합니다.
// 보너스! 얼마나 오래 걸리는지 측정하십시오.
// TODO: 노드당 브로드캐스트 홉 속도를 보고하는 이것의 벤치마크 버전도 만듭니다.
func TestLineNetwork(t *testing.T) {
	partitiontest.PartitionTest(t)

	nodes, counters := lineNetwork(t, lineNetworkLength)
	t.Logf("line network length: %d", lineNetworkLength)
	waitNodesReady(t, nodes, 2*time.Second)
	t.Log("ready")
	defer closeNodes(nodes)
	counter := &counters[len(counters)-1]
	counter.target = lineNetworkNumMessages
	counter.done = make(chan struct{})
	counterDone := counter.done
	counter.verbose = true
	for i := 0; i < lineNetworkNumMessages; i++ {
		sendTime := time.Now().UnixNano()
		var timeblob [8]byte
		binary.LittleEndian.PutUint64(timeblob[:], uint64(sendTime))
		nodes[0].Broadcast(context.Background(), protocol.TxnTag, timeblob[:], true, nil)
	}
	select {
	case <-counterDone:
	case <-time.After(20 * time.Second):
		t.Errorf("timeout, count=%d, wanted %d", counter.Count(), lineNetworkNumMessages)
		for ci := range counters {
			t.Errorf("count[%d]=%d", ci, counters[ci].Count())
		}
	}
	debugMetrics(t)
}

func addrtest(t *testing.T, wn *WebsocketNetwork, expected, src string) {
	actual, err := wn.addrToGossipAddr(src)
	require.NoError(t, err)
	assert.Equal(t, expected, actual)
}

func TestAddrToGossipAddr(t *testing.T) {
	partitiontest.PartitionTest(t)

	wn := &WebsocketNetwork{}
	wn.GenesisID = "test genesisID"
	wn.log = logging.Base()
	addrtest(t, wn, "ws://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "r7.algodev.network.:4166")
	addrtest(t, wn, "ws://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "http://r7.algodev.network.:4166")
	addrtest(t, wn, "wss://r7.algodev.network.:4166/v1/test%20genesisID/gossip", "https://r7.algodev.network.:4166")
}

type nopConn struct{}

func (nc *nopConn) RemoteAddr() net.Addr {
	return nil
}
func (nc *nopConn) NextReader() (int, io.Reader, error) {
	return 0, nil, nil
}
func (nc *nopConn) WriteMessage(int, []byte) error {
	return nil
}
func (nc *nopConn) WriteControl(int, []byte, time.Time) error {
	return nil
}
func (nc *nopConn) SetReadLimit(limit int64) {
}
func (nc *nopConn) CloseWithoutFlush() error {
	return nil
}
func (nc *nopConn) SetPingHandler(h func(appData string) error) {

}
func (nc *nopConn) SetPongHandler(h func(appData string) error) {

}

var nopConnSingleton = nopConn{}

// What happens when all the read message handler threads get busy?
// 모든 읽기 메시지 핸들러 스레드가 사용 중이면 어떻게 됩니까?
func TestSlowHandlers(t *testing.T) {
	partitiontest.PartitionTest(t)

	slowTag := protocol.Tag("sl")
	fastTag := protocol.Tag("fa")
	slowCounter := messageCounterHandler{shouldWait: 1}
	slowCounter.release.L = &slowCounter.lock
	fastCounter := messageCounterHandler{target: incomingThreads}
	fastCounter.done = make(chan struct{})
	fastCounterDone := fastCounter.done
	slowHandler := TaggedMessageHandler{Tag: slowTag, MessageHandler: &slowCounter}
	fastHandler := TaggedMessageHandler{Tag: fastTag, MessageHandler: &fastCounter}
	node := makeTestWebsocketNode(t)
	node.RegisterHandlers([]TaggedMessageHandler{slowHandler, fastHandler})
	node.Start()
	defer node.Stop()
	injectionPeers := make([]wsPeer, incomingThreads*2)
	for i := range injectionPeers {
		injectionPeers[i].closing = make(chan struct{})
		injectionPeers[i].net = node
		injectionPeers[i].conn = &nopConnSingleton
		node.addPeer(&injectionPeers[i])
	}
	ipi := 0
	// start slow handler calls that will block all handler threads
	// 모든 핸들러 스레드를 차단하는 느린 핸들러 호출 시작
	for i := 0; i < incomingThreads; i++ {
		data := []byte{byte(i)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: slowTag, Data: data, Net: node}
		ipi++
	}
	defer slowCounter.Broadcast()

	// start fast handler calls that won't get to run
	// 실행되지 않는 빠른 처리기 호출을 시작합니다.
	for i := 0; i < incomingThreads; i++ {
		data := []byte{byte(i)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: fastTag, Data: data, Net: node}
		ipi++
	}
	ok := false
	lastnw := -1
	totalWait := 0
	for i := 0; i < 7; i++ {
		waitTime := int(1 << uint64(i))
		time.Sleep(time.Duration(waitTime) * time.Millisecond)
		totalWait += waitTime
		nw := slowCounter.numWaiters()
		if nw == incomingThreads {
			ok = true
			break
		}
		if lastnw != nw {
			t.Logf("%dms %d waiting", totalWait, nw)
			lastnw = nw
		}
	}
	if !ok {
		t.Errorf("timeout waiting for %d threads to block on slow handler, have %d", incomingThreads, lastnw)
	}
	require.Equal(t, 0, fastCounter.Count())

	// release one slow request, all the other requests should process on that one handler thread
	// 하나의 느린 요청을 해제하고 다른 모든 요청은 해당 하나의 핸들러 스레드에서 처리해야 합니다.
	slowCounter.Signal()

	select {
	case <-fastCounterDone:
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for %d blocked events to be handled, have %d", incomingThreads, fastCounter.Count())
	}
	// checks that above .Signal() did in fact release just one waiting slow handler
	// 위의 .Signal()이 실제로 대기 중인 느린 처리기를 하나만 해제했는지 확인합니다.
	require.Equal(t, 1, slowCounter.Count())

	// we don't care about counting how things finish
	// 우리는 일이 어떻게 끝나는지 계산하는 데 신경 쓰지 않습니다.
	debugMetrics(t)
}

// one peer sends waaaayy too much slow-to-handle traffic. everything else should run fine.
// 한 피어가 처리 속도가 느린 트래픽을 너무 많이 보냅니다. 다른 모든 것은 잘 실행되어야 합니다.
func TestFloodingPeer(t *testing.T) {
	partitiontest.PartitionTest(t)

	t.Skip("flaky test")
	slowTag := protocol.Tag("sl")
	fastTag := protocol.Tag("fa")
	slowCounter := messageCounterHandler{shouldWait: 1}
	slowCounter.release.L = &slowCounter.lock
	fastCounter := messageCounterHandler{}
	slowHandler := TaggedMessageHandler{Tag: slowTag, MessageHandler: &slowCounter}
	fastHandler := TaggedMessageHandler{Tag: fastTag, MessageHandler: &fastCounter}
	node := makeTestWebsocketNode(t)
	node.RegisterHandlers([]TaggedMessageHandler{slowHandler, fastHandler})
	node.Start()
	defer node.Stop()
	injectionPeers := make([]wsPeer, incomingThreads*2)
	for i := range injectionPeers {
		injectionPeers[i].closing = make(chan struct{})
		injectionPeers[i].net = node
		injectionPeers[i].conn = &nopConnSingleton
		node.addPeer(&injectionPeers[i])
	}
	ipi := 0
	const numBadPeers = 1
	// start slow handler calls that will block some threads
	// 일부 스레드를 차단하는 느린 처리기 호출을 시작합니다.
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < numBadPeers; i++ {
		myI := i
		myIpi := ipi
		go func() {
			processed := make(chan struct{}, 1)
			processed <- struct{}{}

			for qi := 0; qi < incomingThreads*2; qi++ {
				data := []byte{byte(myI), byte(qi)}
				select {
				case <-processed:
				case <-ctx.Done():
					return
				}

				select {
				case node.readBuffer <- IncomingMessage{Sender: &injectionPeers[myIpi], Tag: slowTag, Data: data, Net: node, processing: processed}:
				case <-ctx.Done():
					return
				}
			}
		}()
		ipi++
	}
	defer cancel()
	defer func() {
		t.Log("release slow handlers")
		atomic.StoreInt32(&slowCounter.shouldWait, 0)
		slowCounter.Broadcast()
	}()

	// start fast handler calls that will run on other reader threads
	// 다른 판독기 스레드에서 실행될 빠른 처리기 호출을 시작합니다.
	numFast := 0
	fastCounter.target = len(injectionPeers) - ipi
	fastCounter.done = make(chan struct{})
	fastCounterDone := fastCounter.done
	for ipi < len(injectionPeers) {
		data := []byte{byte(ipi)}
		node.readBuffer <- IncomingMessage{Sender: &injectionPeers[ipi], Tag: fastTag, Data: data, Net: node}
		numFast++
		ipi++
	}
	require.Equal(t, numFast, fastCounter.target)
	select {
	case <-fastCounterDone:
	case <-time.After(time.Second):
		t.Errorf("timeout waiting for %d fast handlers, got %d", fastCounter.target, fastCounter.Count())
	}

	// we don't care about counting how things finish
	// 우리는 일이 어떻게 끝나는지 계산하는 데 신경 쓰지 않습니다.
}

func peerIsClosed(peer *wsPeer) bool {
	return atomic.LoadInt32(&peer.didInnerClose) != 0
}

func avgSendBufferHighPrioLength(wn *WebsocketNetwork) float64 {
	wn.peersLock.Lock()
	defer wn.peersLock.Unlock()
	sum := 0
	for _, peer := range wn.peers {
		sum += len(peer.sendBufferHighPrio)
	}
	return float64(sum) / float64(len(wn.peers))
}

// TestSlowOutboundPeer tests what happens when one outbound peer is slow and the rest are fine. Current logic is to disconnect the one slow peer when its outbound channel is full.
// TestSlowOutboundPeer는 하나의 아웃바운드 피어가 느리고 나머지는 정상일 때 어떤 일이 발생하는지 테스트합니다. 현재 논리는 아웃바운드 채널이 가득 차면 느린 피어 하나의 연결을 끊는 것입니다.
// This is a deeply invasive test that reaches into the guts of WebsocketNetwork and wsPeer. If the implementation chainges consider throwing away or totally reimplementing this test.
// 이것은 WebsocketNetwork 및 wsPeer의 내장에 도달하는 매우 침습적인 테스트입니다. 구현 체인이 이 테스트를 폐기하거나 완전히 다시 구현하는 것을 고려하는 경우.
func TestSlowOutboundPeer(t *testing.T) {
	partitiontest.PartitionTest(t)

	t.Skip() // todo - update this test to reflect the new implementation.
	xtag := protocol.ProposalPayloadTag
	node := makeTestWebsocketNode(t)
	destPeers := make([]wsPeer, 5)
	for i := range destPeers {
		destPeers[i].closing = make(chan struct{})
		destPeers[i].net = node
		destPeers[i].sendBufferHighPrio = make(chan sendMessages, sendBufferLength)
		destPeers[i].sendBufferBulk = make(chan sendMessages, sendBufferLength)
		destPeers[i].conn = &nopConnSingleton
		destPeers[i].rootURL = fmt.Sprintf("fake %d", i)
		node.addPeer(&destPeers[i])
	}
	node.Start()
	tctx, cf := context.WithTimeout(context.Background(), 5*time.Second)
	for i := 0; i < sendBufferLength; i++ {
		t.Logf("broadcast %d", i)
		sent := node.Broadcast(tctx, xtag, []byte{byte(i)}, true, nil)
		require.NoError(t, sent)
	}
	cf()
	ok := false
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond)
		aoql := avgSendBufferHighPrioLength(node)
		if aoql == sendBufferLength {
			ok = true
			break
		}
		t.Logf("node.avgOutboundQueueLength() %f", aoql)
	}
	require.True(t, ok)
	for p := range destPeers {
		if p == 0 {
			continue
		}
		for j := 0; j < sendBufferLength; j++ {
			// throw away a message as if sent
			<-destPeers[p].sendBufferHighPrio
		}
	}
	aoql := avgSendBufferHighPrioLength(node)
	if aoql > (sendBufferLength / 2) {
		t.Fatalf("avgOutboundQueueLength=%f wanted <%f", aoql, sendBufferLength/2.0)
		return
	}
	// it shouldn't have closed for just sitting on the limit of full
	// 전체 제한에 앉아 있기 때문에 닫히지 않아야 합니다.
	require.False(t, peerIsClosed(&destPeers[0]))

	// function context just to contain defer cf()
	// defer cf()를 포함하기 위한 함수 컨텍스트
	func() {
		timeout, cf := context.WithTimeout(context.Background(), time.Second)
		defer cf()
		sent := node.Broadcast(timeout, xtag, []byte{byte(42)}, true, nil)
		assert.NoError(t, sent)
	}()

	// and now with the rest of the peers well and this one slow, we closed the slow one
	// 이제 나머지 피어는 잘되고 이것은 느린 것으로, 느린 것을 닫았습니다.
	require.True(t, peerIsClosed(&destPeers[0]))
}

func makeTestFilterWebsocketNode(t *testing.T, nodename string) *WebsocketNetwork {
	dc := defaultConfig
	dc.EnableIncomingMessageFilter = true
	dc.EnableOutgoingNetworkMessageFiltering = true
	dc.IncomingMessageFilterBucketCount = 5
	dc.IncomingMessageFilterBucketSize = 512
	dc.OutgoingMessageFilterBucketCount = 3
	dc.OutgoingMessageFilterBucketSize = 128
	wn := &WebsocketNetwork{
		log:       logging.TestingLog(t).With("node", nodename),
		config:    dc,
		phonebook: MakePhonebook(1, 1*time.Millisecond),
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	require.True(t, wn.config.EnableIncomingMessageFilter)
	wn.setup()
	wn.eventualReadyDelay = time.Second
	require.True(t, wn.config.EnableIncomingMessageFilter)
	return wn
}

func TestDupFilter(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestFilterWebsocketNode(t, "a")
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestFilterWebsocketNode(t, "b")
	netB.config.GossipFanout = 2
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := &messageCounterHandler{t: t, limit: 1, done: make(chan struct{})}
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.AgreementVoteTag, MessageHandler: counter}})
	debugTag2 := protocol.ProposalPayloadTag
	counter2 := &messageCounterHandler{t: t, limit: 1, done: make(chan struct{})}
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: debugTag2, MessageHandler: counter2}})

	addrB, postListen := netB.Address()
	require.True(t, postListen)
	netC := makeTestFilterWebsocketNode(t, "c")
	netC.config.GossipFanout = 1
	netC.phonebook.ReplacePeerList([]string{addrB}, "default", PhoneBookEntryRelayRole)
	netC.Start()
	defer netC.Stop()

	msg := make([]byte, messageFilterSize+1)
	rand.Read(msg)

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")
	waitReady(t, netC, readyTimeout.C)
	t.Log("c ready")

	// TODO: this test has two halves that exercise inbound de-dup and outbound non-send due to received hash. But it doesn't properly _test_ them as it doesn't measure _why_ it receives each message exactly once. The second half below could actually be because of the same inbound de-dup as this first half. You can see the actions of either in metrics.
	// algod_network_duplicate_message_received_total{} 2
	// algod_outgoing_network_message_filtered_out_total{} 2
	// Maybe we should just .Set(0) those counters and use them in this test?
	// TODO: 이 테스트에는 수신된 해시로 인해 인바운드 중복 제거 및 아웃바운드 미전송을 실행하는 두 개의 절반이 있습니다. 그러나 각 메시지를 정확히 한 번 수신하는 _이유_를 측정하지 않기 때문에 적절하게 _테스트_하지 않습니다. 아래의 후반부는 실제로 이 전반부와 동일한 인바운드 중복 제거 때문일 수 있습니다. 메트릭에서 둘 중 하나의 작업을 볼 수 있습니다.
	// algod_network_duplicate_message_received_total{} 2
	// algod_outgoing_network_message_filtered_out_total{} 2
	// 이 카운터를 .Set(0)하고 이 테스트에서 사용해야 할까요?

	// This exercise inbound dup detection.
	// 인바운드 중복 감지를 실행합니다.
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	netA.Broadcast(context.Background(), protocol.AgreementVoteTag, msg, true, nil)
	t.Log("A dup send done")

	select {
	case <-counter.done:
		// probably a failure, but let it fall through to the equal check
		// 아마도 실패할 수 있지만 동등 검사로 넘어갑니다.
	case <-time.After(time.Second):
	}
	counter.lock.Lock()
	assert.Equal(t, 1, counter.count)
	counter.lock.Unlock()

	// new message
	rand.Read(msg)
	t.Log("A send, C non-dup-send")
	netA.Broadcast(context.Background(), debugTag2, msg, true, nil)
	// B should broadcast its non-desire to receive the message again
	// B는 메시지를 다시 수신하기를 원하지 않음을 브로드캐스트해야 합니다.
	time.Sleep(500 * time.Millisecond)

	// C should now not send these
	netC.Broadcast(context.Background(), debugTag2, msg, true, nil)
	netC.Broadcast(context.Background(), debugTag2, msg, true, nil)

	select {
	case <-counter2.done:
		// probably a failure, but let it fall through to the equal check
		// 아마도 실패할 수 있지만 동등 검사로 넘어갑니다.
	case <-time.After(time.Second):
	}
	assert.Equal(t, 1, counter2.count)

	debugMetrics(t)
}

func TestGetPeers(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer netA.Stop()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	phbMulti := MakePhonebook(1, 1*time.Millisecond)
	phbMulti.ReplacePeerList([]string{addrA}, "phba", PhoneBookEntryRelayRole)
	netB.phonebook = phbMulti
	netB.Start()
	defer netB.Stop()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	phbMulti.ReplacePeerList([]string{"a", "b", "c"}, "ph", PhoneBookEntryRelayRole)

	//addrB, _ := netB.Address()

	// A has only an inbound connection from B
	// A는 B로부터의 인바운드 연결만 가지고 있습니다.
	aPeers := netA.GetPeers(PeersConnectedOut)
	assert.Equal(t, 0, len(aPeers))

	// B's connection to A is outgoing
	// B와 A의 연결이 나가는 중입니다.
	bPeers := netB.GetPeers(PeersConnectedOut)
	assert.Equal(t, 1, len(bPeers))
	assert.Equal(t, addrA, bPeers[0].(HTTPPeer).GetAddress())

	// B also knows about other peers not connected to
	// B는 또한 연결되지 않은 다른 피어에 대해서도 알고 있습니다.
	bPeers = netB.GetPeers(PeersPhonebookRelays)
	assert.Equal(t, 4, len(bPeers))
	peerAddrs := make([]string, len(bPeers))
	for pi, peer := range bPeers {
		peerAddrs[pi] = peer.(HTTPPeer).GetAddress()
	}
	sort.Strings(peerAddrs)
	expectAddrs := []string{addrA, "a", "b", "c"}
	sort.Strings(expectAddrs)
	assert.Equal(t, expectAddrs, peerAddrs)
}

type benchmarkHandler struct {
	returns chan uint64
}

func (bh *benchmarkHandler) Handle(message IncomingMessage) OutgoingMessage {
	i := binary.LittleEndian.Uint64(message.Data)
	bh.returns <- i
	return OutgoingMessage{}
}

// Set up two nodes, test that a.Broadcast is received by B
// 두 개의 노드를 설정하고 a.Broadcast가 B에서 수신되는지 테스트합니다.
func BenchmarkWebsocketNetworkBasic(t *testing.B) {
	deadlock.Opts.Disable = true
	const msgSize = 200
	const inflight = 90
	t.Logf("%s %d", t.Name(), t.N)
	t.StopTimer()
	t.ResetTimer()
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	returns := make(chan uint64, 100)
	bhandler := benchmarkHandler{returns}
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: &bhandler}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")
	var ireturned uint64

	t.StartTimer()
	timeoutd := (time.Duration(t.N) * 100 * time.Microsecond) + (2 * time.Second)
	timeout := time.After(timeoutd)
	for i := 0; i < t.N; i++ {
		for uint64(i) > ireturned+inflight {
			select {
			case ireturned = <-returns:
			case <-timeout:
				t.Errorf("timeout in send at %d", i)
				return
			}
		}
		msg := make([]byte, msgSize)
		binary.LittleEndian.PutUint64(msg, uint64(i))
		err := netA.Broadcast(context.Background(), protocol.TxnTag, msg, true, nil)
		if err != nil {
			t.Errorf("error on broadcast: %v", err)
			return
		}
	}
	netA.Broadcast(context.Background(), protocol.Tag("-1"), []byte("derp"), true, nil)
	t.Logf("sent %d", t.N)

	for ireturned < uint64(t.N-1) {
		select {
		case ireturned = <-returns:
		case <-timeout:
			t.Errorf("timeout, count=%d, wanted %d", ireturned, t.N)
			buf := strings.Builder{}
			networkMessageReceivedTotal.WriteMetric(&buf, "")
			networkMessageSentTotal.WriteMetric(&buf, "")
			networkBroadcasts.WriteMetric(&buf, "")
			duplicateNetworkMessageReceivedTotal.WriteMetric(&buf, "")
			outgoingNetworkMessageFilteredOutTotal.WriteMetric(&buf, "")
			networkBroadcastsDropped.WriteMetric(&buf, "")
			t.Errorf(
				"a out queue=%d, metric: %s",
				len(netA.broadcastQueueBulk),
				buf.String(),
			)
			return
		}
	}
	t.StopTimer()
	t.Logf("counter done")
}

// Check that priority is propagated from B to A
// 우선순위가 B에서 A로 전파되었는지 확인
func TestWebsocketNetworkPrio(t *testing.T) {
	partitiontest.PartitionTest(t)

	prioA := netPrioStub{}
	netA := makeTestWebsocketNode(t)
	netA.SetPrioScheme(&prioA)
	netA.config.GossipFanout = 1
	netA.prioResponseChan = make(chan *wsPeer, 10)
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	prioB := netPrioStub{}
	crypto.RandBytes(prioB.addr[:])
	prioB.prio = crypto.RandUint64()
	netB := makeTestWebsocketNode(t)
	netB.SetPrioScheme(&prioB)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	// Wait for response message to propagate from B to A
	// 응답 메시지가 B에서 A로 전파될 때까지 기다립니다.
	select {
	case <-netA.prioResponseChan:
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan")
	}
	waitReady(t, netA, time.After(time.Second))

	// Peek at A's peers
	// A의 피어를 엿본다.
	netA.peersLock.RLock()
	defer netA.peersLock.RUnlock()
	require.Equal(t, len(netA.peers), 1)

	require.Equal(t, netA.peers[0].prioAddress, prioB.addr)
	require.Equal(t, netA.peers[0].prioWeight, prioB.prio)
}

// Check that priority is propagated from B to A
// 우선순위가 B에서 A로 전파되었는지 확인
func TestWebsocketNetworkPrioLimit(t *testing.T) {
	partitiontest.PartitionTest(t)

	limitConf := defaultConfig
	limitConf.BroadcastConnectionsLimit = 1

	prioA := netPrioStub{}
	netA := makeTestWebsocketNodeWithConfig(t, limitConf)
	netA.SetPrioScheme(&prioA)
	netA.config.GossipFanout = 2
	netA.prioResponseChan = make(chan *wsPeer, 10)
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	addrA, postListen := netA.Address()
	require.True(t, postListen)

	counterB := newMessageCounter(t, 1)
	counterBdone := counterB.done
	prioB := netPrioStub{}
	crypto.RandBytes(prioB.addr[:])
	prioB.prio = 100
	netB := makeTestWebsocketNode(t)
	netB.SetPrioScheme(&prioB)
	netB.config.GossipFanout = 1
	netB.config.NetAddress = ""
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counterB}})
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	counterC := newMessageCounter(t, 1)
	counterCdone := counterC.done
	prioC := netPrioStub{}
	crypto.RandBytes(prioC.addr[:])
	prioC.prio = 10
	netC := makeTestWebsocketNode(t)
	netC.SetPrioScheme(&prioC)
	netC.config.GossipFanout = 1
	netC.config.NetAddress = ""
	netC.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netC.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counterC}})
	netC.Start()
	defer func() { t.Log("stopping C"); netC.Stop(); t.Log("C done") }()

	// Wait for response messages to propagate from B+C to A
	// 응답 메시지가 B+C에서 A로 전파될 때까지 기다립니다.
	select {
	case peer := <-netA.prioResponseChan:
		netA.peersLock.RLock()
		require.Subset(t, []uint64{prioB.prio, prioC.prio}, []uint64{peer.prioWeight})
		netA.peersLock.RUnlock()
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan 1")
	}
	select {
	case peer := <-netA.prioResponseChan:
		netA.peersLock.RLock()
		require.Subset(t, []uint64{prioB.prio, prioC.prio}, []uint64{peer.prioWeight})
		netA.peersLock.RUnlock()
	case <-time.After(time.Second):
		t.Errorf("timeout on netA.prioResponseChan 2")
	}
	waitReady(t, netA, time.After(time.Second))

	firstPeer := netA.peers[0]
	netA.Broadcast(context.Background(), protocol.TxnTag, nil, true, nil)

	failed := false
	select {
	case <-counterBdone:
	case <-time.After(time.Second):
		t.Errorf("timeout, B did not receive message")
		failed = true
	}

	select {
	case <-counterCdone:
		t.Errorf("C received message")
		failed = true
	case <-time.After(time.Second):
	}

	if failed {
		t.Errorf("NetA had the following two peers priorities : [0]:%s=%d [1]:%s=%d", netA.peers[0].rootURL, netA.peers[0].prioWeight, netA.peers[1].rootURL, netA.peers[1].prioWeight)
		t.Errorf("first peer before broadcasting was %s", firstPeer.rootURL)
	}
}

// Create many idle connections, to see if we have excessive CPU utilization.
// 과도한 CPU 사용률이 있는지 확인하기 위해 많은 유휴 연결을 만듭니다.
func TestWebsocketNetworkManyIdle(t *testing.T) {
	partitiontest.PartitionTest(t)

	// This test is meant to be run manually, as:
	//
	//   IDLETEST=x go test -v . -run=ManyIdle -count=1
	//
	// and examining the reported CPU time use.

	if os.Getenv("IDLETEST") == "" {
		t.Skip("Skipping; IDLETEST not set")
	}

	deadlock.Opts.Disable = true

	numClients := 1000
	relayConf := defaultConfig
	relayConf.BaseLoggerDebugLevel = uint32(logging.Error)
	relayConf.MaxConnectionsPerIP = numClients

	relay := makeTestWebsocketNodeWithConfig(t, relayConf)
	relay.config.GossipFanout = numClients
	relay.Start()
	defer relay.Stop()
	relayAddr, postListen := relay.Address()
	require.True(t, postListen)

	clientConf := defaultConfig
	clientConf.BaseLoggerDebugLevel = uint32(logging.Error)
	clientConf.BroadcastConnectionsLimit = 0
	clientConf.NetAddress = ""

	var clients []*WebsocketNetwork
	for i := 0; i < numClients; i++ {
		client := makeTestWebsocketNodeWithConfig(t, clientConf)
		client.config.GossipFanout = 1
		client.phonebook.ReplacePeerList([]string{relayAddr}, "default", PhoneBookEntryRelayRole)
		client.Start()
		defer client.Stop()

		clients = append(clients, client)
	}

	readyTimeout := time.NewTimer(30 * time.Second)
	waitReady(t, relay, readyTimeout.C)

	for i := 0; i < numClients; i++ {
		waitReady(t, clients[i], readyTimeout.C)
	}

	var r0utime, r1utime int64
	var r0stime, r1stime int64

	r0utime, r0stime, _ = util.GetCurrentProcessTimes()
	time.Sleep(10 * time.Second)
	r1utime, r1stime, _ = util.GetCurrentProcessTimes()

	t.Logf("Background CPU use: user %v, system %v\n",
		time.Duration(r1utime-r0utime),
		time.Duration(r1stime-r0stime))
}

// TODO: test both sides of http-header setting and checking?
// TODO: test request-disconnect-reconnect?
// TODO: test server handling of various malformed clients?
// TODO? disconnect a node in the middle of a line and test that messages _don't_ get through?
// TODO: test self-connect rejection
// TODO: test funcion when some message handler is slow?
// ===============================================================
// TODO: http 헤더 설정 및 확인의 양면을 테스트하시겠습니까?
// TODO: 테스트 요청-연결 해제-재연결?
// TODO: 다양한 기형 클라이언트의 테스트 서버 처리?
// TODO? 줄 중간에 있는 노드의 연결을 끊고 메시지가 _don't_ 통과하는지 테스트하십시오.
// TODO: 자체 연결 거부 테스트
// TODO: 일부 메시지 핸들러가 느릴 때 테스트 기능?

func TestWebsocketNetwork_getCommonHeaders(t *testing.T) {
	partitiontest.PartitionTest(t)

	header := http.Header{}
	expectedTelemetryGUID := "123"
	expectedInstanceName := "456"
	expectedPublicAddr := "789"
	header.Set(TelemetryIDHeader, expectedTelemetryGUID)
	header.Set(InstanceNameHeader, expectedInstanceName)
	header.Set(AddressHeader, expectedPublicAddr)
	otherTelemetryGUID, otherInstanceName, otherPublicAddr := getCommonHeaders(header)
	require.Equal(t, expectedTelemetryGUID, otherTelemetryGUID)
	require.Equal(t, expectedInstanceName, otherInstanceName)
	require.Equal(t, expectedPublicAddr, otherPublicAddr)
}

func TestWebsocketNetwork_checkServerResponseVariables(t *testing.T) {
	partitiontest.PartitionTest(t)

	wn := makeTestWebsocketNode(t)
	wn.GenesisID = "genesis-id1"
	wn.RandomID = "random-id1"
	header := http.Header{}
	header.Set(ProtocolVersionHeader, ProtocolVersion)
	header.Set(NodeRandomHeader, wn.RandomID+"tag")
	header.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, matchingVersion := wn.checkServerResponseVariables(header, "addressX")
	require.Equal(t, true, responseVariableOk)
	require.Equal(t, matchingVersion, ProtocolVersion)

	noVersionHeader := http.Header{}
	noVersionHeader.Set(NodeRandomHeader, wn.RandomID+"tag")
	noVersionHeader.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, matchingVersion = wn.checkServerResponseVariables(noVersionHeader, "addressX")
	require.Equal(t, false, responseVariableOk)

	noRandomHeader := http.Header{}
	noRandomHeader.Set(ProtocolVersionHeader, ProtocolVersion)
	noRandomHeader.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, _ = wn.checkServerResponseVariables(noRandomHeader, "addressX")
	require.Equal(t, false, responseVariableOk)

	sameRandomHeader := http.Header{}
	sameRandomHeader.Set(ProtocolVersionHeader, ProtocolVersion)
	sameRandomHeader.Set(NodeRandomHeader, wn.RandomID)
	sameRandomHeader.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, _ = wn.checkServerResponseVariables(sameRandomHeader, "addressX")
	require.Equal(t, false, responseVariableOk)

	differentGenesisIDHeader := http.Header{}
	differentGenesisIDHeader.Set(ProtocolVersionHeader, ProtocolVersion)
	differentGenesisIDHeader.Set(NodeRandomHeader, wn.RandomID+"tag")
	differentGenesisIDHeader.Set(GenesisHeader, wn.GenesisID+"tag")
	responseVariableOk, _ = wn.checkServerResponseVariables(differentGenesisIDHeader, "addressX")
	require.Equal(t, false, responseVariableOk)
}

func (wn *WebsocketNetwork) broadcastWithTimestamp(tag protocol.Tag, data []byte, when time.Time) error {
	msgArr := make([][]byte, 1, 1)
	msgArr[0] = data
	tagArr := make([]protocol.Tag, 1, 1)
	tagArr[0] = tag
	request := broadcastRequest{tags: tagArr, data: msgArr, enqueueTime: when, ctx: context.Background()}

	broadcastQueue := wn.broadcastQueueBulk
	if highPriorityTag(tagArr) {
		broadcastQueue = wn.broadcastQueueHighPrio
	}
	// no wait
	select {
	case broadcastQueue <- request:
		return nil
	default:
		return errBcastQFull
	}
}

func TestDelayedMessageDrop(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()
	counter := newMessageCounter(t, 5)
	counterDone := counter.done
	netB.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	currentTime := time.Now()
	for i := 0; i < 10; i++ {
		err := netA.broadcastWithTimestamp(protocol.TxnTag, []byte("foo"), currentTime.Add(time.Hour*time.Duration(i-5)))
		require.NoErrorf(t, err, "No error was expected")
	}

	select {
	case <-counterDone:
	case <-time.After(maxMessageQueueDuration):
		require.Equalf(t, 5, counter.count, "One or more messages failed to reach destination network")
	}
}

func TestSlowPeerDisconnection(t *testing.T) {
	partitiontest.PartitionTest(t)

	log := logging.TestingLog(t)
	log.SetLevel(logging.Info)
	wn := &WebsocketNetwork{
		log:                            log,
		config:                         defaultConfig,
		phonebook:                      MakePhonebook(1, 1*time.Millisecond),
		GenesisID:                      "go-test-network-genesis",
		NetworkID:                      config.Devtestnet,
		slowWritingPeerMonitorInterval: time.Millisecond * 50,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second
	wn.messagesOfInterest = nil // clear this before starting the network so that we won't be sending a MOI upon connection.

	netA := wn
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	var peers []*wsPeer
	peers, _ = netA.peerSnapshot(peers)
	require.Equalf(t, len(peers), 1, "Expected number of peers should be 1")
	peer := peers[0]
	// modify the peer on netA and
	// netA의 피어를 수정하고
	beforeLoopTime := time.Now()
	atomic.StoreInt64(&peer.intermittentOutgoingMessageEnqueueTime, beforeLoopTime.Add(-maxMessageQueueDuration).Add(time.Second).UnixNano())
	// wait up to 10 seconds for the monitor to figure out it needs to disconnect.
	// 모니터가 연결을 끊을 필요가 있음을 알아낼 때까지 최대 10초 동안 기다립니다.
	expire := beforeLoopTime.Add(2 * slowWritingPeerMonitorInterval)
	for {
		peers, _ = netA.peerSnapshot(peers)
		if len(peers) == 0 || peers[0] != peer {
			// make sure it took more than 1 second, and less than 5 seconds.
			// 1초 이상 5초 미만이 소요되었는지 확인합니다.
			waitTime := time.Now().Sub(beforeLoopTime)
			require.LessOrEqual(t, int64(time.Second), int64(waitTime))
			require.GreaterOrEqual(t, int64(5*time.Second), int64(waitTime))
			break
		}
		if time.Now().After(expire) {
			require.Fail(t, "Slow peer was not disconnected")
		}
		time.Sleep(time.Millisecond * 5)
	}
}

func TestForceMessageRelaying(t *testing.T) {
	partitiontest.PartitionTest(t)

	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(defaultConfig.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:       log,
		config:    defaultConfig,
		phonebook: MakePhonebook(1, 1*time.Millisecond),
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	wn.setup()
	wn.eventualReadyDelay = time.Second

	netA := wn
	netA.config.GossipFanout = 1

	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	counter := newMessageCounter(t, 5)
	counterDone := counter.done
	netA.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})
	netA.Start()
	addrA, postListen := netA.Address()
	require.Truef(t, postListen, "Listening network failed to start")

	noAddressConfig := defaultConfig
	noAddressConfig.NetAddress = ""
	netB := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netB.config.GossipFanout = 1
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	noAddressConfig.ForceRelayMessages = true
	netC := makeTestWebsocketNodeWithConfig(t, noAddressConfig)
	netC.config.GossipFanout = 1
	netC.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netC.Start()
	defer func() { t.Log("stopping C"); netC.Stop(); t.Log("C done") }()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)
	waitReady(t, netC, readyTimeout.C)

	// send 5 messages from both netB and netC to netA
	// netB와 netC에서 모두 5개의 메시지를 netA로 보냅니다.
	for i := 0; i < 5; i++ {
		err := netB.Relay(context.Background(), protocol.TxnTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
		err = netC.Relay(context.Background(), protocol.TxnTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
	}

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		if counter.count < 5 {
			require.Failf(t, "One or more messages failed to reach destination network", "%d > %d", 5, counter.count)
		} else if counter.count > 5 {
			require.Failf(t, "One or more messages that were expected to be dropped, reached destination network", "%d < %d", 5, counter.count)
		}
	}
	netA.ClearHandlers()
	counter = newMessageCounter(t, 10)
	counterDone = counter.done
	netA.RegisterHandlers([]TaggedMessageHandler{{Tag: protocol.TxnTag, MessageHandler: counter}})

	// hack the relayMessages on the netB so that it would start sending messages.
	// netB의 relayMessage를 해킹하여 메시지 전송을 시작합니다.
	netB.relayMessages = true
	// send additional 10 messages from netB
	// netB에서 추가로 10개의 메시지를 보냅니다.
	for i := 0; i < 10; i++ {
		err := netB.Relay(context.Background(), protocol.TxnTag, []byte{1, 2, 3}, true, nil)
		require.NoError(t, err)
	}

	select {
	case <-counterDone:
	case <-time.After(2 * time.Second):
		require.Failf(t, "One or more messages failed to reach destination network", "%d > %d", 10, counter.count)
	}

}

func TestSetUserAgentHeader(t *testing.T) {
	partitiontest.PartitionTest(t)

	headers := http.Header{}
	SetUserAgentHeader(headers)
	require.Equal(t, 1, len(headers))
	t.Log(headers)
}

func TestCheckProtocolVersionMatch(t *testing.T) {
	partitiontest.PartitionTest(t)

	// note - this test changes the SupportedProtocolVersions global variable ( SupportedProtocolVersions ) and therefore cannot be parallelized.
	// 참고 - 이 테스트는 SupportedProtocolVersions 전역 변수( SupportedProtocolVersions )를 변경하므로 병렬화할 수 없습니다.
	originalSupportedProtocolVersions := SupportedProtocolVersions
	defer func() {
		SupportedProtocolVersions = originalSupportedProtocolVersions
	}()
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(defaultConfig.BaseLoggerDebugLevel))
	wn := &WebsocketNetwork{
		log:       log,
		config:    defaultConfig,
		phonebook: MakePhonebook(1, 1*time.Millisecond),
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}
	wn.setup()

	SupportedProtocolVersions = []string{"2", "1"}

	header1 := make(http.Header)
	header1.Add(ProtocolAcceptVersionHeader, "1")
	header1.Add(ProtocolVersionHeader, "3")
	matchingVersion, otherVersion := wn.checkProtocolVersionMatch(header1)
	require.Equal(t, "1", matchingVersion)
	require.Equal(t, "", otherVersion)

	header2 := make(http.Header)
	header2.Add(ProtocolAcceptVersionHeader, "3")
	header2.Add(ProtocolAcceptVersionHeader, "4")
	header2.Add(ProtocolVersionHeader, "1")
	matchingVersion, otherVersion = wn.checkProtocolVersionMatch(header2)
	require.Equal(t, "1", matchingVersion)
	require.Equal(t, "1", otherVersion)

	header3 := make(http.Header)
	header3.Add(ProtocolVersionHeader, "3")
	matchingVersion, otherVersion = wn.checkProtocolVersionMatch(header3)
	require.Equal(t, "", matchingVersion)
	require.Equal(t, "3", otherVersion)

	header4 := make(http.Header)
	header4.Add(ProtocolVersionHeader, "5\n")
	matchingVersion, otherVersion = wn.checkProtocolVersionMatch(header4)
	require.Equal(t, "", matchingVersion)
	require.Equal(t, "5"+unprintableCharacterGlyph, otherVersion)
}

func handleTopicRequest(msg IncomingMessage) (out OutgoingMessage) {

	topics, err := UnmarshallTopics(msg.Data)
	if err != nil {
		return
	}

	val1b, f := topics.GetValue("val1")
	if !f {
		return
	}
	val2b, f := topics.GetValue("val2")
	if !f {
		return
	}
	val1 := int(val1b[0])
	val2 := int(val2b[0])

	respTopics := Topics{
		Topic{
			key:  "value",
			data: []byte{byte(val1 + val2)},
		},
	}
	return OutgoingMessage{
		Action: Respond,
		Tag:    protocol.TopicMsgRespTag,
		Topics: respTopics,
	}
}

// Set up two nodes, test topics send/receive is working
// 두 개의 노드를 설정하고 테스트 토픽 보내기/받기가 작동 중입니다.
func TestWebsocketNetworkTopicRoundtrip(t *testing.T) {
	partitiontest.PartitionTest(t)

	var topicMsgReqTag Tag = protocol.UniEnsBlockReqTag
	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	netB.RegisterHandlers([]TaggedMessageHandler{
		{
			Tag:            topicMsgReqTag,
			MessageHandler: HandlerFunc(handleTopicRequest),
		},
	})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	t.Log("a ready")
	waitReady(t, netB, readyTimeout.C)
	t.Log("b ready")

	peerA := netA.peers[0]

	topics := Topics{
		Topic{
			key:  "command",
			data: []byte("add"),
		},
		Topic{
			key:  "val1",
			data: []byte{1},
		},
		Topic{
			key:  "val2",
			data: []byte{4},
		},
	}

	resp, err := peerA.Request(context.Background(), topicMsgReqTag, topics)
	assert.NoError(t, err)

	sum, found := resp.Topics.GetValue("value")
	assert.Equal(t, true, found)
	assert.Equal(t, 5, int(sum[0]))
}

// Set up two nodes, have one of them request a certain message tag mask, and verify the other follow that.
// 두 개의 노드를 설정하고 그 중 하나가 특정 메시지 태그 마스크를 요청하도록 하고 다른 노드가 이를 따르는지 확인합니다.
func TestWebsocketNetworkMessageOfInterest(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.config.EnablePingHandler = false

	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	netB.config.EnablePingHandler = false
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	incomingMsgSync := deadlock.Mutex{}
	msgCounters := make(map[protocol.Tag]int)
	messageArriveWg := sync.WaitGroup{}
	msgHandler := func(msg IncomingMessage) (out OutgoingMessage) {
		incomingMsgSync.Lock()
		defer incomingMsgSync.Unlock()
		msgCounters[msg.Tag] = msgCounters[msg.Tag] + 1
		messageArriveWg.Done()
		return
	}
	messageFilterArriveWg := sync.WaitGroup{}
	messageFilterArriveWg.Add(1)
	waitMessageArriveHandler := func(msg IncomingMessage) (out OutgoingMessage) {
		messageFilterArriveWg.Done()
		return
	}

	// register all the handlers.
	// 모든 핸들러를 등록합니다.
	taggedHandlers := []TaggedMessageHandler{}
	for tag := range defaultSendMessageTags {
		taggedHandlers = append(taggedHandlers, TaggedMessageHandler{
			Tag:            tag,
			MessageHandler: HandlerFunc(msgHandler),
		})
	}
	netB.RegisterHandlers(taggedHandlers)
	netA.RegisterHandlers([]TaggedMessageHandler{
		{
			Tag:            protocol.AgreementVoteTag,
			MessageHandler: HandlerFunc(waitMessageArriveHandler),
		}})

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	// have netB asking netA to send it only AgreementVoteTag and ProposalPayloadTag
	// netB가 netA에게 동의 VoteTag 및 제안 페이로드 태그만 보내도록 요청합니다.
	netB.Broadcast(context.Background(), protocol.MsgOfInterestTag, MarshallMessageOfInterest([]protocol.Tag{protocol.AgreementVoteTag, protocol.ProposalPayloadTag}), true, nil)
	// send another message which we can track, so that we'll know that the first message was delivered.
	// 추적할 수 있는 다른 메시지를 보내 첫 번째 메시지가 배달되었음을 알 수 있습니다.
	netB.Broadcast(context.Background(), protocol.AgreementVoteTag, []byte{0, 1, 2, 3, 4}, true, nil)
	messageFilterArriveWg.Wait()

	messageArriveWg.Add(5 * 2) // we're expecting exactly 10 messages.
	// send 5 messages of few types.
	// 정확히 10개의 메시지가 필요합니다.
	// 몇 가지 유형의 5개 메시지를 보냅니다.
	for i := 0; i < 5; i++ {
		netA.Broadcast(context.Background(), protocol.AgreementVoteTag, []byte{0, 1, 2, 3, 4}, true, nil)
		netA.Broadcast(context.Background(), protocol.TxnTag, []byte{0, 1, 2, 3, 4}, true, nil)
		netA.Broadcast(context.Background(), protocol.ProposalPayloadTag, []byte{0, 1, 2, 3, 4}, true, nil)
		netA.Broadcast(context.Background(), protocol.VoteBundleTag, []byte{0, 1, 2, 3, 4}, true, nil)
	}
	// wait until all the expected messages arrive.
	messageArriveWg.Wait()
	for tag, count := range msgCounters {
		if tag == protocol.AgreementVoteTag || tag == protocol.ProposalPayloadTag {
			require.Equal(t, 5, count)
		} else {
			require.Equal(t, 0, count)
		}
	}
}

// Set up two nodes, have one of them disconnect from the other, and monitor disconnection error on the side that did not issue the disconnection.
// Plan:
// Network A will be sending messages to network B.
// Network B will respond with another message for the first 4 messages. When it receive the 5th message, it would close the connection.
// We want to get an event with disconnectRequestReceived
//===========
// 두 개의 노드를 설정하고 그 중 하나를 다른 노드와 연결 해제하고 연결 해제가 발생하지 않은 쪽의 연결 해제 오류를 모니터링합니다.
// 계획:
// 네트워크 A는 네트워크 B에 메시지를 보냅니다.
// 네트워크 B는 처음 4개 메시지에 대해 다른 메시지로 응답합니다. 다섯 번째 메시지를 받으면 연결을 닫습니다.
// disconnectRequestReceived로 이벤트를 가져오고 싶습니다.
func TestWebsocketDisconnection(t *testing.T) {
	partitiontest.PartitionTest(t)

	netA := makeTestWebsocketNode(t)
	netA.config.GossipFanout = 1
	netA.config.EnablePingHandler = false
	dl := eventsDetailsLogger{Logger: logging.TestingLog(t), eventReceived: make(chan interface{}, 1), eventIdentifier: telemetryspec.DisconnectPeerEvent}
	netA.log = dl

	netA.Start()
	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()
	netB := makeTestWebsocketNode(t)
	netB.config.GossipFanout = 1
	netB.config.EnablePingHandler = false
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { t.Log("stopping B"); netB.Stop(); t.Log("B done") }()

	msgHandlerA := func(msg IncomingMessage) (out OutgoingMessage) {
		// if we received a message, send a message back.
		// 메시지를 받으면 다시 메시지를 보냅니다.
		if msg.Data[0]%10 == 2 {
			netA.Broadcast(context.Background(), protocol.ProposalPayloadTag, []byte{msg.Data[0] + 8}, true, nil)
		}
		return
	}

	var msgCounterNetB uint32
	msgHandlerB := func(msg IncomingMessage) (out OutgoingMessage) {
		if atomic.AddUint32(&msgCounterNetB, 1) == 5 {
			// disconnect
			netB.DisconnectPeers()
		} else {
			// if we received a message, send a message back.
			// 메시지를 받으면 다시 메시지를 보냅니다.
			netB.Broadcast(context.Background(), protocol.ProposalPayloadTag, []byte{msg.Data[0] + 1}, true, nil)
			netB.Broadcast(context.Background(), protocol.ProposalPayloadTag, []byte{msg.Data[0] + 2}, true, nil)
		}
		return
	}

	// register all the handlers.
	// 모든 핸들러를 등록합니다.
	taggedHandlersA := []TaggedMessageHandler{
		{
			Tag:            protocol.ProposalPayloadTag,
			MessageHandler: HandlerFunc(msgHandlerA),
		},
	}
	netA.ClearHandlers()
	netA.RegisterHandlers(taggedHandlersA)

	taggedHandlersB := []TaggedMessageHandler{
		{
			Tag:            protocol.ProposalPayloadTag,
			MessageHandler: HandlerFunc(msgHandlerB),
		},
	}
	netB.ClearHandlers()
	netB.RegisterHandlers(taggedHandlersB)

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)
	netA.Broadcast(context.Background(), protocol.ProposalPayloadTag, []byte{0}, true, nil)
	// wait until the peers disconnect.
	for {
		peers := netA.GetPeers(PeersConnectedIn)
		if len(peers) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	select {
	case eventDetails := <-dl.eventReceived:
		switch disconnectPeerEventDetails := eventDetails.(type) {
		case telemetryspec.DisconnectPeerEventDetails:
			require.Equal(t, disconnectPeerEventDetails.Reason, string(disconnectRequestReceived))
		default:
			require.FailNow(t, "Unexpected event was send : %v", eventDetails)
		}

	default:
		require.FailNow(t, "The DisconnectPeerEvent was missing")
	}
}

// TestASCIIFiltering tests the behaviour of filterASCII by feeding it with few known inputs and verifying the expected outputs.
// TestASCIIFiltering은 알려진 입력이 거의 없고 예상 출력을 확인하여 filterASCII의 동작을 테스트합니다.
func TestASCIIFiltering(t *testing.T) {
	partitiontest.PartitionTest(t)

	testUnicodePrintableStrings := []struct {
		testString     string
		expectedString string
	}{
		{"abc", "abc"},
		{"", ""},
		{"אבג", unprintableCharacterGlyph + unprintableCharacterGlyph + unprintableCharacterGlyph},
		{"\u001b[31mABC\u001b[0m", unprintableCharacterGlyph + "[31mABC" + unprintableCharacterGlyph + "[0m"},
		{"ab\nc", "ab" + unprintableCharacterGlyph + "c"},
	}
	for _, testElement := range testUnicodePrintableStrings {
		outString := filterASCII(testElement.testString)
		require.Equalf(t, testElement.expectedString, outString, "test string:%s", testElement.testString)
	}
}

type callbackLogger struct {
	logging.Logger
	InfoCallback  func(...interface{})
	InfofCallback func(string, ...interface{})
	WarnCallback  func(...interface{})
	WarnfCallback func(string, ...interface{})
}

func (cl callbackLogger) Info(args ...interface{}) {
	cl.InfoCallback(args...)
}
func (cl callbackLogger) Infof(s string, args ...interface{}) {
	cl.InfofCallback(s, args...)
}

func (cl callbackLogger) Warn(args ...interface{}) {
	cl.WarnCallback(args...)
}
func (cl callbackLogger) Warnf(s string, args ...interface{}) {
	cl.WarnfCallback(s, args...)
}

// TestMaliciousCheckServerResponseVariables test the checkServerResponseVariables to ensure it doesn't print the a malicious input without being filtered to the log file.
// TestMaliciousCheckServerResponseVariables는 checkServerResponseVariables를 테스트하여 로그 파일로 필터링되지 않고 악의적인 입력을 인쇄하지 않는지 확인합니다.
func TestMaliciousCheckServerResponseVariables(t *testing.T) {
	partitiontest.PartitionTest(t)

	wn := makeTestWebsocketNode(t)
	wn.GenesisID = "genesis-id1"
	wn.RandomID = "random-id1"
	wn.log = callbackLogger{
		Logger: wn.log,
		InfoCallback: func(args ...interface{}) {
			s := fmt.Sprint(args...)
			require.NotContains(t, s, "א")
		},
		InfofCallback: func(s string, args ...interface{}) {
			s = fmt.Sprintf(s, args...)
			require.NotContains(t, s, "א")
		},
		WarnCallback: func(args ...interface{}) {
			s := fmt.Sprint(args...)
			require.NotContains(t, s, "א")
		},
		WarnfCallback: func(s string, args ...interface{}) {
			s = fmt.Sprintf(s, args...)
			require.NotContains(t, s, "א")
		},
	}

	header1 := http.Header{}
	header1.Set(ProtocolVersionHeader, ProtocolVersion+"א")
	header1.Set(NodeRandomHeader, wn.RandomID+"tag")
	header1.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, matchingVersion := wn.checkServerResponseVariables(header1, "addressX")
	require.Equal(t, false, responseVariableOk)
	require.Equal(t, "", matchingVersion)

	header2 := http.Header{}
	header2.Set(ProtocolVersionHeader, ProtocolVersion)
	header2.Set("א", "א")
	header2.Set(GenesisHeader, wn.GenesisID)
	responseVariableOk, matchingVersion = wn.checkServerResponseVariables(header2, "addressX")
	require.Equal(t, false, responseVariableOk)
	require.Equal(t, "", matchingVersion)

	header3 := http.Header{}
	header3.Set(ProtocolVersionHeader, ProtocolVersion)
	header3.Set(NodeRandomHeader, wn.RandomID+"tag")
	header3.Set(GenesisHeader, wn.GenesisID+"א")
	responseVariableOk, matchingVersion = wn.checkServerResponseVariables(header3, "addressX")
	require.Equal(t, false, responseVariableOk)
	require.Equal(t, "", matchingVersion)
}

func BenchmarkVariableTransactionMessageBlockSizes(t *testing.B) {
	netA := makeTestWebsocketNode(t)
	netA.log.SetLevel(logging.Warn)
	netA.config.GossipFanout = 1
	netA.config.EnablePingHandler = false
	netA.Start()
	defer func() { netA.Stop() }()

	netB := makeTestWebsocketNode(t)
	netB.log.SetLevel(logging.Warn)
	netB.config.GossipFanout = 1
	netB.config.EnablePingHandler = false
	addrA, postListen := netA.Address()
	require.True(t, postListen)
	t.Log(addrA)
	netB.phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
	netB.Start()
	defer func() { netB.Stop() }()

	const txnSize = 250
	var msgProcessed chan struct{}

	msgHandlerA := func(msg IncomingMessage) (out OutgoingMessage) {
		// spend some time, linear to the size of the message -
		// 메시지 크기에 선형으로 시간을 보냅니다.
		txnCount := len(msg.Data) / txnSize
		time.Sleep(time.Nanosecond * time.Duration(10000*txnCount))
		msgProcessed <- struct{}{}
		return
	}
	// register all the handlers.
	// 모든 핸들러를 등록합니다.
	taggedHandlersA := []TaggedMessageHandler{
		{
			Tag:            protocol.TxnTag,
			MessageHandler: HandlerFunc(msgHandlerA),
		},
	}
	netA.ClearHandlers()
	netA.RegisterHandlers(taggedHandlersA)

	netB.ClearHandlers()

	readyTimeout := time.NewTimer(2 * time.Second)
	waitReady(t, netA, readyTimeout.C)
	waitReady(t, netB, readyTimeout.C)

	highestRate := float64(1)
	sinceHighestRate := 0
	rate := float64(0)
	for txnCount := 1; txnCount < 1024; {
		t.Run(fmt.Sprintf("%d-TxnPerMessage", txnCount), func(t *testing.B) {
			msgProcessed = make(chan struct{}, t.N/txnCount)
			dataBuffer := make([]byte, txnSize*txnCount)
			crypto.RandBytes(dataBuffer[:])
			t.ResetTimer()
			startTime := time.Now()
			for i := 0; i < t.N/txnCount; i++ {
				netB.Broadcast(context.Background(), protocol.TxnTag, dataBuffer, true, nil)
				<-msgProcessed
			}
			deltaTime := time.Now().Sub(startTime)
			rate = float64(t.N) * float64(time.Second) / float64(deltaTime)
			t.ReportMetric(rate, "txn/sec")
		})
		if rate > highestRate {
			highestRate = rate
			sinceHighestRate = 0
			txnCount += txnCount/10 + 1
			continue
		}
		sinceHighestRate++
		if sinceHighestRate > 4 {
			break
		}
		txnCount += txnCount/4 + 1
	}
}

type urlCase struct {
	text string
	out  url.URL
}

func TestParseHostOrURL(t *testing.T) {
	partitiontest.PartitionTest(t)
	urlTestCases := []urlCase{
		{"localhost:123", url.URL{Scheme: "http", Host: "localhost:123"}},
		{"http://localhost:123", url.URL{Scheme: "http", Host: "localhost:123"}},
		{"ws://localhost:9999", url.URL{Scheme: "ws", Host: "localhost:9999"}},
		{"wss://localhost:443", url.URL{Scheme: "wss", Host: "localhost:443"}},
		{"https://localhost:123", url.URL{Scheme: "https", Host: "localhost:123"}},
		{"https://somewhere.tld", url.URL{Scheme: "https", Host: "somewhere.tld"}},
		{"http://127.0.0.1:123", url.URL{Scheme: "http", Host: "127.0.0.1:123"}},
		{"//somewhere.tld", url.URL{Scheme: "", Host: "somewhere.tld"}},
		{"//somewhere.tld:4601", url.URL{Scheme: "", Host: "somewhere.tld:4601"}},
		{"http://[::]:123", url.URL{Scheme: "http", Host: "[::]:123"}},
		{"1.2.3.4:123", url.URL{Scheme: "http", Host: "1.2.3.4:123"}},
		{"[::]:123", url.URL{Scheme: "http", Host: "[::]:123"}},
		{"r2-devnet.devnet.algodev.network:4560", url.URL{Scheme: "http", Host: "r2-devnet.devnet.algodev.network:4560"}},
		{"::11.22.33.44:123", url.URL{Scheme: "http", Host: "::11.22.33.44:123"}},
	}
	badUrls := []string{
		"justahost",
		"localhost:WAT",
		"http://localhost:WAT",
		"https://localhost:WAT",
		"ws://localhost:WAT",
		"wss://localhost:WAT",
		"//localhost:WAT",
		"://badaddress", // See rpcs/blockService_test.go TestRedirectFallbackEndpoints
		"://localhost:1234",
		":xxx",
		":xxx:1234",
		"::11.22.33.44",
		":a:1",
		":a:",
		":1",
		":a",
		":",
		"",
	}
	for _, tc := range urlTestCases {
		t.Run(tc.text, func(t *testing.T) {
			v, err := ParseHostOrURL(tc.text)
			require.NoError(t, err)
			if tc.out != *v {
				t.Errorf("url wanted %#v, got %#v", tc.out, v)
				return
			}
		})
	}
	for _, addr := range badUrls {
		t.Run(addr, func(t *testing.T) {
			_, err := ParseHostOrURL(addr)
			require.Error(t, err, "url should fail", addr)
		})
	}
}
