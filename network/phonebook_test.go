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
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPhonebookAll(t *testing.T, set []string, ph Phonebook) {
	actual := ph.GetAddresses(len(set), PhoneBookEntryRelayRole)
	for _, got := range actual {
		ok := false
		for _, known := range set {
			if got == known {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("get returned junk %#v", got)
		}
	}
	for _, known := range set {
		ok := false
		for _, got := range actual {
			if got == known {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("get missed %#v; actual=%#v; set=%#v", known, actual, set)
		}
	}
}

func testPhonebookUniform(t *testing.T, set []string, ph Phonebook, getsize int) {
	uniformityTestLength := 250000 / len(set)
	expected := (uniformityTestLength * getsize) / len(set)
	counts := make([]int, len(set))
	for i := 0; i < uniformityTestLength; i++ {
		actual := ph.GetAddresses(getsize, PhoneBookEntryRelayRole)
		for i, known := range set {
			for _, xa := range actual {
				if known == xa {
					counts[i]++
				}
			}
		}
	}
	min := counts[0]
	max := counts[0]
	for i := 1; i < len(counts); i++ {
		if counts[i] > max {
			max = counts[i]
		}
		if counts[i] < min {
			min = counts[i]
		}
	}
	// TODO: what's a good probability-theoretic threshold for good enough?
	if max-min > (expected / 5) {
		t.Errorf("counts %#v", counts)
	}
}

func TestArrayPhonebookAll(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e"}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	for _, e := range set {
		ph.data[e] = makePhonebookEntryData("", PhoneBookEntryRelayRole)
	}
	testPhonebookAll(t, set, ph)
}

func TestArrayPhonebookUniform1(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e"}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	for _, e := range set {
		ph.data[e] = makePhonebookEntryData("", PhoneBookEntryRelayRole)
	}
	testPhonebookUniform(t, set, ph, 1)
}

func TestArrayPhonebookUniform3(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e"}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	for _, e := range set {
		ph.data[e] = makePhonebookEntryData("", PhoneBookEntryRelayRole)
	}
	testPhonebookUniform(t, set, ph, 3)
}

// TestPhonebookExtension tests for extending different phonebooks with addresses.
// TestPhonebookExtension은 주소가 있는 다른 전화번호부를 확장하기 위한 테스트입니다.
func TestPhonebookExtension(t *testing.T) {
	partitiontest.PartitionTest(t)

	setA := []string{"a"}
	moreB := []string{"b"}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	ph.ReplacePeerList(setA, "default", PhoneBookEntryRelayRole)
	ph.ExtendPeerList(moreB, "default", PhoneBookEntryRelayRole)
	ph.ExtendPeerList(setA, "other", PhoneBookEntryRelayRole)
	assert.Equal(t, 2, ph.Length())
	assert.Equal(t, true, ph.data["a"].networkNames["default"])
	assert.Equal(t, true, ph.data["a"].networkNames["other"])
	assert.Equal(t, true, ph.data["b"].networkNames["default"])
	assert.Equal(t, false, ph.data["b"].networkNames["other"])
}

func extenderThread(th *phonebookImpl, more []string, wg *sync.WaitGroup, repetitions int) {
	defer wg.Done()
	for i := 0; i <= repetitions; i++ {
		start := rand.Intn(len(more))
		end := rand.Intn(len(more)-start) + start
		th.ExtendPeerList(more[start:end], "default", PhoneBookEntryRelayRole)
	}
	th.ExtendPeerList(more, "default", PhoneBookEntryRelayRole)
}

func TestThreadsafePhonebookExtension(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e"}
	more := []string{"f", "g", "h", "i", "j"}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	ph.ReplacePeerList(set, "default", PhoneBookEntryRelayRole)
	wg := sync.WaitGroup{}
	wg.Add(5)
	for ti := 0; ti < 5; ti++ {
		go extenderThread(ph, more, &wg, 1000)
	}
	wg.Wait()

	assert.Equal(t, 10, ph.Length())
}

func threadTestThreadsafePhonebookExtensionLong(wg *sync.WaitGroup, ph *phonebookImpl, setSize, repetitions int) {
	set := make([]string, setSize)
	for i := range set {
		set[i] = fmt.Sprintf("%06d", i)
	}
	rand.Shuffle(len(set), func(i, j int) { t := set[i]; set[i] = set[j]; set[j] = t })
	extenderThread(ph, set, wg, repetitions)
}

func TestThreadsafePhonebookExtensionLong(t *testing.T) {
	partitiontest.PartitionTest(t)

	if testing.Short() {
		t.SkipNow()
		return
	}
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	wg := sync.WaitGroup{}
	const threads = 5
	const setSize = 1000
	const repetitions = 100
	wg.Add(threads)
	for i := 0; i < threads; i++ {
		go threadTestThreadsafePhonebookExtensionLong(&wg, ph, setSize, repetitions)
	}

	wg.Wait()

	assert.Equal(t, setSize, ph.Length())
}

