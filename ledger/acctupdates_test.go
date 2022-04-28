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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/internal"
	"github.com/Orca18/novarand/ledger/ledgercore"
	ledgertesting "github.com/Orca18/novarand/ledger/testing"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/test/partitiontest"
	"github.com/Orca18/novarand/util/db"
)

var testPoolAddr = basics.Address{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
var testSinkAddr = basics.Address{0x2c, 0x2a, 0x6c, 0xe9, 0xa9, 0xa7, 0xc2, 0x8c, 0x22, 0x95, 0xfd, 0x32, 0x4f, 0x77, 0xa5, 0x4, 0x8b, 0x42, 0xc2, 0xb7, 0xa8, 0x54, 0x84, 0xb6, 0x80, 0xb1, 0xe1, 0x3d, 0x59, 0x9b, 0xeb, 0x36}

type mockLedgerForTracker struct {
	dbs             db.Pair
	blocks          []blockEntry
	deltas          []ledgercore.StateDelta
	log             logging.Logger
	filename        string
	inMemory        bool
	consensusParams config.ConsensusParams
	accts           map[basics.Address]basics.AccountData

	// trackerRegistry manages persistence into DB so we have to have it here even for a single tracker test
	trackers trackerRegistry
}

func accumulateTotals(t testing.TB, consensusVersion protocol.ConsensusVersion, accts []map[basics.Address]basics.AccountData, rewardLevel uint64) (totals ledgercore.AccountTotals) {
	var ot basics.OverflowTracker
	proto := config.Consensus[consensusVersion]
	totals.RewardsLevel = rewardLevel
	for _, ar := range accts {
		for _, data := range ar {
			totals.AddAccount(proto, data, &ot)
		}
	}
	require.False(t, ot.Overflowed)
	return
}

func makeMockLedgerForTrackerWithLogger(t testing.TB, inMemory bool, initialBlocksCount int, consensusVersion protocol.ConsensusVersion, accts []map[basics.Address]basics.AccountData, l logging.Logger) *mockLedgerForTracker {
	dbs, fileName := dbOpenTest(t, inMemory)
	dbs.Rdb.SetLogger(l)
	dbs.Wdb.SetLogger(l)

	blocks := randomInitChain(consensusVersion, initialBlocksCount)
	deltas := make([]ledgercore.StateDelta, initialBlocksCount)
	totals := accumulateTotals(t, consensusVersion, accts, 0)
	for i := range deltas {
		deltas[i] = ledgercore.StateDelta{
			Hdr:    &bookkeeping.BlockHeader{},
			Totals: totals,
		}
	}
	consensusParams := config.Consensus[consensusVersion]
	return &mockLedgerForTracker{dbs: dbs, log: l, filename: fileName, inMemory: inMemory, blocks: blocks, deltas: deltas, consensusParams: consensusParams, accts: accts[0]}

}

func makeMockLedgerForTracker(t testing.TB, inMemory bool, initialBlocksCount int, consensusVersion protocol.ConsensusVersion, accts []map[basics.Address]basics.AccountData) *mockLedgerForTracker {
	dblogger := logging.TestingLog(t)
	dblogger.SetLevel(logging.Info)

	return makeMockLedgerForTrackerWithLogger(t, inMemory, initialBlocksCount, consensusVersion, accts, dblogger)
}

// fork creates another database which has the same content as the current one. Works only for non-memory databases.
func (ml *mockLedgerForTracker) fork(t testing.TB) *mockLedgerForTracker {
	if ml.inMemory {
		return nil
	}
	// create a new random file name.
	fn := fmt.Sprintf("%s.%d", strings.ReplaceAll(t.Name(), "/", "."), crypto.RandUint64())

	dblogger := logging.TestingLog(t)
	dblogger.SetLevel(logging.Info)
	newLedgerTracker := &mockLedgerForTracker{
		inMemory: false,
		log:      dblogger,
		blocks:   make([]blockEntry, len(ml.blocks)),
		deltas:   make([]ledgercore.StateDelta, len(ml.deltas)),
		accts:    make(map[basics.Address]basics.AccountData),
		filename: fn,
	}
	for k, v := range ml.accts {
		newLedgerTracker.accts[k] = v
	}
	copy(newLedgerTracker.blocks, ml.blocks)
	copy(newLedgerTracker.deltas, ml.deltas)

	// calling Vacuum implies flushing the datbaase content to disk..
	ml.dbs.Wdb.Vacuum(context.Background())
	// copy the database files.
	for _, ext := range []string{"", "-shm", "-wal"} {
		bytes, err := ioutil.ReadFile(ml.filename + ext)
		require.NoError(t, err)
		err = ioutil.WriteFile(newLedgerTracker.filename+ext, bytes, 0600)
		require.NoError(t, err)
	}
	dbs, err := db.OpenPair(newLedgerTracker.filename, false)
	require.NoError(t, err)
	dbs.Rdb.SetLogger(dblogger)
	dbs.Wdb.SetLogger(dblogger)

	newLedgerTracker.dbs = dbs
	return newLedgerTracker
}

func (ml *mockLedgerForTracker) Close() {
	ml.trackers.close()

	ml.dbs.Close()
	// delete the database files of non-memory instances.
	if !ml.inMemory {
		os.Remove(ml.filename)
		os.Remove(ml.filename + "-shm")
		os.Remove(ml.filename + "-wal")
	}
}

func (ml *mockLedgerForTracker) Latest() basics.Round {
	return basics.Round(len(ml.blocks)) - 1
}

func (ml *mockLedgerForTracker) addMockBlock(be blockEntry, delta ledgercore.StateDelta) error {
	ml.blocks = append(ml.blocks, be)
	ml.deltas = append(ml.deltas, delta)
	return nil
}

func (ml *mockLedgerForTracker) trackerEvalVerified(blk bookkeeping.Block, accUpdatesLedger internal.LedgerForEvaluator) (ledgercore.StateDelta, error) {
	// support returning the deltas if the client explicitly provided them by calling addMockBlock, otherwise,
	// just return an empty state delta ( since the client clearly didn't care about these )
	if len(ml.deltas) > int(blk.Round()) {
		return ml.deltas[uint64(blk.Round())], nil
	}
	return ledgercore.StateDelta{
		Hdr: &bookkeeping.BlockHeader{},
	}, nil
}

func (ml *mockLedgerForTracker) Block(rnd basics.Round) (bookkeeping.Block, error) {
	if rnd > ml.Latest() {
		return bookkeeping.Block{}, fmt.Errorf("rnd %d out of bounds", rnd)
	}

	return ml.blocks[int(rnd)].block, nil
}

func (ml *mockLedgerForTracker) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	if rnd > ml.Latest() {
		return bookkeeping.BlockHeader{}, fmt.Errorf("rnd %d out of bounds", rnd)
	}

	return ml.blocks[int(rnd)].block.BlockHeader, nil
}

func (ml *mockLedgerForTracker) trackerDB() db.Pair {
	return ml.dbs
}

func (ml *mockLedgerForTracker) blockDB() db.Pair {
	return db.Pair{}
}

func (ml *mockLedgerForTracker) trackerLog() logging.Logger {
	return ml.log
}

