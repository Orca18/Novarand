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
	"math"
	"math/rand"
	"time"

	"github.com/algorand/go-deadlock"
)

// when using GetAddresses with getAllAddresses, all the addresses will be retrieved, regardless of how many addresses the phonebook actually has. ( with the retry-after logic applied )
// getAllAddresses와 함께 GetAddresses를 사용할 때 전화번호부의 실제 주소 수에 관계없이 모든 주소가 검색됩니다. ( 재시도 후 로직이 적용된 경우 )
const getAllAddresses = math.MaxInt32

// PhoneBookEntryRoles defines the roles that a single entry on the phonebook can take.
// currently, we have two roles : relay role and archiver role, which are mutually exclusive.
// PhoneBookEntryRoles는 전화번호부의 단일 항목이 취할 수 있는 역할을 정의합니다.
// 현재 두 가지 역할이 있습니다. 즉, 릴레이 역할과 아카이버 역할은 상호 배타적입니다.
type PhoneBookEntryRoles int

// PhoneBookEntryRelayRole used for all the relays that are provided either via the algobootstrap SRV record or via a configuration file.
// algobootstrap SRV 레코드 또는 구성 파일을 통해 제공되는 모든 릴레이에 사용되는 PhoneBookEntryRelayRole.
const PhoneBookEntryRelayRole = 1

// PhoneBookEntryArchiverRole used for all the archivers that are provided via the archive SRV record.
// 아카이브 SRV 레코드를 통해 제공되는 모든 아카이버에 사용되는 PhoneBookEntryArchiverRole.
const PhoneBookEntryArchiverRole = 2

// Phonebook stores or looks up addresses of nodes we might contact
// 전화번호부는 우리가 접촉할 수 있는 노드의 주소를 저장하거나 조회합니다.
type Phonebook interface {
	// GetAddresses(N) returns up to N addresses, but may return fewer
	// GetAddresses(N)은 최대 N개의 주소를 반환하지만 더 적은 수를 반환할 수 있습니다.
	GetAddresses(n int, role PhoneBookEntryRoles) []string

	// UpdateRetryAfter updates the retry-after field for the entries matching the given address
	// UpdateRetryAfter는 주어진 주소와 일치하는 항목에 대한 retry-after 필드를 업데이트합니다.
	UpdateRetryAfter(addr string, retryAfter time.Time)

	// GetConnectionWaitTime will calculate and return the wait time to prevent exceeding connectionsRateLimitingCount.
	// The connection should be established when the waitTime is 0.
	// It will register a provisional next connection time when the waitTime is 0.
	// The provisional time should be updated after the connection with UpdateConnectionTime\
	// GetConnectionWaitTime은 connectionsRateLimitingCount 초과를 방지하기 위해 대기 시간을 계산하고 반환합니다.
	// waitTime이 0일 때 연결이 설정되어야 합니다.
	// waitTime이 0일 때 잠정적으로 다음 연결 시간을 등록합니다.
	// UpdateConnectionTime과 연결한 후 임시 시간을 업데이트해야 합니다.
	GetConnectionWaitTime(addr string) (addrInPhonebook bool,
		waitTime time.Duration, provisionalTime time.Time)

	// UpdateConnectionTime will update the provisional connection time.
	// Returns true of the addr was in the phonebook
	// UpdateConnectionTime은 임시 연결 시간을 업데이트합니다.
	// addr가 전화번호부에 있었다면 true를 반환합니다.
	UpdateConnectionTime(addr string, provisionalTime time.Time) bool

	// ReplacePeerList merges a set of addresses with that passed in for networkName
	// new entries in dnsAddresses are being added
	// existing items that aren't included in dnsAddresses are being removed
	// matching entries don't change
	// ReplacePeerList는 networkName에 대해 전달된 주소 세트를 병합합니다.
	// dnsAddresses의 새 항목이 추가되고 있습니다.
	// dnsAddresses에 포함되지 않은 기존 항목은 제거 중입니다.
	// 일치하는 항목은 변경되지 않음
	ReplacePeerList(dnsAddresses []string, networkName string, role PhoneBookEntryRoles)

	// ExtendPeerList adds unique addresses to this set of addresses
	// ExtendPeerList는 이 주소 집합에 고유 주소를 추가합니다.
	ExtendPeerList(more []string, networkName string, role PhoneBookEntryRoles)
}

