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
	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/protocol"
)

// IncomingMessage represents a message arriving from some peer in our p2p network
// IncomingMessage는 p2p 네트워크의 일부 피어에서 도착하는 메시지를 나타냅니다.
type messageFilter struct {
	deadlock.Mutex
	buckets          []map[crypto.Digest]bool
	maxBucketSize    int
	currentTopBucket int
	nonce            [16]byte
}

func makeMessageFilter(bucketsCount, maxBucketSize int) *messageFilter {
	mf := &messageFilter{
		buckets:          make([]map[crypto.Digest]bool, bucketsCount),
		maxBucketSize:    maxBucketSize,
		currentTopBucket: 0,
	}
	for i := range mf.buckets {
		mf.buckets[i] = make(map[crypto.Digest]bool)
	}
	crypto.RandBytes(mf.nonce[:])
	return mf
}

// CheckIncomingMessage CheckMessage checks if the given tag/msg already in the collection, and return true if it was there before the call.
// CheckMessage는 주어진 태그/메시지가 이미 컬렉션에 있는지 확인하고 호출 전에 존재했다면 true를 반환합니다.
// Prepends our own random secret to the message to make it hard to abuse hash collisions.
// 해시 충돌을 남용하기 어렵게 메시지에 우리 자신의 무작위 비밀을 추가합니다.
func (f *messageFilter) CheckIncomingMessage(tag protocol.Tag, msg []byte, add bool, promote bool) bool {
	hasher := crypto.NewHash()
	hasher.Write(f.nonce[:])
	hasher.Write([]byte(tag))
	hasher.Write(msg)
	var digest crypto.Digest
	hasher.Sum(digest[:0])
	return f.CheckDigest(digest, add, promote)
}

// CheckDigest checks if the given digest already in the collection, and return true if it was there before the call.
// CheckDigest 는 주어진 다이제스트가 이미 컬렉션에 있는지 확인하고 호출 전에 존재했다면 true를 반환합니다.

// CheckDigest is used on outgoing messages, either given a hash from a peer notifying us of messages it doesn't need, or as we are about to send a message to see if we should send it.
// CheckDigest 는 필요하지 않은 메시지를 알려주는 피어로부터 해시가 제공되거나 메시지를 보내야 하는지 확인하기 위해 메시지를 보내려고 할 때 나가는 메시지에 사용됩니다.
func (f *messageFilter) CheckDigest(msgHash crypto.Digest, add bool, promote bool) bool {
	f.Lock()
	defer f.Unlock()
	idx, has := f.find(msgHash)
	if !add {
		return has
	}

	if !has {
		// we don't have this entry. add it.
		// 이 항목이 없습니다. 추가하십시오.
		f.buckets[f.currentTopBucket][msgHash] = true
	} else {
		// we already have it.
		// do we need to promote it ?
		// 홍보해야 합니까?
		if promote && f.currentTopBucket != idx {
			delete(f.buckets[idx], msgHash)
			f.buckets[f.currentTopBucket][msgHash] = true
		}
	}
	// check to see if the current bucket reached capacity.
	if len(f.buckets[f.currentTopBucket]) >= f.maxBucketSize {
		f.currentTopBucket = (f.currentTopBucket + len(f.buckets) - 1) % len(f.buckets)
		f.buckets[f.currentTopBucket] = make(map[crypto.Digest]bool)
	}

	return has
}

func generateMessageDigest(tag protocol.Tag, msg []byte) crypto.Digest {
	hasher := crypto.NewHash()
	hasher.Write([]byte(tag))
	hasher.Write(msg)
	var digest crypto.Digest
	hasher.Sum(digest[:0])
	return digest
}

func (f *messageFilter) find(digest crypto.Digest) (idx int, found bool) {
	for i := len(f.buckets); i > 0; i-- {
		bucketIdx := (f.currentTopBucket + i) % len(f.buckets)
		if _, has := f.buckets[bucketIdx][digest]; has {
			return bucketIdx, true
		}
	}
	return -1, false
}