func (ml *mockLedgerForTracker) GenesisHash() crypto.Digest {
	if len(ml.blocks) > 0 {
		return ml.blocks[0].block.GenesisHash()
	}
	return crypto.Digest{}
}

func (ml *mockLedgerForTracker) GenesisProto() config.ConsensusParams {
	return ml.consensusParams
}

func (ml *mockLedgerForTracker) GenesisAccounts() map[basics.Address]basics.AccountData {
	return ml.accts
}

// this function used to be in acctupdates.go, but we were never using it for production purposes. This
// function has a conceptual flaw in that it attempts to load the entire balances into memory. This might
// not work if we have large number of balances. On these unit testing, however, it's not the case, and it's
// safe to call it.
func (au *accountUpdates) allBalances(rnd basics.Round) (bals map[basics.Address]basics.AccountData, err error) {
	au.accountsMu.RLock()
	defer au.accountsMu.RUnlock()
	offsetLimit, err := au.roundOffset(rnd)

	if err != nil {
		return
	}

	err = au.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		var err0 error
		bals, err0 = accountsAll(tx)
		return err0
	})
	if err != nil {
		return
	}

	for offset := uint64(0); offset < offsetLimit; offset++ {
		for i := 0; i < au.deltas[offset].Len(); i++ {
			addr, delta := au.deltas[offset].GetByIdx(i)
			bals[addr] = delta
		}
	}
	return
}

func newAcctUpdates(tb testing.TB, l *mockLedgerForTracker, conf config.Local, dbPathPrefix string) *accountUpdates {
	au := &accountUpdates{}
	au.initialize(conf)
	_, err := trackerDBInitialize(l, false, ".")
	require.NoError(tb, err)

	l.trackers.initialize(l, []ledgerTracker{au}, conf)
	err = l.trackers.loadFromDisk(l)
	require.NoError(tb, err)

	return au
}

func checkAcctUpdates(t *testing.T, au *accountUpdates, base basics.Round, latestRnd basics.Round, accts []map[basics.Address]basics.AccountData, rewards []uint64, proto config.ConsensusParams) {
	latest := au.latest()
	require.Equal(t, latestRnd, latest)

	_, err := au.Totals(latest + 1)
	require.Error(t, err)

	var validThrough basics.Round
	_, validThrough, err = au.LookupWithoutRewards(latest+1, ledgertesting.RandomAddress())
	require.Error(t, err)
	require.Equal(t, basics.Round(0), validThrough)

	if base > 0 {
		_, err := au.Totals(base - 1)
		require.Error(t, err)

		_, validThrough, err = au.LookupWithoutRewards(base-1, ledgertesting.RandomAddress())
		require.Error(t, err)
		require.Equal(t, basics.Round(0), validThrough)
	}

	roundsRanges := []struct {
		start, end basics.Round
	}{}

	// running the checkAcctUpdates on the entire range of base..latestRnd is too slow, and unlikely to help us
	// to trap a regression ( might be a good to find where the regression started ). so, for
	// performance reasons, we're going to run it againt the first and last 5 rounds, plus few rounds
	// in between.
	if latestRnd-base <= 10 {
		roundsRanges = append(roundsRanges, struct{ start, end basics.Round }{base, latestRnd})
	} else {
		roundsRanges = append(roundsRanges, struct{ start, end basics.Round }{base, base + 5})
		roundsRanges = append(roundsRanges, struct{ start, end basics.Round }{latestRnd - 5, latestRnd})
		for i := base + 5; i < latestRnd-5; i += 1 + (latestRnd-base-10)/10 {
			roundsRanges = append(roundsRanges, struct{ start, end basics.Round }{i, i + 1})
		}
	}
	for _, roundRange := range roundsRanges {
		for rnd := roundRange.start; rnd <= roundRange.end; rnd++ {
			var totalOnline, totalOffline, totalNotPart uint64

			for addr, data := range accts[rnd] {
				d, validThrough, err := au.LookupWithoutRewards(rnd, addr)
				require.NoError(t, err)
				require.Equal(t, d, data)
				require.GreaterOrEqualf(t, uint64(validThrough), uint64(rnd), fmt.Sprintf("validThrough :%v\nrnd :%v\n", validThrough, rnd))

				rewardsDelta := rewards[rnd] - d.RewardsBase
				switch d.Status {
				case basics.Online:
					totalOnline += d.MicroNovas.Raw
					totalOnline += (d.MicroNovas.Raw / proto.RewardUnit) * rewardsDelta
				case basics.Offline:
					totalOffline += d.MicroNovas.Raw
					totalOffline += (d.MicroNovas.Raw / proto.RewardUnit) * rewardsDelta
				case basics.NotParticipating:
					totalNotPart += d.MicroNovas.Raw
				default:
					t.Errorf("unknown status %v", d.Status)
				}
			}

			all, err := au.allBalances(rnd)
			require.NoError(t, err)
			require.Equal(t, all, accts[rnd])

			totals, err := au.Totals(rnd)
			require.NoError(t, err)
			require.Equal(t, totals.Online.Money.Raw, totalOnline)
			require.Equal(t, totals.Offline.Money.Raw, totalOffline)
			require.Equal(t, totals.NotParticipating.Money.Raw, totalNotPart)
			require.Equal(t, totals.Participating().Raw, totalOnline+totalOffline)
			require.Equal(t, totals.All().Raw, totalOnline+totalOffline+totalNotPart)

			d, validThrough, err := au.LookupWithoutRewards(rnd, ledgertesting.RandomAddress())
			require.NoError(t, err)
			require.GreaterOrEqualf(t, uint64(validThrough), uint64(rnd), fmt.Sprintf("validThrough :%v\nrnd :%v\n", validThrough, rnd))
			require.Equal(t, d, basics.AccountData{})
		}
	}
	checkAcctUpdatesConsistency(t, au)
}

func checkAcctUpdatesConsistency(t *testing.T, au *accountUpdates) {
	accounts := make(map[basics.Address]modifiedAccount)

	for _, rdelta := range au.deltas {
		for i := 0; i < rdelta.Len(); i++ {
			addr, adelta := rdelta.GetByIdx(i)
			macct := accounts[addr]
			macct.data = adelta
			macct.ndeltas++
			accounts[addr] = macct
		}
	}

	require.Equal(t, au.accounts, accounts)
}