// addressData: holds the information associated with each phonebook address.
// addressData: 각 전화번호부 주소와 관련된 정보를 보유합니다.
type addressData struct {
	// retryAfter is the time to wait before retrying to connect to the address.
	// retryAfter는 해당 주소에 다시 연결을 시도하기 전에 대기하는 시간입니다.
	retryAfter time.Time

	// recentConnectionTimes: is the log of connection times used to observe the maximum connections to the address in a given time window.
	// RecentConnectionTimes: 주어진 시간 창에서 주소에 대한 최대 연결을 관찰하는 데 사용된 연결 시간의 로그입니다.
	recentConnectionTimes []time.Time

	// networkNames: lists the networks to which the given address belongs.
	// networkNames: 주어진 주소가 속한 네트워크를 나열합니다.
	networkNames map[string]bool

	// role is the role that this address serves.
	// role은 이 주소가 제공하는 역할입니다.
	role PhoneBookEntryRoles
}

// makePhonebookEntryData creates a new addressData entry for provided network name and role. /n
// makePhonebookEntryData는 제공된 네트워크 이름과 역할에 대한 새 addressData 항목을 만듭니다.
func makePhonebookEntryData(networkName string, role PhoneBookEntryRoles) addressData {
	pbData := addressData{
		networkNames:          make(map[string]bool),
		recentConnectionTimes: make([]time.Time, 0),
		role:                  role,
	}
	pbData.networkNames[networkName] = true
	return pbData
}

// phonebookImpl holds the server connection configuration values and the list of request times within the time window for each address.
// phonebookImpl은 각 주소에 대한 시간 창 내 서버 연결 구성 값과 요청 시간 목록을 보유합니다.
type phonebookImpl struct {
	connectionsRateLimitingCount  uint
	connectionsRateLimitingWindow time.Duration
	data                          map[string]addressData
	lock                          deadlock.RWMutex
}

// MakePhonebook creates phonebookImpl with the passed configuration values
// MakePhonebook은 전달된 구성 값으로 phonebookImpl을 생성합니다.
func MakePhonebook(connectionsRateLimitingCount uint,
	connectionsRateLimitingWindow time.Duration) Phonebook {
	return &phonebookImpl{
		connectionsRateLimitingCount:  connectionsRateLimitingCount,
		connectionsRateLimitingWindow: connectionsRateLimitingWindow,
		data:                          make(map[string]addressData, 0),
	}
}

func (e *phonebookImpl) deletePhonebookEntry(entryName, networkName string) {
	pbEntry := e.data[entryName]
	delete(pbEntry.networkNames, networkName)
	if 0 == len(pbEntry.networkNames) {
		delete(e.data, entryName)
	}
}

// PopEarliestTime removes the earliest time from recentConnectionTimes in addressData for addr
// It is expected to be later than ConnectionsRateLimitingWindow
// PopEarliestTime은 addr에 대한 addressData의 RecentConnectionTimes에서 가장 빠른 시간을 제거합니다.
// ConnectionRateLimitingWindow 이후일 것으로 예상됩니다.
func (e *phonebookImpl) popNElements(n int, addr string) {
	entry := e.data[addr]
	entry.recentConnectionTimes = entry.recentConnectionTimes[n:]
	e.data[addr] = entry
}

// AppendTime adds the current time to recentConnectionTimes in addressData of addr
// AppendTime은 addr의 addressData에 있는 RecentConnectionTimes에 현재 시간을 추가합니다.
func (e *phonebookImpl) appendTime(addr string, t time.Time) {
	entry := e.data[addr]
	entry.recentConnectionTimes = append(entry.recentConnectionTimes, t)
	e.data[addr] = entry
}

func (e *phonebookImpl) filterRetryTime(t time.Time, role PhoneBookEntryRoles) []string {
	o := make([]string, 0, len(e.data))
	for addr, entry := range e.data {
		if t.After(entry.retryAfter) && role == entry.role {
			o = append(o, addr)
		}
	}
	return o
}