func TestMultiPhonebook(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	pha := make([]string, 0)
	for _, e := range set[:5] {
		pha = append(pha, e)
	}
	phb := make([]string, 0)
	for _, e := range set[5:] {
		phb = append(phb, e)
	}
	mp := MakePhonebook(1, 1*time.Millisecond)
	mp.ReplacePeerList(pha, "pha", PhoneBookEntryRelayRole)
	mp.ReplacePeerList(phb, "phb", PhoneBookEntryRelayRole)

	testPhonebookAll(t, set, mp)
	testPhonebookUniform(t, set, mp, 1)
	testPhonebookUniform(t, set, mp, 3)
}

func TestMultiPhonebookDuplicateFiltering(t *testing.T) {
	partitiontest.PartitionTest(t)

	set := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	pha := make([]string, 0)
	for _, e := range set[:7] {
		pha = append(pha, e)
	}
	phb := make([]string, 0)
	for _, e := range set[3:] {
		phb = append(phb, e)
	}
	mp := MakePhonebook(1, 1*time.Millisecond)
	mp.ReplacePeerList(pha, "pha", PhoneBookEntryRelayRole)
	mp.ReplacePeerList(phb, "phb", PhoneBookEntryRelayRole)

	testPhonebookAll(t, set, mp)
	testPhonebookUniform(t, set, mp, 1)
	testPhonebookUniform(t, set, mp, 3)
}

func TestWaitAndAddConnectionTimeLongtWindow(t *testing.T) {
	partitiontest.PartitionTest(t)

	// make the connectionsRateLimitingWindow long enough to avoid triggering it when the test is running in a slow environment
	// 테스트가 느린 환경에서 실행될 때 트리거되는 것을 피하기 위해 connectionsRateLimitingWindow를 충분히 길게 만듭니다.
	// The test will artificially simulate time passing
	// 테스트는 시간 경과를 인위적으로 시뮬레이션합니다.
	timeUnit := 2000 * time.Second
	connectionsRateLimitingWindow := 2 * timeUnit
	entries := MakePhonebook(3, connectionsRateLimitingWindow).(*phonebookImpl)
	addr1 := "addrABC"
	addr2 := "addrXYZ"

	// Address not in. Should return false
	// 주소가 없습니다. false를 반환해야 합니다.
	addrInPhonebook, _, provisionalTime := entries.GetConnectionWaitTime(addr1)
	require.Equal(t, false, addrInPhonebook)
	require.Equal(t, false, entries.UpdateConnectionTime(addr1, provisionalTime))

	// Test the addresses are populated in the phonebook and a time can be added to one of them
	// 전화번호부에 주소가 채워져 있는지 테스트하고 그 중 하나에 시간을 추가할 수 있습니다.
	entries.ReplacePeerList([]string{addr1, addr2}, "default", PhoneBookEntryRelayRole)
	addrInPhonebook, waitTime, provisionalTime := entries.GetConnectionWaitTime(addr1)
	require.Equal(t, true, addrInPhonebook)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))
	phBookData := entries.data[addr1].recentConnectionTimes
	require.Equal(t, 1, len(phBookData))

	// simulate passing a unit of time
	// 시간 단위를 전달하는 시뮬레이션
	for rct := range entries.data[addr1].recentConnectionTimes {
		entries.data[addr1].recentConnectionTimes[rct] = entries.data[addr1].recentConnectionTimes[rct].Add(-1 * timeUnit)
	}

	// add another value to addr
	// addr에 다른 값 추가
	addrInPhonebook, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr1)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))
	phBookData = entries.data[addr1].recentConnectionTimes
	require.Equal(t, 2, len(phBookData))

	// simulate passing a unit of time
	// 시간 단위를 전달하는 시뮬레이션
	for rct := range entries.data[addr1].recentConnectionTimes {
		entries.data[addr1].recentConnectionTimes[rct] =
			entries.data[addr1].recentConnectionTimes[rct].Add(-1 * timeUnit)
	}

	// the first time should be removed and a new one added there should not be any wait
	// 첫 번째 시간은 제거되고 새 항목이 추가되어야 대기가 없어야 합니다.
	addrInPhonebook, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr1)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))
	phBookData2 := entries.data[addr1].recentConnectionTimes
	require.Equal(t, 2, len(phBookData2))

	// make sure the right time was removed
	// 올바른 시간이 제거되었는지 확인
	require.Equal(t, phBookData[1], phBookData2[0])
	require.Equal(t, true, phBookData2[0].Before(phBookData2[1]))

	// try requesting from another address, make sure
	// a separate array is used for these new requests
	// 다른 주소에서 요청을 시도합니다.
	// 이러한 새 요청에는 별도의 배열이 사용됩니다.

	// add 3 values to another address. should not wait value 1
	// 다른 주소에 3개의 값을 추가합니다. 값 1을 기다리지 않아야 함
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr2)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr2, provisionalTime))

	// introduce a gap between the two requests so that only the first will be removed later when waited simulate passing a unit of time
	// 두 요청 사이에 간격을 만들어 대기 시 첫 번째 요청만 나중에 제거되도록 시간 단위 통과 시뮬레이트
	for rct := range entries.data[addr2].recentConnectionTimes {
		entries.data[addr2].recentConnectionTimes[rct] =
			entries.data[addr2].recentConnectionTimes[rct].Add(-1 * timeUnit)
	}

	// value 2
	addrInPhonebook, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr2)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr2, provisionalTime))
	// value 3
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr2)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr2, provisionalTime))

	phBookData = entries.data[addr2].recentConnectionTimes
	// all three times should be queued
	// 세 번 모두 대기열에 넣어야 합니다.
	require.Equal(t, 3, len(phBookData))

	// add another element to trigger wait
	// 대기를 트리거하기 위해 다른 요소를 추가합니다.
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr2)
	require.Greater(t, int64(waitTime), int64(0))
	// no element should be removed
	// 제거할 요소가 없어야 함
	phBookData2 = entries.data[addr2].recentConnectionTimes
	require.Equal(t, phBookData[0], phBookData2[0])
	require.Equal(t, phBookData[1], phBookData2[1])
	require.Equal(t, phBookData[2], phBookData2[2])

	// simulate passing of the waitTime duration
	// waitTime 지속 시간의 전달을 시뮬레이션합니다.
	for rct := range entries.data[addr2].recentConnectionTimes {
		entries.data[addr2].recentConnectionTimes[rct] =
			entries.data[addr2].recentConnectionTimes[rct].Add(-1 * waitTime)
	}

	// The wait should be sufficient
	// 기다림은 충분해야 합니다.
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr2)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr2, provisionalTime))
	// only one element should be removed, and one added
	// 하나의 요소만 제거하고 하나의 요소를 추가해야 합니다.
	phBookData2 = entries.data[addr2].recentConnectionTimes
	require.Equal(t, 3, len(phBookData2))

	// make sure the right time was removed
	// 올바른 시간이 제거되었는지 확인
	require.Equal(t, phBookData[1], phBookData2[0])
	require.Equal(t, phBookData[2], phBookData2[1])
}