func TestAcctUpdates(t *testing.T) {
	partitiontest.PartitionTest(t)

	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		t.Skip("This test is too slow on ARM and causes travis builds to time out")
	}
	proto := config.Consensus[protocol.ConsensusCurrentVersion]

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(20, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, 10, protocol.ConsensusCurrentVersion, accts)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(t, ml, conf, ".")
	defer au.close()

	// cover 10 genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < 10; i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	checkAcctUpdates(t, au, 0, 9, accts, rewardsLevels, proto)

	// lastCreatableID stores asset or app max used index to get rid of conflicts
	lastCreatableID := crypto.RandUint64() % 512
	knownCreatables := make(map[basics.CreatableIndex]bool)

	for i := basics.Round(10); i < basics.Round(proto.MaxBalLookback+15); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		var updates ledgercore.AccountDeltas
		var totals map[basics.Address]basics.AccountData
		base := accts[i-1]
		updates, totals, lastCreatableID = ledgertesting.RandomDeltasBalancedFull(1, base, rewardLevel, lastCreatableID)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = protocol.ConsensusCurrentVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Creatables = creatablesFromUpdates(base, updates, knownCreatables)
		delta.Totals = curTotals
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)

		checkAcctUpdates(t, au, 0, i, accts, rewardsLevels, proto)
	}

	for i := basics.Round(0); i < 15; i++ {
		// Clear the timer to ensure a flush
		ml.trackers.lastFlushTime = time.Time{}

		ml.trackers.committedUpTo(basics.Round(proto.MaxBalLookback) + i)
		ml.trackers.waitAccountsWriting()
		checkAcctUpdates(t, au, i, basics.Round(proto.MaxBalLookback+14), accts, rewardsLevels, proto)
	}

	// check the account totals.
	var dbRound basics.Round
	err := ml.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		dbRound, err = accountsRound(tx)
		return
	})
	require.NoError(t, err)

	var updates ledgercore.AccountDeltas
	for addr, acctData := range accts[dbRound] {
		updates.Upsert(addr, acctData)
	}

	expectedTotals := ledgertesting.CalculateNewRoundAccountTotals(t, updates, rewardsLevels[dbRound], proto, nil, ledgercore.AccountTotals{})
	var actualTotals ledgercore.AccountTotals
	err = ml.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		actualTotals, err = accountsTotals(tx, false)
		return
	})
	require.NoError(t, err)
	require.Equal(t, expectedTotals, actualTotals)
}
func TestAcctUpdatesFastUpdates(t *testing.T) {
	partitiontest.PartitionTest(t)

	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		t.Skip("This test is too slow on ARM and causes travis builds to time out")
	}
	proto := config.Consensus[protocol.ConsensusCurrentVersion]

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(20, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, 10, protocol.ConsensusCurrentVersion, accts)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	au := newAcctUpdates(t, ml, conf, ".")
	defer au.close()

	// cover 10 genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < 10; i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	checkAcctUpdates(t, au, 0, 9, accts, rewardsLevels, proto)

	wg := sync.WaitGroup{}

	for i := basics.Round(10); i < basics.Round(proto.MaxBalLookback+15); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		updates, totals := ledgertesting.RandomDeltasBalanced(1, accts[i-1], rewardLevel)

		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = protocol.ConsensusCurrentVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)

		wg.Add(1)
		go func(round basics.Round) {
			defer wg.Done()
			ml.trackers.committedUpTo(round)
		}(i)
	}
	wg.Wait()
}

func BenchmarkBalancesChanges(b *testing.B) {
	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		b.Skip("This test is too slow on ARM and causes travis builds to time out")
	}
	if b.N < 100 {
		b.N = 50
	}
	protocolVersion := protocol.ConsensusVersion("BenchmarkBalancesChanges-test-protocol-version")
	testProtocol := config.Consensus[protocol.ConsensusCurrentVersion]
	testProtocol.MaxBalLookback = 25
	config.Consensus[protocolVersion] = testProtocol
	defer func() {
		delete(config.Consensus, protocolVersion)
	}()

	proto := config.Consensus[protocolVersion]

	initialRounds := uint64(1)

	accountsCount := 5000
	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(accountsCount, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(b, true, int(initialRounds), protocolVersion, accts)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(b, ml, conf, ".")
	defer au.close()

	// cover initialRounds genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < int(initialRounds); i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	for i := basics.Round(initialRounds); i < basics.Round(proto.MaxBalLookback+uint64(b.N)); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 0
		if i <= basics.Round(initialRounds)+basics.Round(b.N) {
			accountChanges = accountsCount - 2 - int(basics.Round(proto.MaxBalLookback+uint64(b.N))+i)
		}

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(b, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(b, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(b, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = protocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}
	for i := proto.MaxBalLookback; i < proto.MaxBalLookback+initialRounds; i++ {
		// Clear the timer to ensure a flush
		ml.trackers.lastFlushTime = time.Time{}
		ml.trackers.committedUpTo(basics.Round(i))
	}
	ml.trackers.waitAccountsWriting()
	b.ResetTimer()
	startTime := time.Now()
	for i := proto.MaxBalLookback + initialRounds; i < proto.MaxBalLookback+uint64(b.N); i++ {
		// Clear the timer to ensure a flush
		ml.trackers.lastFlushTime = time.Time{}
		ml.trackers.committedUpTo(basics.Round(i))
	}
	ml.trackers.waitAccountsWriting()
	deltaTime := time.Now().Sub(startTime)
	if deltaTime > time.Second {
		return
	}
	// we want to fake the N to reflect the time it took us, if we were to wait an entire second.
	singleIterationTime := deltaTime / time.Duration(uint64(b.N)-initialRounds)
	b.N = int(time.Second / singleIterationTime)
	// and now, wait for the reminder of the second.
	time.Sleep(time.Second - deltaTime)

}

func BenchmarkCalibrateNodesPerPage(b *testing.B) {
	b.Skip("This benchmark was used to tune up the NodesPerPage; it's not really useful otherwise")
	defaultNodesPerPage := merkleCommitterNodesPerPage
	for nodesPerPage := 32; nodesPerPage < 300; nodesPerPage++ {
		b.Run(fmt.Sprintf("Test_merkleCommitterNodesPerPage_%d", nodesPerPage), func(b *testing.B) {
			merkleCommitterNodesPerPage = int64(nodesPerPage)
			BenchmarkBalancesChanges(b)
		})
	}
	merkleCommitterNodesPerPage = defaultNodesPerPage
}

func BenchmarkCalibrateCacheNodeSize(b *testing.B) {
	//b.Skip("This benchmark was used to tune up the trieCachedNodesCount; it's not really useful otherwise")
	defaultTrieCachedNodesCount := trieCachedNodesCount
	for cacheSize := 3000; cacheSize < 50000; cacheSize += 1000 {
		b.Run(fmt.Sprintf("Test_cacheSize_%d", cacheSize), func(b *testing.B) {
			trieCachedNodesCount = cacheSize
			BenchmarkBalancesChanges(b)
		})
	}
	trieCachedNodesCount = defaultTrieCachedNodesCount
}

// TestLargeAccountCountCatchpointGeneration creates a ledger containing a large set of accounts ( i.e. 100K accounts )
// and attempts to have the accountUpdates create the associated catchpoint. It's designed precisely around setting an
// environment which would quickly ( i.e. after 32 rounds ) would start producing catchpoints.
func TestLargeAccountCountCatchpointGeneration(t *testing.T) {
	partitiontest.PartitionTest(t)

	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		t.Skip("This test is too slow on ARM and causes travis builds to time out")
	}

	// The next operations are heavy on the memory.
	// Garbage collection helps prevent trashing
	runtime.GC()

	// create new protocol version, which has lower lookback
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestLargeAccountCountCatchpointGeneration")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.MaxBalLookback = 32
	protoParams.SeedLookback = 2
	protoParams.SeedRefreshInterval = 8
	config.Consensus[testProtocolVersion] = protoParams
	defer func() {
		delete(config.Consensus, testProtocolVersion)
		os.RemoveAll("./catchpoints")
	}()

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(100000, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, accts)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	conf.CatchpointInterval = 1
	conf.Archival = true
	au := newAcctUpdates(t, ml, conf, ".")
	defer au.close()

	// cover 10 genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < 10; i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	for i := basics.Round(10); i < basics.Round(protoParams.MaxBalLookback+5); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		updates, totals := ledgertesting.RandomDeltasBalanced(1, accts[i-1], rewardLevel)

		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = testProtocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)

		ml.trackers.committedUpTo(i)
		if i%2 == 1 {
			ml.trackers.waitAccountsWriting()
		}
	}

	// Garbage collection helps prevent trashing for next tests
	runtime.GC()
}