// ReplacePeerList merges a set of addresses with that passed in.
// new entries in addressesThey are being added existing items that aren't included in addressesThey are being removed matching entries don't change
// ReplacePeerList는 전달된 주소 집합을 병합합니다.
// 주소에 새 항목이 추가되고 있습니다. 주소에 포함되지 않은 기존 항목이 제거됩니다. 일치하는 항목은 변경되지 않습니다.
func (e *phonebookImpl) ReplacePeerList(addressesThey []string, networkName string, role PhoneBookEntryRoles) {
	e.lock.Lock()
	defer e.lock.Unlock()

	// prepare a map of items we'd like to remove.
	// 제거하려는 항목의 맵을 준비합니다.
	removeItems := make(map[string]bool, 0)
	for k, pbd := range e.data {
		if pbd.networkNames[networkName] && pbd.role == role {
			removeItems[k] = true
		}
	}

	for _, addr := range addressesThey {
		if pbData, has := e.data[addr]; has {
			// we already have this.
			// Update the networkName
			// 이미 가지고 있습니다.
			// networkName 업데이트
			pbData.networkNames[networkName] = true

			// do not remove this entry
			// 이 항목을 제거하지 마십시오.
			delete(removeItems, addr)
		} else {
			// we don't have this item. add it.
			// 이 항목이 없습니다. 그것을 추가하십시오.
			e.data[addr] = makePhonebookEntryData(networkName, role)
		}
	}

	// remove items that were missing in addressesThey
	// 주소에서 누락된 항목을 제거합니다.
	for k := range removeItems {
		e.deletePhonebookEntry(k, networkName)
	}
}

func (e *phonebookImpl) UpdateRetryAfter(addr string, retryAfter time.Time) {
	e.lock.Lock()
	defer e.lock.Unlock()

	var entry addressData

	entry, found := e.data[addr]
	if !found {
		return
	}
	entry.retryAfter = retryAfter
	e.data[addr] = entry
}

// GetConnectionWaitTime will calculate and return the wait time to prevent exceeding connectionsRateLimitingCount.
// The connection should be established when the waitTime is 0.
// It will register a provisional next connection time when the waitTime is 0.
// The provisional time should be updated after the connection with UpdateConnectionTime
// GetConnectionWaitTime은 connectionsRateLimitingCount 초과를 방지하기 위해 대기 시간을 계산하고 반환합니다.
// waitTime이 0일 때 연결이 설정되어야 합니다.
// waitTime이 0일 때 잠정적으로 다음 연결 시간을 등록합니다.
// UpdateConnectionTime과 연결한 후 임시 시간을 업데이트해야 합니다.
func (e *phonebookImpl) GetConnectionWaitTime(addr string) (addrInPhonebook bool,
	waitTime time.Duration, provisionalTime time.Time) {
	e.lock.Lock()
	defer e.lock.Unlock()

	_, addrInPhonebook = e.data[addr]
	curTime := time.Now()
	if !addrInPhonebook {
		// The addr is not in this phonebook.
		// Will find the addr in a different phonebook.
		// 이 전화번호부에 해당 주소가 없습니다.
		// 다른 전화번호부에서 addr를 찾습니다.
		return addrInPhonebook, 0 /* not used */, curTime /* not used */
	}

	var timeSince time.Duration
	var numElmtsToRemove int
	// Remove from recentConnectionTimes the times later than ConnectionsRateLimitingWindowSeconds
	// 최근ConnectionTimes에서 ConnectionsRateLimitingWindowSeconds보다 늦은 시간 제거
	for numElmtsToRemove < len(e.data[addr].recentConnectionTimes) {
		timeSince = curTime.Sub((e.data[addr].recentConnectionTimes)[numElmtsToRemove])
		if timeSince >= e.connectionsRateLimitingWindow {
			numElmtsToRemove++
		} else {
			break // break the loop. The rest are earlier than 1 second
		}
	}
	// Remove the expired elements from e.data[addr].recentConnectionTimes
	// e.data[addr].recentConnectionTimes에서 만료된 요소를 제거합니다.
	e.popNElements(numElmtsToRemove, addr)

	// If there are max number of connections within the time window, wait
	// 시간 창 내에 최대 연결 수가 있는 경우 대기
	numElts := len(e.data[addr].recentConnectionTimes)
	if uint(numElts) >= e.connectionsRateLimitingCount {
		return addrInPhonebook, /* true */
			(e.connectionsRateLimitingWindow - timeSince), curTime /* not used */
	}

	// Else, there is space in connectionsRateLimitingCount.
	// The connection request of the caller will proceed
	// Update curTime, since it may have significantly changed if waited
	// 그렇지 않으면 connectionsRateLimitingCount에 공간이 있습니다.
	// 호출자의 연결 요청이 진행됩니다.
	// 기다리면 크게 변경되었을 수 있으므로 curTime을 업데이트합니다.
	provisionalTime = time.Now()
	// Append the provisional time for the next connection request
	// 다음 연결 요청을 위한 잠정 시간 추가
	e.appendTime(addr, provisionalTime)
	return addrInPhonebook /* true */, 0 /* no wait. proceed */, provisionalTime
}

