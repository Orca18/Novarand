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

package pools

import (
	"github.com/Orca18/novarand/data/transactions"
)

type statusCacheEntry struct {
	tx    transactions.SignedTxn
	txErr string
}

/*
	에러가 난 경우 해당 tx그룹과 에러의 상태를 저장하는 캐시구나!
*/
type statusCache struct {
	cur  map[transactions.Txid]statusCacheEntry
	prev map[transactions.Txid]statusCacheEntry
	sz   int
}

func makeStatusCache(sz int) *statusCache {
	sc := &statusCache{
		sz: sz,
	}
	sc.reset()
	return sc
}

func (sc *statusCache) check(txid transactions.Txid) (tx transactions.SignedTxn, txErr string, found bool) {
	ent, found := sc.cur[txid]
	if !found {
		ent, found = sc.prev[txid]
	}
	tx = ent.tx
	txErr = ent.txErr
	return
}

/*
	statusCache에 트랜잭션을 넣는다.
	에러가 난 경우 해당 tx그룹과 에러의 상태를 저장하는 캐시구나!
*/
func (sc *statusCache) put(tx transactions.SignedTxn, txErr string) {
	if len(sc.cur) >= sc.sz {
		sc.prev = sc.cur
		sc.cur = map[transactions.Txid]statusCacheEntry{}
	}

	sc.cur[tx.ID()] = statusCacheEntry{
		tx:    tx,
		txErr: txErr,
	}
}

func (sc *statusCache) reset() {
	sc.cur = map[transactions.Txid]statusCacheEntry{}
	sc.prev = map[transactions.Txid]statusCacheEntry{}
}