// The TestAcctUpdatesUpdatesCorrectness conduct a correctless test for the accounts update in the following way -
// Each account is initialized with 100 algos.
// On every round, each account move variable amount of funds to an accumulating account.
// The deltas for each accounts are picked by using the lookup method.
// At the end of the test, we verify that each account has the expected amount of algos.
// In addition, throughout the test, we check ( using lookup ) that the historical balances, *beyond* the
// lookback are generating either an error, or returning the correct amount.
func TestAcctUpdatesUpdatesCorrectness(t *testing.T) {
	partitiontest.PartitionTest(t)

	if runtime.GOARCH == "arm" || runtime.GOARCH == "arm64" {
		t.Skip("This test is too slow on ARM and causes travis builds to time out")
	}

	// create new protocol version, which has lower look back.
	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestAcctUpdatesUpdatesCorrectness")
	protoParams := config.Consensus[protocol.ConsensusCurrentVersion]
	protoParams.MaxBalLookback = 5
	config.Consensus[testProtocolVersion] = protoParams
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	inMemory := true

	testFunction := func(t *testing.T) {
		accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(9, true)}

		pooldata := basics.AccountData{}
		pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
		pooldata.Status = basics.NotParticipating
		accts[0][testPoolAddr] = pooldata

		sinkdata := basics.AccountData{}
		sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
		sinkdata.Status = basics.NotParticipating
		accts[0][testSinkAddr] = sinkdata

		ml := makeMockLedgerForTracker(t, inMemory, 10, testProtocolVersion, accts)
		defer ml.Close()

		var moneyAccounts []basics.Address

		for addr := range accts[0] {
			if bytes.Compare(addr[:], testPoolAddr[:]) == 0 || bytes.Compare(addr[:], testSinkAddr[:]) == 0 {
				continue
			}
			moneyAccounts = append(moneyAccounts, addr)
		}

		moneyAccountsExpectedAmounts := make([][]uint64, 0)
		// set all the accounts with 100 algos.
		for _, addr := range moneyAccounts {
			accountData := accts[0][addr]
			accountData.MicroNovas.Raw = 100 * 1000000
			accts[0][addr] = accountData
		}

		conf := config.GetDefaultLocal()
		au := newAcctUpdates(t, ml, conf, ".")
		defer au.close()

		// cover 10 genesis blocks
		rewardLevel := uint64(0)
		for i := 1; i < 10; i++ {
			accts = append(accts, accts[0])

		}
		for i := 0; i < 10; i++ {
			moneyAccountsExpectedAmounts = append(moneyAccountsExpectedAmounts, make([]uint64, len(moneyAccounts)))
			for j := range moneyAccounts {
				moneyAccountsExpectedAmounts[i][j] = 100 * 1000000
			}
		}

		i := basics.Round(10)
		roundCount := 50
		for ; i < basics.Round(10+roundCount); i++ {
			updates := make(map[basics.Address]basics.AccountData)
			moneyAccountsExpectedAmounts = append(moneyAccountsExpectedAmounts, make([]uint64, len(moneyAccounts)))
			toAccount := moneyAccounts[0]
			toAccountDataOld, validThrough, err := au.LookupWithoutRewards(i-1, toAccount)
			require.NoError(t, err)
			require.Equal(t, i-1, validThrough)
			toAccountDataNew := toAccountDataOld

			for j := 1; j < len(moneyAccounts); j++ {
				fromAccount := moneyAccounts[j]

				fromAccountDataOld, validThrough, err := au.LookupWithoutRewards(i-1, fromAccount)
				require.NoError(t, err)
				require.Equal(t, i-1, validThrough)
				require.Equalf(t, moneyAccountsExpectedAmounts[i-1][j], fromAccountDataOld.MicroNovas.Raw, "Account index : %d\nRound number : %d", j, i)

				fromAccountDataNew := fromAccountDataOld

				fromAccountDataNew.MicroNovas.Raw -= uint64(i - 10)
				toAccountDataNew.MicroNovas.Raw += uint64(i - 10)
				updates[fromAccount] = fromAccountDataNew

				moneyAccountsExpectedAmounts[i][j] = fromAccountDataNew.MicroNovas.Raw
			}

			moneyAccountsExpectedAmounts[i][0] = moneyAccountsExpectedAmounts[i-1][0] + uint64(len(moneyAccounts)-1)*uint64(i-10)

			// force to perform a test that goes directly to disk, and see if it has the expected values.
			if uint64(i) > protoParams.MaxBalLookback+3 {

				// check the status at a historical time:
				checkRound := uint64(i) - protoParams.MaxBalLookback - 2

				testback := 1
				for j := 1; j < len(moneyAccounts); j++ {
					if checkRound < uint64(testback) {
						continue
					}
					acct, validThrough, err := au.LookupWithoutRewards(basics.Round(checkRound-uint64(testback)), moneyAccounts[j])
					// we might get an error like "round 2 before dbRound 5", which is the success case, so we'll ignore it.
					roundOffsetError := &RoundOffsetError{}
					if errors.As(err, &roundOffsetError) {
						require.Equal(t, basics.Round(0), validThrough)
						// verify it's the expected error and not anything else.
						require.Less(t, int64(roundOffsetError.round), int64(roundOffsetError.dbRound))
						if testback > 1 {
							testback--
						}
						continue
					}
					require.NoError(t, err)
					require.GreaterOrEqual(t, int64(validThrough), int64(basics.Round(checkRound-uint64(testback))))
					// if we received no error, we want to make sure the reported amount is correct.
					require.Equalf(t, moneyAccountsExpectedAmounts[checkRound-uint64(testback)][j], acct.MicroNovas.Raw, "Account index : %d\nRound number : %d", j, checkRound)
					testback++
					j--
				}
			}

			updates[toAccount] = toAccountDataNew

			blk := bookkeeping.Block{
				BlockHeader: bookkeeping.BlockHeader{
					Round: basics.Round(i),
				},
			}
			blk.RewardsLevel = rewardLevel
			blk.CurrentProtocol = testProtocolVersion

			delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, len(updates), 0)
			for addr, ad := range updates {
				delta.Accts.Upsert(addr, ad)
			}
			au.newBlock(blk, delta)
			ml.trackers.committedUpTo(i)
		}
		lastRound := i - 1
		ml.trackers.waitAccountsWriting()

		for idx, addr := range moneyAccounts {
			balance, validThrough, err := au.LookupWithoutRewards(lastRound, addr)
			require.NoErrorf(t, err, "unable to retrieve balance for account idx %d %v", idx, addr)
			require.Equal(t, lastRound, validThrough)
			if idx != 0 {
				require.Equalf(t, 100*1000000-roundCount*(roundCount-1)/2, int(balance.MicroNovas.Raw), "account idx %d %v has the wrong balance", idx, addr)
			} else {
				require.Equalf(t, 100*1000000+(len(moneyAccounts)-1)*roundCount*(roundCount-1)/2, int(balance.MicroNovas.Raw), "account idx %d %v has the wrong balance", idx, addr)
			}

		}
	}

	t.Run("InMemoryDB", testFunction)
	inMemory = false
	t.Run("DiskDB", testFunction)
}