// UpdateConnectionTime will update the provisional connection time.
// Returns true of the addr was in the phonebook
// UpdateConnectionTime은 임시 연결 시간을 업데이트합니다.
// addr가 전화번호부에 있었다면 true를 반환합니다.
func (e *phonebookImpl) UpdateConnectionTime(addr string, provisionalTime time.Time) bool {
	e.lock.Lock()
	defer e.lock.Unlock()

	entry, found := e.data[addr]
	if !found {
		return false
	}

	defer func() {
		e.data[addr] = entry
	}()

	// Find the provisionalTime and update it
	for indx, val := range entry.recentConnectionTimes {
		if provisionalTime == val {
			entry.recentConnectionTimes[indx] = time.Now()
			return true
		}
	}
	// Case where the time is not found: it was removed from the list.
	// This may happen when the time expires before the connection was established with the server.
	// The time should be added again.
	// 시간을 찾을 수 없는 경우: 목록에서 제거되었습니다.
	// 이것은 서버와 연결이 설정되기 전에 시간이 만료될 때 발생할 수 있습니다.
	// 시간을 다시 추가해야 합니다.
	entry.recentConnectionTimes = append(entry.recentConnectionTimes, time.Now())
	return true
}

func shuffleStrings(set []string) {
	rand.Shuffle(len(set), func(i, j int) { t := set[i]; set[i] = set[j]; set[j] = t })
}

func shuffleSelect(set []string, n int) []string {
	if n >= len(set) || n == getAllAddresses {
		// return shuffled copy of everything
		out := make([]string, len(set))
		copy(out, set)
		shuffleStrings(out)
		return out
	}
	// Pick random indexes from the set
	// 집합에서 임의의 인덱스를 선택합니다.
	indexSample := make([]int, n)
	for i := range indexSample {
		indexSample[i] = rand.Intn(len(set)-i) + i
		for oi, ois := range indexSample[:i] {
			if ois == indexSample[i] {
				indexSample[i] = oi
			}
		}
	}
	out := make([]string, n)
	for i, index := range indexSample {
		out[i] = set[index]
	}
	return out
}

// GetAddresses returns up to N shuffled address
// GetAddresses는 최대 N개의 셔플된 주소를 반환합니다.
func (e *phonebookImpl) GetAddresses(n int, role PhoneBookEntryRoles) []string {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return shuffleSelect(e.filterRetryTime(time.Now(), role), n)
}

// ExtendPeerList adds unique addresses to this set of addresses
// ExtendPeerList는 이 주소 집합에 고유 주소를 추가합니다.
func (e *phonebookImpl) ExtendPeerList(more []string, networkName string, role PhoneBookEntryRoles) {
	e.lock.Lock()
	defer e.lock.Unlock()
	for _, addr := range more {
		if pbEntry, has := e.data[addr]; has {
			pbEntry.networkNames[networkName] = true
			continue
		}
		e.data[addr] = makePhonebookEntryData(networkName, role)
	}
}

// Length returns the number of addrs contained
// 길이는 포함된 가산기의 수를 반환합니다.
func (e *phonebookImpl) Length() int {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return len(e.data)
}
