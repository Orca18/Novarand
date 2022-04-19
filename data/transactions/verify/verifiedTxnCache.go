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

package verify

import (
	"errors"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/data/transactions/logic"
	"github.com/Orca18/novarand/protocol"
)

const maxPinnedEntries = 500000

// VerifiedTxnCacheError helps to identify the errors of a cache error and diffrentiate these from a general verification errors.
/*
VerifiedTxnCacheError는 캐시 오류의 오류를 식별하고 일반 확인 오류와 구별하는 데 도움이 된다.
*/
type VerifiedTxnCacheError struct {
	inner error
}

// Unwrap provides access to the underlying error
func (e *VerifiedTxnCacheError) Unwrap() error {
	return e.inner
}

// Error formats the underlying error message
func (e *VerifiedTxnCacheError) Error() string {
	return e.inner.Error()
}

// errTooManyPinnedEntries is being generated when we attempt to pin an transaction while we've already exceeded the maximum number of allows
// transactions in the verification cache.
var errTooManyPinnedEntries = &VerifiedTxnCacheError{errors.New("Too many pinned entries")}

// errMissingPinnedEntry is being generated when we're trying to pin a transaction that does not appear in the cache
var errMissingPinnedEntry = &VerifiedTxnCacheError{errors.New("Missing pinned entry")}

// VerifiedTransactionCache provides a cached store of recently verified transactions. The cache is desiged two have two separate "levels". On the
// bottom tier, the cache would be using a cyclic buffer, where old transactions would end up overridden by new ones. In order to support transactions
// that goes into the transaction pool, we have a higher tier of pinned cache. Pinned transactions would not be cycled-away by new incoming transactions,
// and would only get eliminated by updates to the transaction pool, which would inform the cache of updates to the pinned items.
/*
	VerifiedTransactionCache는 최근 검증된 트랜잭션들을 저장하는 캐시다.
	캐시는 두개의 레벨이 있다.
	아래쪽 부분의 캐시는 순환버퍼를 사용해서 오래된 트랜잭션은 새로운 트랜잭션에게 덮어진다.
	트랜잭션풀로 들어가는 트랜잭션들을 지원하기 위해 윗 부분의 캐시는 고정되어있다.
	고정 캐시에 있는 트랜잭션은 트랜잭션풀로 이동했다는 업데이트가 있을 때만 캐시에서 제거된다.
*/
type VerifiedTransactionCache interface {
	// Add adds a given transaction group and it's associated group context to the cache. If any of the transactions already appear
	// in the cache, the new entry overrides the old one.
	// 서명된 트랜잭션 정보를 캐시에 저장한다.
	Add(txgroup []transactions.SignedTxn, groupCtx *GroupContext)

	// AddPayset works in a similar way to Add, but is intended for adding an array of transaction groups, along with their corresponding contexts.
	// 서명된 트랜잭션 배열 정보를 캐시에 저장한다.
	AddPayset(txgroup [][]transactions.SignedTxn, groupCtxs []*GroupContext) error

	// GetUnverifiedTranscationGroups compares the provided payset against the currently cached transactions and figure which transaction groups aren't fully cached.
	// GetUnverifiedTranscationGroups는 서명된 트랜잭션 세트를 현재 캐시된 트랜잭션과 비교하고 완전히 캐시되지 않은 트랜잭션 그룹을 파악합니다.
	GetUnverifiedTranscationGroups(payset [][]transactions.SignedTxn, CurrSpecAddrs transactions.SpecialAddresses, CurrProto protocol.ConsensusVersion) [][]transactions.SignedTxn

	// UpdatePinned replaces the pinned entries with the one provided in the pinnedTxns map. This is typically expected to be a subset of the
	// already-pinned transactions. If a transaction is not currently pinned, and it's can't be found in the cache, a errMissingPinnedEntry error would be generated.
	// UpdatePinned는 고정된 항목을 pinnedTxns 맵에 제공된 항목으로 바꿉니다(고정된 캐시에 있는 데이터를 인풋 트랜잭션집합으로 변경하는 것 같다)
	UpdatePinned(pinnedTxns map[transactions.Txid]transactions.SignedTxn) error

	// Pin function would mark the given transaction group as pinned.
	// 인풋 트랜잭션그룹을 pin 캐시에 넣는다
	Pin(txgroup []transactions.SignedTxn) error
}

// verifiedTransactionCache provides an implementation of the VerifiedTransactionCache interface
/*
	검증된 트랜잭션 캐시 인터페이스의 구조체
	반복적인 서명을 피하기 위해 서명이 완료된 트랜잭션을 캐시에 저장을 해놓는것이다.
*/
type verifiedTransactionCache struct {
	// Number of entries in each map (bucket).
	// 각 맵에서 저장할 수 있는 트랜잭션 수
	entriesPerBucket int
	// bucketsLock is the lock for syncornizing the access to the cache
	bucketsLock deadlock.Mutex
	// buckets is the circular cache buckets buffer
	// 순환 캐시
	buckets []map[transactions.Txid]*GroupContext
	// pinned is the pinned transactions entries map.
	// 고정 캐시
	pinned map[transactions.Txid]*GroupContext
	// base is the index into the buckets array where the next transaction entry would be written.
	// 다음 tx가 저장되어야 할 캐시 버킷에서의 인덱스
	base int
}