// listAndCompareComb lists the assets/applications and then compares against the expected
// It repeats with different combinations of the limit parameters
func listAndCompareComb(t *testing.T, au *accountUpdates, expected map[basics.CreatableIndex]ledgercore.ModifiedCreatable) {

	// test configuration parameters

	// pick the second largest index for the app and asset
	// This is to make sure exactly one element is left out
	// as a result of max index
	maxAss1 := basics.CreatableIndex(0)
	maxAss2 := basics.CreatableIndex(0)
	maxApp1 := basics.CreatableIndex(0)
	maxApp2 := basics.CreatableIndex(0)
	for a, b := range expected {
		// A moving window of the last two largest indexes: [maxAss1, maxAss2]
		if b.Ctype == basics.AssetCreatable {
			if maxAss2 < a {
				maxAss1 = maxAss2
				maxAss2 = a
			} else if maxAss1 < a {
				maxAss1 = a
			}
		}
		if b.Ctype == basics.AppCreatable {
			if maxApp2 < a {
				maxApp1 = maxApp2
				maxApp2 = a
			} else if maxApp1 < a {
				maxApp1 = a
			}
		}
	}

	// No limits. max asset index, max app index and max results have no effect
	// This is to make sure the deleted elements do not show up
	maxAssetIdx := basics.AssetIndex(maxAss2)
	maxAppIdx := basics.AppIndex(maxApp2)
	maxResults := uint64(len(expected))
	listAndCompare(t, maxAssetIdx, maxAppIdx, maxResults, au, expected)

	// Limit with max asset index and max app index (max results has no effect)
	maxAssetIdx = basics.AssetIndex(maxAss1)
	maxAppIdx = basics.AppIndex(maxApp1)
	maxResults = uint64(len(expected))
	listAndCompare(t, maxAssetIdx, maxAppIdx, maxResults, au, expected)

	// Limit with max results
	maxResults = 1
	listAndCompare(t, maxAssetIdx, maxAppIdx, maxResults, au, expected)
}

// listAndCompareComb lists the assets/applications and then compares against the expected
// It uses the provided limit parameters
func listAndCompare(t *testing.T,
	maxAssetIdx basics.AssetIndex,
	maxAppIdx basics.AppIndex,
	maxResults uint64,
	au *accountUpdates,
	expected map[basics.CreatableIndex]ledgercore.ModifiedCreatable) {

	// get the results with the given parameters
	assetRes, err := au.ListAssets(maxAssetIdx, maxResults)
	require.NoError(t, err)
	appRes, err := au.ListApplications(maxAppIdx, maxResults)
	require.NoError(t, err)

	// count the expected number of results
	expectedAssetCount := uint64(0)
	expectedAppCount := uint64(0)
	for a, b := range expected {
		if b.Created {
			if b.Ctype == basics.AssetCreatable &&
				a <= basics.CreatableIndex(maxAssetIdx) &&
				expectedAssetCount < maxResults {
				expectedAssetCount++
			}
			if b.Ctype == basics.AppCreatable &&
				a <= basics.CreatableIndex(maxAppIdx) &&
				expectedAppCount < maxResults {
				expectedAppCount++
			}
		}
	}

	// check the total counts are as expected
	require.Equal(t, int(expectedAssetCount), len(assetRes))
	require.Equal(t, int(expectedAppCount), len(appRes))

	// verify the results are correct
	for _, respCrtor := range assetRes {
		crtor := expected[respCrtor.Index]
		require.NotNil(t, crtor)
		require.Equal(t, basics.AssetCreatable, crtor.Ctype)
		require.Equal(t, true, crtor.Created)

		require.Equal(t, basics.AssetCreatable, respCrtor.Type)
		require.Equal(t, crtor.Creator, respCrtor.Creator)
	}
	for _, respCrtor := range appRes {
		crtor := expected[respCrtor.Index]
		require.NotNil(t, crtor)
		require.Equal(t, basics.AppCreatable, crtor.Ctype)
		require.Equal(t, true, crtor.Created)

		require.Equal(t, basics.AppCreatable, respCrtor.Type)
		require.Equal(t, crtor.Creator, respCrtor.Creator)
	}
}

// TestListCreatables tests ListAssets and ListApplications
// It tests with all elements in cache, all synced to database, and combination of both
// It also tests the max results, max app index and max asset index
func TestListCreatables(t *testing.T) {
	partitiontest.PartitionTest(t)

	// test configuration parameters
	numElementsPerSegement := 25

	// set up the database
	dbs, _ := dbOpenTest(t, true)
	setDbLogging(t, dbs)
	defer dbs.Close()

	tx, err := dbs.Wdb.Handle.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	proto := config.Consensus[protocol.ConsensusCurrentVersion]

	accts := make(map[basics.Address]basics.AccountData)
	_, err = accountsInit(tx, accts, proto)
	require.NoError(t, err)

	err = accountsAddNormalizedBalance(tx, proto)
	require.NoError(t, err)

	au := &accountUpdates{}
	au.accountsq, err = accountsInitDbQueries(tx, tx)
	require.NoError(t, err)

	// ******* All results are obtained from the cache. Empty database *******
	// ******* No deletes                                              *******
	// get random data. Initial batch, no deletes
	ctbsList, randomCtbs := randomCreatables(numElementsPerSegement)
	expectedDbImage := make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable)
	ctbsWithDeletes := randomCreatableSampling(1, ctbsList, randomCtbs,
		expectedDbImage, numElementsPerSegement)
	// set the cache
	au.creatables = ctbsWithDeletes
	listAndCompareComb(t, au, expectedDbImage)

	// ******* All results are obtained from the database. Empty cache *******
	// ******* No deletes	                                           *******
	// sync with the database
	var updates compactAccountDeltas
	_, err = accountsNewRound(tx, updates, ctbsWithDeletes, proto, basics.Round(1))
	require.NoError(t, err)
	// nothing left in cache
	au.creatables = make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable)
	listAndCompareComb(t, au, expectedDbImage)

	// ******* Results are obtained from the database and from the cache *******
	// ******* No deletes in the database.                               *******
	// ******* Data in the database deleted in the cache                 *******
	au.creatables = randomCreatableSampling(2, ctbsList, randomCtbs,
		expectedDbImage, numElementsPerSegement)
	listAndCompareComb(t, au, expectedDbImage)

	// ******* Results are obtained from the database and from the cache *******
	// ******* Deletes are in the database and in the cache              *******
	// sync with the database. This has deletes synced to the database.
	_, err = accountsNewRound(tx, updates, au.creatables, proto, basics.Round(1))
	require.NoError(t, err)
	// get new creatables in the cache. There will be deletes in the cache from the previous batch.
	au.creatables = randomCreatableSampling(3, ctbsList, randomCtbs,
		expectedDbImage, numElementsPerSegement)
	listAndCompareComb(t, au, expectedDbImage)
}

