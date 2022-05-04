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

package ledgercore

import (
	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/transactions"
)

const (
	accountArrayEntrySize                 = uint64(232) // Measured by BenchmarkBalanceRecord
	accountMapCacheEntrySize              = uint64(64)  // Measured by BenchmarkAcctCache
	txleasesEntrySize                     = uint64(112) // Measured by BenchmarkTxLeases
	creatablesEntrySize                   = uint64(100) // Measured by BenchmarkCreatables
	stateDeltaTargetOptimizationThreshold = uint64(50000000)
)

// ModifiedCreatable defines the changes to a single creatable state
/*
ModifiedCreatable은 단일 creatable state에 대한 변경 사항을 정의합니다
*/
type ModifiedCreatable struct {
	// Type of the creatable: app or asset
	Ctype basics.CreatableType

	// Created if true, deleted if false
	Created bool

	// creator of the app/asset
	Creator basics.Address

	// Keeps track of how many times this app/asset appears in
	// accountUpdates.creatableDeltas
	Ndeltas int
}

// AccountAsset is used as a map key.
type AccountAsset struct {
	Address basics.Address
	Asset   basics.AssetIndex
}

// AccountApp is used as a map key.
type AccountApp struct {
	Address basics.Address
	App     basics.AppIndex
}

// A Txlease is a transaction (sender, lease) pair which uniquely specifies a
// transaction lease.
/*
	Txlease는 트랜잭션의 전송자, lease쌍이다.
	이것은 트랜잭션 lease를 고유하게 지정한다.
*/
type Txlease struct {
	Sender basics.Address
	Lease  [32]byte
}

// StateDelta describes the delta between a given round to the previous round
/*
	StateDelta는 주어진 라운드와 이전 라운드 사이의 델타(차이?)를 나타냅니다.
	아 블록뿐만 아니라 계정, 블록헤더, 등 이전라운드와 현재 라운드의 객체들간의 차이를
	나타내는 구조체!!
*/
type StateDelta struct {
	// modified accounts
	/*
		이전 상태에서 수정된 계정들의 정보
	*/
	// 이전과 현재의 계정 정보를 가지고 있는 배열
	Accts AccountDeltas

	// new Txids for the txtail and TxnCounter, mapped to txn.LastValid
	// the txtail과 TxnCounter 그리고 txn.LastValid에 매핑하기 위한 txId
	Txids map[transactions.Txid]basics.Round

	// new txleases for the txtail mapped to expiration
	// 만료에 매핑된 txtail에 대한 새로운 txleases(new txleases for the txtail mapped to expiration)
	Txleases map[Txlease]basics.Round

	// new creatables creator lookup table
	// 단일 creatable(app혹은 asset) state에 대한 변경 사항
	Creatables map[basics.CreatableIndex]ModifiedCreatable

	// new block header; read-only
	// 새로 생성된 블록의 헤더이며 읽기만 가능
	Hdr *bookkeeping.BlockHeader

	// next round for which we expect a compact cert.
	// zero if no compact cert is expected.
	//우리가 컴팩트 인증서를 기대하는 다음 라운드. 컴팩트 인증서가 예상되지 않으면 0입니다
	CompactCertNext basics.Round

	// previous block timestamp
	// 이전블록의 timestamp
	PrevTimestamp int64

	// Modified local creatable states. The value is true if the creatable local state
	// is created and false if deleted. Used by indexer.
	/*
		local creatable states는 Asset과 App을 말하는 거구나!
		이들이 새로 생성됐다면 true 삭제됐다면 false!
	*/
	ModifiedAssetHoldings  map[AccountAsset]bool
	ModifiedAppLocalStates map[AccountApp]bool

	// initial hint for allocating data structures for StateDelta
	// StateDelta에 대한 데이터 구조 할당을 위한 초기 힌트
	initialTransactionsCount int

	// The account totals reflecting the changes in this StateDelta object.
	// StateDelta를 반영한 총 계정수
	Totals AccountTotals
}

// AccountDeltas stores ordered accounts and allows fast lookup by address
/*
	AccountDeltas는 정렬된 계정들을 저장하고 주소로 빠르게 조회할 수 있게 해준다.
*/
type AccountDeltas struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	// Actual data. If an account is deleted, `accts` contains a balance record
	// with empty `AccountData`.
	accts []basics.BalanceRecord `codec:"accts"`
	// cache for addr to deltas index resolution
	acctsCache map[basics.Address]int `codec:"cache"`
}

