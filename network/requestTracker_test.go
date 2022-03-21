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
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func (ard *hostIncomingRequests) remove(trackedRequest *TrackerRequest) {
	for i := range ard.requests {
		if ard.requests[i] == trackedRequest {
			// remove entry.
			ard.requests = append(ard.requests[0:i], ard.requests[i+1:]...)
			return
		}
	}
}
func TestHostIncomingRequestsOrdering(t *testing.T) {
	partitiontest.PartitionTest(t)

	if defaultConfig.ConnectionsRateLimitingCount == 0 || defaultConfig.ConnectionsRateLimitingWindowSeconds == 0 {
		t.Skip()
	}
	// add 100 items to the hostIncomingRequests object, and make sure they are sorted.
	// hostIncomingRequests 객체에 100개의 항목을 추가하고 정렬되었는지 확인합니다.
	hir := hostIncomingRequests{}
	now := time.Now()
	perm := rand.Perm(100)
	for i := 0; i < 100; i++ {
		trackedRequest := makeTrackerRequest("remoteaddr", "host", "port", now.Add(time.Duration(perm[i])*time.Minute), nil)
		hir.add(trackedRequest)
	}
	require.Equal(t, 100, len(hir.requests))

	// make sure the array ends up being ordered.
	// 배열이 순서대로 끝나는지 확인합니다.
	for i := 1; i < 100; i++ {
		require.True(t, hir.requests[i].created.After(hir.requests[i-1].created))
	}

	// test the remove function.
	// 제거 기능을 테스트합니다.
	for len(hir.requests) > 0 {
		// select a random item.
		// 임의의 항목을 선택합니다.
		i := rand.Int() % len(hir.requests)
		o := hir.requests[i]
		hir.remove(o)
		// make sure the item isn't there anymore.
		// 항목이 더 이상 존재하지 않는지 확인합니다.
		for _, p := range hir.requests {
			require.False(t, p == o)
			require.Equal(t, hir.countConnections(now.Add(-time.Second)), uint(len(hir.requests)))
		}
	}
}

func TestRateLimiting(t *testing.T) {
	partitiontest.PartitionTest(t)

	if defaultConfig.ConnectionsRateLimitingCount == 0 || defaultConfig.ConnectionsRateLimitingWindowSeconds == 0 {
		t.Skip()
	}
	log := logging.TestingLog(t)
	log.SetLevel(logging.Level(defaultConfig.BaseLoggerDebugLevel))
	testConfig := defaultConfig
	// This test is conducted locally, so we want to treat all hosts the same for counting incoming requests.
	// 이 테스트는 로컬에서 수행되므로 들어오는 요청을 계산할 때 모든 호스트를 동일하게 처리하려고 합니다.
	testConfig.DisableLocalhostConnectionRateLimit = false
	wn := &WebsocketNetwork{
		log:       log,
		config:    testConfig,
		phonebook: MakePhonebook(1, 1),
		GenesisID: "go-test-network-genesis",
		NetworkID: config.Devtestnet,
	}

	// increase the IncomingConnectionsLimit/MaxConnectionsPerIP limits, since we don't want to test these.
	// IncomingConnectionsLimit/MaxConnectionsPerIP 제한을 증가시킵니다. 우리는 이것을 테스트하고 싶지 않기 때문입니다.
	wn.config.IncomingConnectionsLimit = int(testConfig.ConnectionsRateLimitingCount) * 5
	wn.config.MaxConnectionsPerIP += int(testConfig.ConnectionsRateLimitingCount) * 5

	wn.setup()
	wn.eventualReadyDelay = time.Second

	netA := wn
	netA.config.GossipFanout = 1

	defer func() { t.Log("stopping A"); netA.Stop(); t.Log("A done") }()

	netA.Start()
	addrA, postListen := netA.Address()
	require.Truef(t, postListen, "Listening network failed to start")

	noAddressConfig := testConfig
	noAddressConfig.NetAddress = ""

	clientsCount := int(testConfig.ConnectionsRateLimitingCount + 5)

	networks := make([]*WebsocketNetwork, clientsCount)
	phonebooks := make([]Phonebook, clientsCount)
	for i := 0; i < clientsCount; i++ {
		networks[i] = makeTestWebsocketNodeWithConfig(t, noAddressConfig)
		networks[i].config.GossipFanout = 1
		phonebooks[i] = MakePhonebook(networks[i].config.ConnectionsRateLimitingCount,
			time.Duration(networks[i].config.ConnectionsRateLimitingWindowSeconds)*time.Second)
		phonebooks[i].ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
		networks[i].phonebook = MakePhonebook(1, 1*time.Millisecond)
		networks[i].phonebook.ReplacePeerList([]string{addrA}, "default", PhoneBookEntryRelayRole)
		defer func(net *WebsocketNetwork, i int) {
			t.Logf("stopping network %d", i)
			net.Stop()
			t.Logf("network %d done", i)
		}(networks[i], i)
	}

	deadline := time.Now().Add(time.Duration(testConfig.ConnectionsRateLimitingWindowSeconds) * time.Second)

	for i := 0; i < clientsCount; i++ {
		networks[i].Start()
	}

	var connectedClients int
	timedOut := false
	for {
		if time.Now().After(deadline) {
			timedOut = true
			break
		}
		connectedClients = 0
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < clientsCount; i++ {
			// check if the channel is ready.
			readyCh := networks[i].Ready()
			select {
			case <-readyCh:
				// it's closed, so this client got connected.
				connectedClients++
				phonebookLen := len(phonebooks[i].GetAddresses(1, PhoneBookEntryRelayRole))
				// if this channel is ready, than we should have an address, since it didn't get blocked.
				// 이 채널이 준비되면 차단되지 않았으므로 주소가 있어야 합니다.
				require.Equal(t, 1, phonebookLen)
			default:
				// not ready yet.
				// 아직 준비되지 않았습니다.
				// wait abit longer.
				// 조금 더 기다립니다.
			}
		}
		if connectedClients >= int(testConfig.ConnectionsRateLimitingCount) {
			timedOut = time.Now().After(deadline)
			break
		}
	}
	if !timedOut {
		// test to see that at least some of the clients have seen 429
		// 적어도 일부 클라이언트가 429를 보았는지 테스트합니다.
		require.Equal(t, int(testConfig.ConnectionsRateLimitingCount), connectedClients)
	}
}

func TestIsLocalHost(t *testing.T) {
	partitiontest.PartitionTest(t)

	require.True(t, isLocalhost("localhost"))
	require.True(t, isLocalhost("127.0.0.1"))
	require.True(t, isLocalhost("[::1]"))
	require.True(t, isLocalhost("::1"))
	require.True(t, isLocalhost("[::]"))
	require.False(t, isLocalhost("192.168.0.1"))
	require.False(t, isLocalhost(""))
	require.False(t, isLocalhost("0.0.0.0"))
	require.False(t, isLocalhost("127.0.0.0"))
}