func accountsAll(tx *sql.Tx) (bals map[basics.Address]basics.AccountData, err error) {
	rows, err := tx.Query("SELECT address, data FROM accountbase")
	if err != nil {
		return
	}
	defer rows.Close()

	bals = make(map[basics.Address]basics.AccountData)
	for rows.Next() {
		var addrbuf []byte
		var buf []byte
		err = rows.Scan(&addrbuf, &buf)
		if err != nil {
			return
		}

		var data basics.AccountData
		err = protocol.Decode(buf, &data)
		if err != nil {
			return
		}

		var addr basics.Address
		if len(addrbuf) != len(addr) {
			err = fmt.Errorf("Account DB address length mismatch: %d != %d", len(addrbuf), len(addr))
			return
		}

		copy(addr[:], addrbuf)
		bals[addr] = data
	}

	err = rows.Err()
	return
}

func BenchmarkLargeMerkleTrieRebuild(b *testing.B) {
	proto := config.Consensus[protocol.ConsensusCurrentVersion]

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(5, true)}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(b, true, 10, protocol.ConsensusCurrentVersion, accts)
	defer ml.Close()

	cfg := config.GetDefaultLocal()
	cfg.Archival = true
	au := newAcctUpdates(b, ml, cfg, ".")
	defer au.close()

	// at this point, the database was created. We want to fill the accounts data
	accountsNumber := 6000000 * b.N
	for i := 0; i < accountsNumber-5-2; { // subtract the account we've already created above, plus the sink/reward
		var updates compactAccountDeltas
		for k := 0; i < accountsNumber-5-2 && k < 1024; k++ {
			addr := ledgertesting.RandomAddress()
			acctData := basics.AccountData{}
			acctData.MicroNovas.Raw = 1
			updates.upsert(addr, accountDelta{new: acctData})
			i++
		}

		err := ml.dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
			_, err = accountsNewRound(tx, updates, nil, proto, basics.Round(1))
			return
		})
		require.NoError(b, err)
	}

	err := ml.dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		return updateAccountsHashRound(tx, 1)
	})
	require.NoError(b, err)

	au.close()

	b.ResetTimer()
	err = au.loadFromDisk(ml, 0)
	require.NoError(b, err)
	b.StopTimer()
	b.ReportMetric(float64(accountsNumber), "entries/trie")
}

func BenchmarkCompactDeltas(b *testing.B) {
	b.Run("account-deltas", func(b *testing.B) {
		if b.N < 500 {
			b.N = 500
		}
		window := 5000
		accountDeltas := make([]ledgercore.AccountDeltas, b.N)
		addrs := make([]basics.Address, b.N*window)
		for i := 0; i < len(addrs); i++ {
			addrs[i] = basics.Address(crypto.Hash([]byte{byte(i % 256), byte((i / 256) % 256), byte(i / 65536)}))
		}
		for rnd := 0; rnd < b.N; rnd++ {
			m := make(map[basics.Address]basics.AccountData)
			start := 0
			if rnd > 0 {
				start = window/2 + (rnd-1)*window
			}
			for k := start; k < start+window; k++ {
				accountDeltas[rnd].Upsert(addrs[k], basics.AccountData{})
				m[addrs[k]] = basics.AccountData{}
			}
		}
		var baseAccounts lruAccounts
		baseAccounts.init(nil, 100, 80)
		b.ResetTimer()

		makeCompactAccountDeltas(accountDeltas, baseAccounts)

	})
}
func TestCompactDeltas(t *testing.T) {
	partitiontest.PartitionTest(t)

	addrs := make([]basics.Address, 10)
	for i := 0; i < len(addrs); i++ {
		addrs[i] = basics.Address(crypto.Hash([]byte{byte(i % 256), byte((i / 256) % 256), byte(i / 65536)}))
	}

	accountDeltas := make([]ledgercore.AccountDeltas, 1, 1)
	creatableDeltas := make([]map[basics.CreatableIndex]ledgercore.ModifiedCreatable, 1, 1)
	creatableDeltas[0] = make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable)
	accountDeltas[0].Upsert(addrs[0], basics.AccountData{MicroNovas: basics.MicroNovas{Raw: 2}})
	creatableDeltas[0][100] = ledgercore.ModifiedCreatable{Creator: addrs[2], Created: true}
	var baseAccounts lruAccounts
	baseAccounts.init(nil, 100, 80)
	outAccountDeltas := makeCompactAccountDeltas(accountDeltas, baseAccounts)
	outCreatableDeltas := compactCreatableDeltas(creatableDeltas)

	require.Equal(t, accountDeltas[0].Len(), outAccountDeltas.len())
	require.Equal(t, len(creatableDeltas[0]), len(outCreatableDeltas))
	require.Equal(t, accountDeltas[0].Len(), len(outAccountDeltas.misses))

	delta, _ := outAccountDeltas.get(addrs[0])
	require.Equal(t, persistedAccountData{}, delta.old)
	require.Equal(t, basics.AccountData{MicroNovas: basics.MicroNovas{Raw: 2}}, delta.new)
	require.Equal(t, ledgercore.ModifiedCreatable{Creator: addrs[2], Created: true, Ndeltas: 1}, outCreatableDeltas[100])

	// add another round
	accountDeltas = append(accountDeltas, ledgercore.AccountDeltas{})
	creatableDeltas = append(creatableDeltas, make(map[basics.CreatableIndex]ledgercore.ModifiedCreatable))
	accountDeltas[1].Upsert(addrs[0], basics.AccountData{MicroNovas: basics.MicroNovas{Raw: 3}})
	accountDeltas[1].Upsert(addrs[3], basics.AccountData{MicroNovas: basics.MicroNovas{Raw: 8}})

	creatableDeltas[1][100] = ledgercore.ModifiedCreatable{Creator: addrs[2], Created: false}
	creatableDeltas[1][101] = ledgercore.ModifiedCreatable{Creator: addrs[4], Created: true}

	baseAccounts.write(persistedAccountData{addr: addrs[0], accountData: basics.AccountData{MicroNovas: basics.MicroNovas{Raw: 1}}})
	outAccountDeltas = makeCompactAccountDeltas(accountDeltas, baseAccounts)
	outCreatableDeltas = compactCreatableDeltas(creatableDeltas)

	require.Equal(t, 2, outAccountDeltas.len())
	require.Equal(t, 2, len(outCreatableDeltas))

	delta, _ = outAccountDeltas.get(addrs[0])
	require.Equal(t, uint64(1), delta.old.accountData.MicroNovas.Raw)
	require.Equal(t, uint64(3), delta.new.MicroNovas.Raw)
	require.Equal(t, int(2), delta.ndeltas)
	delta, _ = outAccountDeltas.get(addrs[3])
	require.Equal(t, uint64(0), delta.old.accountData.MicroNovas.Raw)
	require.Equal(t, uint64(8), delta.new.MicroNovas.Raw)
	require.Equal(t, int(1), delta.ndeltas)

	require.Equal(t, addrs[2], outCreatableDeltas[100].Creator)
	require.Equal(t, addrs[4], outCreatableDeltas[101].Creator)
	require.Equal(t, false, outCreatableDeltas[100].Created)
	require.Equal(t, true, outCreatableDeltas[101].Created)
	require.Equal(t, 2, outCreatableDeltas[100].Ndeltas)
	require.Equal(t, 1, outCreatableDeltas[101].Ndeltas)

}