// MakeVerifiedTransactionCache creates an instance of verifiedTransactionCache and returns it.
func MakeVerifiedTransactionCache(cacheSize int) VerifiedTransactionCache {
	impl := &verifiedTransactionCache{
		entriesPerBucket: (cacheSize + 1) / 2,
		buckets:          make([]map[transactions.Txid]*GroupContext, 3),
		pinned:           make(map[transactions.Txid]*GroupContext, cacheSize),
		base:             0,
	}
	for i := 0; i < len(impl.buckets); i++ {
		impl.buckets[i] = make(map[transactions.Txid]*GroupContext, impl.entriesPerBucket)
	}
	return impl
}

// Add adds a given transaction group and it's associated group context to the cache. If any of the transactions already appear
// in the cache, the new entry overrides the old one.
func (v *verifiedTransactionCache) Add(txgroup []transactions.SignedTxn, groupCtx *GroupContext) {
	v.bucketsLock.Lock()
	defer v.bucketsLock.Unlock()
	v.add(txgroup, groupCtx)
}

// AddPayset works in a similar way to Add, but is intended for adding an array of transaction groups, along with their corresponding contexts.
func (v *verifiedTransactionCache) AddPayset(txgroup [][]transactions.SignedTxn, groupCtxs []*GroupContext) error {
	v.bucketsLock.Lock()
	defer v.bucketsLock.Unlock()
	for i := range txgroup {
		v.add(txgroup[i], groupCtxs[i])
	}
	return nil
}

// GetUnverifiedTranscationGroups compares the provided payset against the currently cached transactions and figure which transaction groups aren't fully cached.
/*

 */
func (v *verifiedTransactionCache) GetUnverifiedTranscationGroups(txnGroups [][]transactions.SignedTxn, currSpecAddrs transactions.SpecialAddresses, currProto protocol.ConsensusVersion) (unverifiedGroups [][]transactions.SignedTxn) {
	v.bucketsLock.Lock()
	defer v.bucketsLock.Unlock()
	groupCtx := &GroupContext{
		specAddrs:        currSpecAddrs,
		consensusVersion: currProto,
	}
	unverifiedGroups = make([][]transactions.SignedTxn, 0, len(txnGroups))

	for txnGroupIndex := 0; txnGroupIndex < len(txnGroups); txnGroupIndex++ {
		signedTxnGroup := txnGroups[txnGroupIndex]
		verifiedTxn := 0
		groupCtx.minTealVersion = logic.ComputeMinTealVersion(transactions.WrapSignedTxnsWithAD(signedTxnGroup), false)

		baseBucket := v.base
		for txnIdx := 0; txnIdx < len(signedTxnGroup); txnIdx++ {
			txn := &signedTxnGroup[txnIdx]
			id := txn.Txn.ID()
			// check pinned first
			entryGroup := v.pinned[id]
			// if not found in the pinned map, try to find in the verified buckets:
			if entryGroup == nil {
				// try to look in the previously verified buckets.
				// we use the (base + W) % W trick here so we can go backward and wrap around the zero.
				for offsetBucketIdx := baseBucket + len(v.buckets); offsetBucketIdx > baseBucket; offsetBucketIdx-- {
					bucketIdx := offsetBucketIdx % len(v.buckets)
					if params, has := v.buckets[bucketIdx][id]; has {
						entryGroup = params
						baseBucket = bucketIdx
						break
					}
				}
			}

			if entryGroup == nil {
				break
			}

			if !entryGroup.Equal(groupCtx) {
				break
			}

			if entryGroup.signedGroupTxns[txnIdx].Sig != txn.Sig || (!entryGroup.signedGroupTxns[txnIdx].Msig.Equal(txn.Msig)) || (!entryGroup.signedGroupTxns[txnIdx].Lsig.Equal(&txn.Lsig)) || (entryGroup.signedGroupTxns[txnIdx].AuthAddr != txn.AuthAddr) {
				break
			}
			verifiedTxn++
		}
		if verifiedTxn != len(signedTxnGroup) || verifiedTxn == 0 {
			unverifiedGroups = append(unverifiedGroups, signedTxnGroup)
		}
	}
	return
}