// MakeStateDelta creates a new instance of StateDelta.
// hint is amount of transactions for evaluation, 2 * hint is for sender and receiver balance records.
// This does not play well for AssetConfig and ApplicationCall transactions on scale
/*
MakeStateDelta는 StateDelta의 새 인스턴스를 만듭니다.
힌트는 평가를 위한 총 트랜잭션량, 2 * 힌트는 발신자 및 수신자 잔액 기록용입니다.
대규모 AssetConfig 및 ApplicationCall 트랜잭션에는 적합하지 않습니다.
*/
func MakeStateDelta(hdr *bookkeeping.BlockHeader, prevTimestamp int64, hint int, compactCertNext basics.Round) StateDelta {
	return StateDelta{
		Accts: AccountDeltas{
			accts:      make([]basics.BalanceRecord, 0, hint*2),
			acctsCache: make(map[basics.Address]int, hint*2),
		},
		Txids:    make(map[transactions.Txid]basics.Round, hint),
		Txleases: make(map[Txlease]basics.Round, hint),
		// asset or application creation are considered as rare events so do not pre-allocate space for them
		Creatables:               make(map[basics.CreatableIndex]ModifiedCreatable),
		Hdr:                      hdr,
		CompactCertNext:          compactCertNext,
		PrevTimestamp:            prevTimestamp,
		ModifiedAssetHoldings:    make(map[AccountAsset]bool),
		ModifiedAppLocalStates:   make(map[AccountApp]bool),
		initialTransactionsCount: hint,
	}
}

// Get lookups AccountData by address
func (ad *AccountDeltas) Get(addr basics.Address) (basics.AccountData, bool) {
	idx, ok := ad.acctsCache[addr]
	if !ok {
		return basics.AccountData{}, false
	}
	return ad.accts[idx].AccountData, true
}

// ModifiedAccounts returns list of addresses of modified accounts
func (ad *AccountDeltas) ModifiedAccounts() []basics.Address {
	result := make([]basics.Address, len(ad.accts))
	for i := 0; i < len(ad.accts); i++ {
		result[i] = ad.accts[i].Addr
	}
	return result
}

// MergeAccounts applies other accounts into this StateDelta accounts
func (ad *AccountDeltas) MergeAccounts(other AccountDeltas) {
	for new := range other.accts {
		ad.upsert(other.accts[new])
	}
}

// Len returns number of stored accounts
func (ad *AccountDeltas) Len() int {
	return len(ad.accts)
}

// GetByIdx returns address and AccountData
// It does NOT check boundaries.
func (ad *AccountDeltas) GetByIdx(i int) (basics.Address, basics.AccountData) {
	return ad.accts[i].Addr, ad.accts[i].AccountData
}

// Upsert adds new or updates existing account
func (ad *AccountDeltas) Upsert(addr basics.Address, data basics.AccountData) {
	ad.upsert(basics.BalanceRecord{Addr: addr, AccountData: data})
}

func (ad *AccountDeltas) upsert(br basics.BalanceRecord) {
	addr := br.Addr
	if idx, exist := ad.acctsCache[addr]; exist { // nil map lookup is OK
		ad.accts[idx] = br
		return
	}

	last := len(ad.accts)
	ad.accts = append(ad.accts, br)

	if ad.acctsCache == nil {
		ad.acctsCache = make(map[basics.Address]int)
	}
	ad.acctsCache[addr] = last
}

// OptimizeAllocatedMemory by reallocating maps to needed capacity
// For each data structure, reallocate if it would save us at least 50MB aggregate
func (sd *StateDelta) OptimizeAllocatedMemory(proto config.ConsensusParams) {
	// accts takes up 232 bytes per entry, and is saved for 320 rounds
	if uint64(cap(sd.Accts.accts)-len(sd.Accts.accts))*accountArrayEntrySize*proto.MaxBalLookback > stateDeltaTargetOptimizationThreshold {
		accts := make([]basics.BalanceRecord, len(sd.Accts.acctsCache))
		copy(accts, sd.Accts.accts)
		sd.Accts.accts = accts
	}

	// acctsCache takes up 64 bytes per entry, and is saved for 320 rounds
	// realloc if original allocation capacity greater than length of data, and space difference is significant
	if 2*sd.initialTransactionsCount > len(sd.Accts.acctsCache) &&
		uint64(2*sd.initialTransactionsCount-len(sd.Accts.acctsCache))*accountMapCacheEntrySize*proto.MaxBalLookback > stateDeltaTargetOptimizationThreshold {
		acctsCache := make(map[basics.Address]int, len(sd.Accts.acctsCache))
		for k, v := range sd.Accts.acctsCache {
			acctsCache[k] = v
		}
		sd.Accts.acctsCache = acctsCache
	}

	// TxLeases takes up 112 bytes per entry, and is saved for 1000 rounds
	if sd.initialTransactionsCount > len(sd.Txleases) &&
		uint64(sd.initialTransactionsCount-len(sd.Txleases))*txleasesEntrySize*proto.MaxTxnLife > stateDeltaTargetOptimizationThreshold {
		txLeases := make(map[Txlease]basics.Round, len(sd.Txleases))
		for k, v := range sd.Txleases {
			txLeases[k] = v
		}
		sd.Txleases = txLeases
	}

	// Creatables takes up 100 bytes per entry, and is saved for 320 rounds
	if uint64(len(sd.Creatables))*creatablesEntrySize*proto.MaxBalLookback > stateDeltaTargetOptimizationThreshold {
		creatableDeltas := make(map[basics.CreatableIndex]ModifiedCreatable, len(sd.Creatables))
		for k, v := range sd.Creatables {
			creatableDeltas[k] = v
		}
		sd.Creatables = creatableDeltas
	}
}