// TestAcctUpdatesCachesInitialization test the functionality of the initializeCaches cache.
func TestAcctUpdatesCachesInitialization(t *testing.T) {
	partitiontest.PartitionTest(t)

	// The next operations are heavy on the memory.
	// Garbage collection helps prevent trashing
	runtime.GC()

	protocolVersion := protocol.ConsensusCurrentVersion
	proto := config.Consensus[protocolVersion]

	initialRounds := uint64(1)
	accountsCount := 5

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(accountsCount, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, int(initialRounds), protocolVersion, accts)
	ml.log.SetLevel(logging.Warn)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(t, ml, conf, ".")

	// cover initialRounds genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < int(initialRounds); i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	recoveredLedgerRound := basics.Round(initialRounds + initializeCachesRoundFlushInterval + proto.MaxBalLookback + 1)

	for i := basics.Round(initialRounds); i <= recoveredLedgerRound; i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = protocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		ml.trackers.committedUpTo(basics.Round(i))
		ml.trackers.waitAccountsWriting()
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}
	au.close()

	// reset the accounts, since their balances are now changed due to the rewards.
	accts = []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(accountsCount, true)}

	// create another mocked ledger, but this time with a fresh new tracker database.
	ml2 := makeMockLedgerForTracker(t, true, int(initialRounds), protocolVersion, accts)
	ml2.log.SetLevel(logging.Warn)
	defer ml2.Close()

	// and "fix" it to contain the blocks and deltas from before.
	ml2.blocks = ml.blocks
	ml2.deltas = ml.deltas

	conf = config.GetDefaultLocal()
	au = newAcctUpdates(t, ml2, conf, ".")
	defer au.close()

	// make sure the deltas array end up containing only the most recent 320 rounds.
	require.Equal(t, int(proto.MaxBalLookback), len(au.deltas))
	require.Equal(t, recoveredLedgerRound-basics.Round(proto.MaxBalLookback), au.cachedDBRound)

	// Garbage collection helps prevent trashing for next tests
	runtime.GC()
}

// TestAcctUpdatesSplittingConsensusVersionCommits tests the a sequence of commits that spans over multiple consensus versions works correctly.
func TestAcctUpdatesSplittingConsensusVersionCommits(t *testing.T) {
	partitiontest.PartitionTest(t)

	initProtocolVersion := protocol.ConsensusV20
	initialProtoParams := config.Consensus[initProtocolVersion]

	initialRounds := uint64(1)

	accountsCount := 5
	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(accountsCount, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, int(initialRounds), initProtocolVersion, accts)
	ml.log.SetLevel(logging.Warn)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(t, ml, conf, ".")

	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	defer au.close()

	// cover initialRounds genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < int(initialRounds); i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	extraRounds := uint64(39)

	// write the extraRounds rounds so that we will fill up the queue.
	for i := basics.Round(initialRounds); i < basics.Round(initialRounds+extraRounds); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = initProtocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	newVersionBlocksCount := uint64(47)
	newVersion := protocol.ConsensusV21
	// add 47 more rounds that contains blocks using a newer consensus version, and stuff it with MaxBalLookback
	lastRoundToWrite := basics.Round(initialRounds + initialProtoParams.MaxBalLookback + extraRounds + newVersionBlocksCount)
	for i := basics.Round(initialRounds + extraRounds); i < lastRoundToWrite; i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = newVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}
	// now, commit and verify that the produceCommittingTask method broken the range correctly.
	ml.trackers.committedUpTo(lastRoundToWrite)
	ml.trackers.waitAccountsWriting()
	require.Equal(t, basics.Round(initialRounds+extraRounds)-1, au.cachedDBRound)

}

// TestAcctUpdatesSplittingConsensusVersionCommitsBoundry tests the a sequence of commits that spans over multiple consensus versions works correctly, and
// in particular, complements TestAcctUpdatesSplittingConsensusVersionCommits by testing the commit boundary.
func TestAcctUpdatesSplittingConsensusVersionCommitsBoundry(t *testing.T) {
	partitiontest.PartitionTest(t)

	initProtocolVersion := protocol.ConsensusV20
	initialProtoParams := config.Consensus[initProtocolVersion]

	initialRounds := uint64(1)

	accountsCount := 5
	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(accountsCount, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, int(initialRounds), initProtocolVersion, accts)
	ml.log.SetLevel(logging.Warn)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(t, ml, conf, ".")

	err := au.loadFromDisk(ml, 0)
	require.NoError(t, err)
	defer au.close()

	// cover initialRounds genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < int(initialRounds); i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	extraRounds := uint64(39)

	// write extraRounds rounds so that we will fill up the queue.
	for i := basics.Round(initialRounds); i < basics.Round(initialRounds+extraRounds); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = initProtocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	newVersion := protocol.ConsensusV21
	// add MaxBalLookback-extraRounds more rounds that contains blocks using a newer consensus version.
	endOfFirstNewProtocolSegment := basics.Round(initialRounds + extraRounds + initialProtoParams.MaxBalLookback)
	for i := basics.Round(initialRounds + extraRounds); i <= endOfFirstNewProtocolSegment; i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = newVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}
	// now, commit and verify that the produceCommittingTask method broken the range correctly.
	ml.trackers.committedUpTo(endOfFirstNewProtocolSegment)
	ml.trackers.waitAccountsWriting()
	require.Equal(t, basics.Round(initialRounds+extraRounds)-1, au.cachedDBRound)

	// write additional extraRounds elements and verify these can be flushed.
	for i := endOfFirstNewProtocolSegment + 1; i <= basics.Round(initialRounds+2*extraRounds+initialProtoParams.MaxBalLookback); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		accountChanges := 2

		updates, totals := ledgertesting.RandomDeltasBalanced(accountChanges, accts[i-1], rewardLevel)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool
		curTotals := accumulateTotals(t, protocol.ConsensusCurrentVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		require.Equal(t, prevTotals.All(), curTotals.All())

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = newVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Totals = curTotals
		ml.addMockBlock(blockEntry{block: blk}, delta)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}
	ml.trackers.committedUpTo(endOfFirstNewProtocolSegment + basics.Round(extraRounds))
	ml.trackers.waitAccountsWriting()
	require.Equal(t, basics.Round(initialRounds+2*extraRounds), au.cachedDBRound)
}

// TestConsecutiveVersion tests the consecutiveVersion method correctness.
func TestConsecutiveVersion(t *testing.T) {
	partitiontest.PartitionTest(t)

	var au accountUpdates
	au.versions = []protocol.ConsensusVersion{
		protocol.ConsensusV19,
		protocol.ConsensusV20,
		protocol.ConsensusV20,
		protocol.ConsensusV20,
		protocol.ConsensusV20,
		protocol.ConsensusV21,
		protocol.ConsensusV21,
		protocol.ConsensusV21,
		protocol.ConsensusV21,
		protocol.ConsensusV21,
		protocol.ConsensusV21,
		protocol.ConsensusV22,
	}
	for offset := uint64(1); offset < uint64(len(au.versions)); offset++ {
		co := au.consecutiveVersion(offset)
		require.Equal(t, au.versions[1], au.versions[co])
	}
	au.versions = []protocol.ConsensusVersion{
		protocol.ConsensusV19,
		protocol.ConsensusV20,
		protocol.ConsensusV21,
	}
}