// UpdatePinned replaces the pinned entries with the one provided in the pinnedTxns map. This is typically expected to be a subset of the
// already-pinned transactions. If a transaction is not currently pinned, and it's can't be found in the cache, a errMissingPinnedEntry error would be generated.
/*
	UpdatePinned는 고정된 항목을 pinnedTxns 맵에 제공된 항목으로 바꿉니다.
	이것은 일반적으로 이미 고정된 트랜잭션의 하위 집합으로 예상됩니다.
	입력된 트랜잭션 중 하나라고 현재 고정되어 있지 않고 캐시에서 찾을 수 없는 경우 errMissingPinnedEntry 오류가 생성됩니다.
*/
func (v *verifiedTransactionCache) UpdatePinned(pinnedTxns map[transactions.Txid]transactions.SignedTxn) (err error) {
	v.bucketsLock.Lock()
	defer v.bucketsLock.Unlock()
	pinned := make(map[transactions.Txid]*GroupContext, len(pinnedTxns))
	for txID := range pinnedTxns {
		if groupEntry, has := v.pinned[txID]; has {
			pinned[txID] = groupEntry
			continue
		}

		// entry isn't in pinned; maybe we have it in one of the buckets ?
		found := false
		// we use the (base + W) % W trick here so we can go backward and wrap around the zero.
		for offsetBucketIdx := v.base + len(v.buckets); offsetBucketIdx > v.base; offsetBucketIdx-- {
			bucketIdx := offsetBucketIdx % len(v.buckets)
			if groupEntry, has := v.buckets[bucketIdx][txID]; has {
				pinned[txID] = groupEntry
				found = true
				break
			}
		}
		if !found {
			err = errMissingPinnedEntry
		}

	}
	v.pinned = pinned
	return err
}

// Pin sets a given transaction group as pinned items, after they have already been verified.
/*
	이미 검증된 트랜잭션 그룹을 캐시의 pin영역에 넣어준다.
	( 동일한 id를 가지는 bucket영역의 tx를 삭제해준다.)
*/
func (v *verifiedTransactionCache) Pin(txgroup []transactions.SignedTxn) (err error) {
	v.bucketsLock.Lock()
	defer v.bucketsLock.Unlock()
	transactionMissing := false
	if len(v.pinned)+len(txgroup) > maxPinnedEntries {
		// reaching this number likely means that we have an issue not removing entries from the pinned map.
		// return an error ( which would get logged )
		return errTooManyPinnedEntries
	}
	baseBucket := v.base
	for _, txn := range txgroup {
		txID := txn.ID()
		if _, has := v.pinned[txID]; has {
			// it's already pinned; keep going.
			continue
		}

		// entry isn't in pinned; maybe we have it in one of the buckets ?
		found := false
		// we use the (base + W) % W trick here so we can go backward and wrap around the zero.
		/*
			bucket에 있는 tx를 지우고 pin에 넣어준다.
		*/
		for offsetBucketIdx := baseBucket + len(v.buckets); offsetBucketIdx > baseBucket; offsetBucketIdx-- {
			bucketIdx := offsetBucketIdx % len(v.buckets)
			if ctx, has := v.buckets[bucketIdx][txID]; has {
				// move it to the pinned items :
				v.pinned[txID] = ctx
				delete(v.buckets[bucketIdx], txID)
				found = true
				baseBucket = bucketIdx
				break
			}
		}
		if !found {
			transactionMissing = true
		}
	}
	if transactionMissing {
		err = errMissingPinnedEntry
	}
	return
}

// add is the internal implementation of Add/AddPayset which adds a transaction group to the buffer.
func (v *verifiedTransactionCache) add(txgroup []transactions.SignedTxn, groupCtx *GroupContext) {
	if len(v.buckets[v.base])+len(txgroup) > v.entriesPerBucket {
		// move to the next bucket while deleting the content of the next bucket.
		v.base = (v.base + 1) % len(v.buckets)
		v.buckets[v.base] = make(map[transactions.Txid]*GroupContext, v.entriesPerBucket)
	}
	currentBucket := v.buckets[v.base]
	for _, txn := range txgroup {
		currentBucket[txn.ID()] = groupCtx
	}
}

var alwaysVerifiedCache = mockedCache{true}
var neverVerifiedCache = mockedCache{false}

type mockedCache struct {
	alwaysVerified bool
}

func (v *mockedCache) Add(txgroup []transactions.SignedTxn, groupCtx *GroupContext) {
	return
}

func (v *mockedCache) AddPayset(txgroup [][]transactions.SignedTxn, groupCtxs []*GroupContext) error {
	return nil
}

func (v *mockedCache) GetUnverifiedTranscationGroups(txnGroups [][]transactions.SignedTxn, currSpecAddrs transactions.SpecialAddresses, currProto protocol.ConsensusVersion) (unverifiedGroups [][]transactions.SignedTxn) {
	if v.alwaysVerified {
		return nil
	}
	return txnGroups
}

func (v *mockedCache) UpdatePinned(pinnedTxns map[transactions.Txid]transactions.SignedTxn) (err error) {
	return nil
}

func (v *mockedCache) Pin(txgroup []transactions.SignedTxn) (err error) {
	return nil
}

// GetMockedCache returns a mocked transaction cache implementation
func GetMockedCache(alwaysVerified bool) VerifiedTransactionCache {
	if alwaysVerified {
		return &alwaysVerifiedCache
	}
	return &neverVerifiedCache
}