func TestWaitAndAddConnectionTimeShortWindow(t *testing.T) {
	partitiontest.PartitionTest(t)

	entries := MakePhonebook(3, 2*time.Millisecond).(*phonebookImpl)
	addr1 := "addrABC"

	// Init the data structures
	// 데이터 구조 초기화
	entries.ReplacePeerList([]string{addr1}, "default", PhoneBookEntryRelayRole)

	// add 3 values. should not wait
	// value 1
	addrInPhonebook, waitTime, provisionalTime := entries.GetConnectionWaitTime(addr1)
	require.Equal(t, true, addrInPhonebook)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))
	// value 2
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr1)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))
	// value 3
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr1)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))

	// give enough time to expire all the elements
	// 모든 요소가 만료되기에 충분한 시간을 줍니다.
	time.Sleep(10 * time.Millisecond)

	// there should not be any wait
	// 대기가 없어야 함
	_, waitTime, provisionalTime = entries.GetConnectionWaitTime(addr1)
	require.Equal(t, time.Duration(0), waitTime)
	require.Equal(t, true, entries.UpdateConnectionTime(addr1, provisionalTime))

	// only one time should be left (the newly added)
	// 한 번만 남겨야 함(새로 추가됨)
	phBookData := entries.data[addr1].recentConnectionTimes
	require.Equal(t, 1, len(phBookData))
}

func BenchmarkThreadsafePhonebook(b *testing.B) {
	ph := MakePhonebook(1, 1).(*phonebookImpl)
	threads := 5
	if b.N < threads {
		threads = b.N
	}
	wg := sync.WaitGroup{}
	wg.Add(threads)
	repetitions := b.N / threads
	for t := 0; t < threads; t++ {
		go threadTestThreadsafePhonebookExtensionLong(&wg, ph, 1000, repetitions)
	}
	wg.Wait()
}

// TestPhonebookRoles tests that the filtering by roles for different phonebooks entries works as expected.
// TestPhonebookRoles는 다른 전화번호부 항목에 대한 역할별 필터링이 예상대로 작동하는지 테스트합니다.
func TestPhonebookRoles(t *testing.T) {
	partitiontest.PartitionTest(t)

	relaysSet := []string{"relay1", "relay2", "relay3"}
	archiverSet := []string{"archiver1", "archiver2", "archiver3"}

	ph := MakePhonebook(1, 1).(*phonebookImpl)
	ph.ReplacePeerList(relaysSet, "default", PhoneBookEntryRelayRole)
	ph.ReplacePeerList(archiverSet, "default", PhoneBookEntryArchiverRole)
	require.Equal(t, len(relaysSet)+len(archiverSet), len(ph.data))
	require.Equal(t, len(relaysSet)+len(archiverSet), ph.Length())

	for _, role := range []PhoneBookEntryRoles{PhoneBookEntryRelayRole, PhoneBookEntryArchiverRole} {
		for k := 0; k < 100; k++ {
			for l := 0; l < 3; l++ {
				entries := ph.GetAddresses(l, role)
				if role == PhoneBookEntryRelayRole {
					for _, entry := range entries {
						require.Contains(t, entry, "relay")
					}
				} else if role == PhoneBookEntryArchiverRole {
					for _, entry := range entries {
						require.Contains(t, entry, "archiver")
					}
				}
			}
		}
	}
}
