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

package ledger

import (
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/logging"
)

// lruAccounts provides a storage class for the most recently used accounts data.
// It doesn't have any synchronization primitive on it's own and require to be
// syncronized by the caller.
/*
	lru 계정은 가장 최근에 사용한 계정 데이터에 대한 저장소 클래스를 제공합니다.
	자체적으로 동기화 프리미티브가 없으며 호출자가 동기화해야 합니다.
*/
type lruAccounts struct {
	// accountsList contain the list of persistedAccountData, where the front ones are the most "fresh"
	// and the ones on the back are the oldest.
	/*
		AccountsList는 persistedAccountData의 목록을 포함합니다.
		가장 앞의 것이 제일 나중에 저장된 것이고 가장 뒤의 것이 첫번째 저장된 것이다(LIFO)
	*/
	accountsList *persistedAccountDataList

	// accounts provides fast access to the various elements in the list by using the account address
	accounts map[basics.Address]*persistedAccountDataListNode
	// pendingAccounts are used as a way to avoid taking a write-lock. When the caller needs to "materialize" these,
	// it would call flushPendingWrites and these would be merged into the accounts/accountsList
	/*
		pendingAccounts는 쓰기 잠금을 방지하는 방법으로 사용됩니다.
		호출자가 이것을 "구체화"해야 할 때 flushPendingWrites를 호출하고 이것들은 accounts/accountsList에 병합됩니다.
		=> 아 고루틴간에 persistedAccountData를 전송할 때 LOCK되는걸 방지하기 위해 채널을 통해 전송하고(채널 전송이 완료 될 때까지
			양쪽의 고루틴은 다른 동작을 하지 않아서 LOCK 안걸림) 그 후에 accounts/accountsList에 저장 하는 듯?
	*/
	pendingAccounts chan persistedAccountData
	// log interface; used for logging the threshold event.
	log logging.Logger
	// pendingWritesWarnThreshold is the threshold beyond we would write a warning for exceeding the number of pendingAccounts entries
	pendingWritesWarnThreshold int
}

// init initializes the lruAccounts for use.
// thread locking semantics : write lock
func (m *lruAccounts) init(log logging.Logger, pendingWrites int, pendingWritesWarnThreshold int) {
	m.accountsList = newPersistedAccountList().allocateFreeNodes(pendingWrites)
	m.accounts = make(map[basics.Address]*persistedAccountDataListNode, pendingWrites)
	m.pendingAccounts = make(chan persistedAccountData, pendingWrites)
	m.log = log
	m.pendingWritesWarnThreshold = pendingWritesWarnThreshold
}

// read the persistedAccountData object that the lruAccounts has for the given address.
// thread locking semantics : read lock
func (m *lruAccounts) read(addr basics.Address) (data persistedAccountData, has bool) {
	if el := m.accounts[addr]; el != nil {
		return *el.Value, true
	}
	return persistedAccountData{}, false
}

// flushPendingWrites flushes the pending writes to the main lruAccounts cache.
// thread locking semantics : write lock
func (m *lruAccounts) flushPendingWrites() {
	pendingEntriesCount := len(m.pendingAccounts)
	if pendingEntriesCount >= m.pendingWritesWarnThreshold {
		m.log.Warnf("lruAccounts: number of entries in pendingAccounts(%d) exceed the warning threshold of %d", pendingEntriesCount, m.pendingWritesWarnThreshold)
	}
	for ; pendingEntriesCount > 0; pendingEntriesCount-- {
		select {
		case pendingAccountData := <-m.pendingAccounts:
			m.write(pendingAccountData)
		default:
			return
		}
	}
	return
}

// writePending write a single persistedAccountData entry to the pendingAccounts buffer.
// the function doesn't block, and in case of a buffer overflow the entry would not be added.
// thread locking semantics : no lock is required.
func (m *lruAccounts) writePending(acct persistedAccountData) {
	select {
	case m.pendingAccounts <- acct:
	default:
	}
}

// write a single persistedAccountData to the lruAccounts cache.
// when writing the entry, the round number would be used to determine if it's a newer
// version of what's already on the cache or not. In all cases, the entry is going
// to be promoted to the front of the list.
// thread locking semantics : write lock
func (m *lruAccounts) write(acctData persistedAccountData) {
	if el := m.accounts[acctData.addr]; el != nil {
		// already exists; is it a newer ?
		if el.Value.before(&acctData) {
			// we update with a newer version.
			el.Value = &acctData
		}
		m.accountsList.moveToFront(el)
	} else {
		// new entry.
		m.accounts[acctData.addr] = m.accountsList.pushFront(&acctData)
	}
}

// prune adjust the current size of the lruAccounts cache, by dropping the least
// recently used entries.
// thread locking semantics : write lock
func (m *lruAccounts) prune(newSize int) (removed int) {
	for {
		if len(m.accounts) <= newSize {
			break
		}
		back := m.accountsList.back()
		delete(m.accounts, back.Value.addr)
		m.accountsList.remove(back)
		removed++
	}
	return
}