// This test attempts to cover the case when an accountUpdates.lookupX method:
// - can't find the requested address,
// - falls through looking at deltas and the LRU accounts cache,
// - then hits the database (calling accountsDbQueries.lookup)
// only to discover that the round stored in the database (committed in accountUpdates.commitRound)
// is out of sync with accountUpdates.cachedDBRound (updated a little bit later in accountUpdates.postCommit).
//
// In this case it waits on a condition variable and retries when
// commitSyncer/accountUpdates has advanced the cachedDBRound.
func TestAcctUpdatesLookupRetry(t *testing.T) {
	partitiontest.PartitionTest(t)

	testProtocolVersion := protocol.ConsensusVersion("test-protocol-TestAcctUpdatesLookupRetry")
	proto := config.Consensus[protocol.ConsensusCurrentVersion]
	proto.MaxBalLookback = 10
	config.Consensus[testProtocolVersion] = proto
	defer func() {
		delete(config.Consensus, testProtocolVersion)
	}()

	accts := []map[basics.Address]basics.AccountData{ledgertesting.RandomAccounts(20, true)}
	rewardsLevels := []uint64{0}

	pooldata := basics.AccountData{}
	pooldata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	pooldata.Status = basics.NotParticipating
	accts[0][testPoolAddr] = pooldata

	sinkdata := basics.AccountData{}
	sinkdata.MicroNovas.Raw = 1000 * 1000 * 1000 * 1000
	sinkdata.Status = basics.NotParticipating
	accts[0][testSinkAddr] = sinkdata

	ml := makeMockLedgerForTracker(t, true, 10, testProtocolVersion, accts)
	defer ml.Close()

	conf := config.GetDefaultLocal()
	au := newAcctUpdates(t, ml, conf, ".")
	defer au.close()

	// cover 10 genesis blocks
	rewardLevel := uint64(0)
	for i := 1; i < 10; i++ {
		accts = append(accts, accts[0])
		rewardsLevels = append(rewardsLevels, rewardLevel)
	}

	checkAcctUpdates(t, au, 0, 9, accts, rewardsLevels, proto)

	// lastCreatableID stores asset or app max used index to get rid of conflicts
	lastCreatableID := crypto.RandUint64() % 512
	knownCreatables := make(map[basics.CreatableIndex]bool)

	for i := basics.Round(10); i < basics.Round(proto.MaxBalLookback+15); i++ {
		rewardLevelDelta := crypto.RandUint64() % 5
		rewardLevel += rewardLevelDelta
		var updates ledgercore.AccountDeltas
		var totals map[basics.Address]basics.AccountData
		base := accts[i-1]
		updates, totals, lastCreatableID = ledgertesting.RandomDeltasBalancedFull(1, base, rewardLevel, lastCreatableID)
		prevTotals, err := au.Totals(basics.Round(i - 1))
		require.NoError(t, err)

		newPool := totals[testPoolAddr]
		newPool.MicroNovas.Raw -= prevTotals.RewardUnits() * rewardLevelDelta
		updates.Upsert(testPoolAddr, newPool)
		totals[testPoolAddr] = newPool

		blk := bookkeeping.Block{
			BlockHeader: bookkeeping.BlockHeader{
				Round: basics.Round(i),
			},
		}
		blk.RewardsLevel = rewardLevel
		blk.CurrentProtocol = testProtocolVersion

		delta := ledgercore.MakeStateDelta(&blk.BlockHeader, 0, updates.Len(), 0)
		delta.Accts.MergeAccounts(updates)
		delta.Creatables = creatablesFromUpdates(base, updates, knownCreatables)
		delta.Totals = accumulateTotals(t, testProtocolVersion, []map[basics.Address]basics.AccountData{totals}, rewardLevel)
		au.newBlock(blk, delta)
		accts = append(accts, totals)
		rewardsLevels = append(rewardsLevels, rewardLevel)

		checkAcctUpdates(t, au, 0, i, accts, rewardsLevels, proto)
	}

	flushRound := func(i basics.Round) {
		// Clear the timer to ensure a flush
		ml.trackers.lastFlushTime = time.Time{}

		ml.trackers.committedUpTo(basics.Round(proto.MaxBalLookback) + i)
		ml.trackers.waitAccountsWriting()
	}

	// flush a couple of rounds (indirectly schedules commitSyncer)
	flushRound(basics.Round(0))
	flushRound(basics.Round(1))

	// add stallingTracker to list of trackers
	stallingTracker := &blockingTracker{
		postCommitUnlockedEntryLock:   make(chan struct{}),
		postCommitUnlockedReleaseLock: make(chan struct{}),
		postCommitEntryLock:           make(chan struct{}),
		postCommitReleaseLock:         make(chan struct{}),
		alwaysLock:                    true,
	}
	ml.trackers.trackers = append([]ledgerTracker{stallingTracker}, ml.trackers.trackers...)

	// kick off another round
	go flushRound(basics.Round(2))

	// let stallingTracker enter postCommit() and block (waiting on postCommitReleaseLock)
	// this will prevent accountUpdates.postCommit() from updating au.cachedDBRound = newBase
	<-stallingTracker.postCommitEntryLock

	// prune the baseAccounts cache, so that lookup will fall through to the DB
	au.accountsMu.Lock()
	au.baseAccounts.prune(0)
	au.accountsMu.Unlock()

	rnd := basics.Round(2)

	// grab any address and data to use for call to lookup
	var addr basics.Address
	var data basics.AccountData
	for a, d := range accts[rnd] {
		addr = a
		data = d
		break
	}

	defer func() { // allow the postCommitUnlocked() handler to go through, even if test fails
		<-stallingTracker.postCommitUnlockedEntryLock
		stallingTracker.postCommitUnlockedReleaseLock <- struct{}{}
	}()

	// issue a LookupWithoutRewards while persistedData.round != au.cachedDBRound
	// when synchronized=false it will fail fast
	d, validThrough, err := au.lookupWithoutRewards(rnd, addr, false)
	require.Equal(t, err, &MismatchingDatabaseRoundError{databaseRound: 2, memoryRound: 1})

	// release the postCommit lock, once au.lookupWithoutRewards() hits au.accountsReadCond.Wait()
	go func() {
		time.Sleep(200 * time.Millisecond)
		stallingTracker.postCommitReleaseLock <- struct{}{}
	}()

	// when synchronized=true it will wait until above goroutine releases postCommitReleaseLock
	d, validThrough, err = au.lookupWithoutRewards(rnd, addr, true)
	require.NoError(t, err)
	require.Equal(t, d, data)
	require.GreaterOrEqualf(t, uint64(validThrough), uint64(rnd), "validThrough: %v rnd :%v", validThrough, rnd)
}
