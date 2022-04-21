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
	"sync/atomic"

	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/protocol"
)

// NetPrioScheme is an implementation of network connection priorities based on a challenge-response protocol.
// NetPrioScheme은 시도-응답 프로토콜을 기반으로 하는 네트워크 연결 우선 순위의 구현입니다.
type NetPrioScheme interface {
	NewPrioChallenge() string
	MakePrioResponse(challenge string) []byte
	VerifyPrioResponse(challenge string, response []byte) (basics.Address, error)
	GetPrioWeight(addr basics.Address) uint64
}

func prioResponseHandler(message IncomingMessage) OutgoingMessage {
	wn := message.Net.(*WebsocketNetwork)
	if wn.prioScheme == nil {
		return OutgoingMessage{}
	}

	peer := message.Sender.(*wsPeer)
	challenge := peer.prioChallenge
	if challenge == "" {
		return OutgoingMessage{}
	}

	addr, err := wn.prioScheme.VerifyPrioResponse(challenge, message.Data)
	if err != nil {
		wn.log.Warnf("prioScheme.VerifyPrioResponse from %s: %v", peer.rootURL, err)
	} else {
		weight := wn.prioScheme.GetPrioWeight(addr)

		wn.peersLock.Lock()
		defer wn.peersLock.Unlock()
		wn.prioTracker.setPriority(peer, addr, weight)
	}

	// For testing
	if wn.prioResponseChan != nil {
		wn.prioResponseChan <- peer
	}

	return OutgoingMessage{}
}

var prioHandlers = []TaggedMessageHandler{
	{protocol.NetPrioResponseTag, HandlerFunc(prioResponseHandler)},
}

// The prioTracker sorts active peers by priority, and ensures there's only one peer with weight per address.
// The data structure is not thread-safe;
// it is protected by wn.peersLock.
// prioTracker는 우선 순위에 따라 활성 피어를 정렬하고 주소당 가중치가 있는 피어가 하나만 있는지 확인합니다.
// 데이터 구조는 스레드로부터 안전하지 않습니다.
// wn.peersLock에 의해 보호됩니다.
type prioTracker struct {
	// If a peer has a non-zero prioWeight, it will be present in this map under its peerAddress.
	// 피어에 0이 아닌 prioWeight가 있으면 이 맵의 peerAddress 아래에 표시됩니다.
	peerByAddress map[basics.Address]*wsPeer

	wn *WebsocketNetwork
}

func newPrioTracker(wn *WebsocketNetwork) *prioTracker {
	return &prioTracker{
		peerByAddress: make(map[basics.Address]*wsPeer),
		wn:            wn,
	}
}

func (pt *prioTracker) setPriority(peer *wsPeer, addr basics.Address, weight uint64) {
	wn := pt.wn

	// Make sure this peer is currently in the peers slice
	// 이 피어가 현재 피어 슬라이스에 있는지 확인합니다.
	if peer.peerIndex >= len(wn.peers) || wn.peers[peer.peerIndex] != peer {
		// The peer might be in the process of being added to wn.peers;
		// in this case, wn.addPeer() will call setPriority again and
		// we will finish setup in that call.
		// 피어가 wn.peers에 추가되는 중일 수 있습니다.
		// 이 경우 wn.addPeer()는 setPriority를 다시 호출하고
		// 우리는 그 호출에서 설정을 마칠 것입니다.
		peer.prioAddress = addr
		peer.prioWeight = weight
		return
	}

	// Evict old peer from same address, if present
	// 같은 주소에서 오래된 피어(있는 경우)를 제거합니다.
	old, present := pt.peerByAddress[addr]
	if present {
		if old == peer {
			// No eviction necessary if it was already this peer
			// 이미 이 피어인 경우 축출이 필요하지 않습니다.
			if peer.prioAddress == addr && peer.prioWeight == weight {
				// Same address and weight, nothing to update
				return
			}
		} else if old.prioAddress == addr {
			old.prioWeight = 0
			if old.peerIndex < len(wn.peers) && wn.peers[old.peerIndex] == old {
				heap.Fix(peersHeap{wn}, old.peerIndex)
			}
		}
	}

	// Check if this peer was in peerByAddress[] under its old address, and delete that mapping if so.
	// 이 피어가 이전 주소 아래의 peerByAddress[]에 있는지 확인하고, 그렇다면 해당 매핑을 삭제합니다.
	if addr != peer.prioAddress && peer == pt.peerByAddress[peer.prioAddress] {
		delete(pt.peerByAddress, peer.prioAddress)
	}

	pt.peerByAddress[addr] = peer
	peer.prioAddress = addr
	peer.prioWeight = weight
	heap.Fix(peersHeap{wn}, peer.peerIndex)
	atomic.AddInt32(&wn.peersChangeCounter, 1)
}

func (pt *prioTracker) removePeer(peer *wsPeer) {
	addr := peer.prioAddress
	old, present := pt.peerByAddress[addr]
	if present && old == peer {
		delete(pt.peerByAddress, addr)
	}
}
