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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/internal"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
	"github.com/Orca18/novarand/util/db"
	"github.com/algorand/go-deadlock"
)

// ledgerTracker defines part of the API for any state machine that
// tracks the ledger's blockchain.  In addition to the API below,
// each ledgerTracker provides a tracker-specific read-only API
// (e.g., querying the balance of an account).
//
// A tracker's read-only API must be indexed by rounds, and the
// tracker must be prepared to answer queries for rounds until a
// subsequent call to committedUpTo().
//
// For example, the rewards AVL tree must be prepared to answer
// queries for old rounds, even if the tree has moved on in response
// to newBlock() calls.  It should do so by remembering the precise
// answer for old rounds, until committedUpTo() allows it to GC
// those old answers.
//
// The ledger provides a RWMutex to ensure that the tracker is invoked
// with at most one modification API call (below), OR zero modification
// calls and any number of read-only calls.  If internally the tracker
// modifies state in response to read-only calls, it is the tracker's
// responsibility to ensure thread-safety.
/*
ledgerTracker는 원장의 블록체인을 추적하는 모든 상태 머신(트래커들!!)을 위한 API의 일부를 정의합니다.
아래 API 외에도 각 ledgerTracker는 트래커별 읽기 전용 API(예: 계정 잔액 조회)를 제공합니다.
트래커의 읽기 전용 API는 라운드별로 인덱싱되어야 한다(라운드와 연결되어 있어야 한다).
트래커는 commitUpTo()에 대한 후속 호출까지 라운드에 대한 쿼리에 응답할 준비가 되어 있어야 합니다.(??)
한 AVL 트리가 newBlock()의 호출에 대한 응답으로 이동한 상태더라도 이전 라운드에 대한 쿼리에 응답할 준비가 되어 있어야 한다..(뭔말일까????)
commitUpTo()가 이전 답변을 GC하도록 허용할 때까지 이전 라운드에 대한 정확한 답변을 기억함으로써 그렇게 해야 합니다.
원장은 RWMutex를 제공하여 트래커가 최대 한 번의 수정 API 호출(아래), 또는 제로 수정 호출 및 임의의 수의 읽기 전용 호출로 호출되도록 합니다.
내부적으로 트래커가 읽기 전용 호출에 대한 응답으로 상태를 수정하는 경우 스레드 안전성을 보장하는 것은 트래커의 책임입니다.
*/
type ledgerTracker interface {
	// loadFromDisk loads the state of a tracker from persistent
	// storage.  The ledger argument allows loadFromDisk to load
	// blocks from the database, or access its own state.  The
	// ledgerForTracker interface abstracts away the details of
	// ledger internals so that individual trackers can be tested
	// in isolation. The provided round number represents the
	// current accounts storage round number.
	/*
		loadFromDisk는 영구 저장소에서 트래커의 상태를 로드합니다.
		ledger 인수를 사용하면 loadFromDisk가 데이터베이스에서 블록을 로드하거나 자체 상태에 액세스할 수 있습니다.
		ledgerForTracker 인터페이스는 개별 트래커를 격리하여 테스트할 수 있도록 원장 내부의 세부 정보를 추상화합니다.
		제공된 라운드 번호는 현재 계정 저장소의 라운드 번호를 나타냅니다.
	*/
	//loadFromDisk는 영구 저장소에서 트래커의 상태를 로드합니다.
	loadFromDisk(ledgerForTracker, basics.Round) error

	// newBlock informs the tracker of a new block along with
	// a given ledgercore.StateDelta as produced by BlockEvaluator.
	/*
		newBlock은 트래커에게 BlockEvaluator로부터 생성된 StateDelta와 함께 새로운 블록 정보를 알려준다.
	*/
	// newBlock은 트래커에게 BlockEvaluator로부터 생성된 StateDelta와 함께 새로운 블록 정보를 알려준다.
	newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta)

	// committedUpTo informs the tracker that the block database has
	// committed all blocks up to and including rnd to persistent
	// storage.  This can allow the tracker
	// to garbage-collect state that will not be needed.
	//
	// committedUpTo() returns the round number of the earliest
	// block that this tracker needs to be stored in the block
	// database for subsequent calls to loadFromDisk().
	// All blocks with round numbers before that may be deleted to
	// save space, and the tracker is expected to still function
	// after a restart and a call to loadFromDisk().
	// For example, returning 0 means that no blocks can be deleted.
	// Separetly, the method returns the lookback that is being
	// maintained by the tracker.
	/*
		commitUpTo는 트래커에게 블록 데이터베이스가 영구 저장소에 rnd까지의 모든 블록을 커밋했음을 알려준다..
		이렇게 하면 트래커가 필요없는 상태를 가비지 수집할 수 있습니다.
		loadFromDisk()호출 후 데이터베이스에 저장해야 하는 가장 이른 블록의 라운드 번호를 반환합니다.
		반환된 라운드 번호보다 작은 모든 블록은 공간을 절약하기 위해 삭제될 수 있으며
		트래커는 다시 시작하고 loadFromDisk()를 호출한 후에도 계속 작동할 것으로 예상됩니다.
		예를 들어 0을 반환하면 블록을 삭제할 수 없음을 의미합니다.
		별도로 이 메서드는 트래커에서 유지 관리 중인 룩백을 반환합니다.
	*/
	committedUpTo(basics.Round) (minRound, lookback basics.Round)

	// produceCommittingTask prepares a deferredCommitRange; Preparing a deferredCommitRange is a joint
	// effort, and all the trackers contribute to that effort. All the trackers are being handed a
	// pointer to the deferredCommitRange, and have the ability to either modify it, or return a
	// nil. If nil is returned, the commit would be skipped.
	// The contract:
	// offset must not be greater than the received dcr.offset value of non zero
	// oldBase must not be modifed if non zero
	/*
		produceCommittingTask deferredCommitRange를 준비합니다.
		deferredCommitRange를 준비하는 것은 공동 노력이며 모든 추적기가 그 노력에 기여합니다.
		모든 추적기는 deferredCommitRange에 대한 포인터를 전달받고 있으며 이를 수정하거나 nil을 반환할 수 있습니다.
		nil이 반환되면 커밋을 건너뜁니다.
		계약: 오프셋은 0이 아닌 경우 수신된 dcr.offset 값보다 크지 않아야 합니다.
		oldBase는 0이 아닌 경우 수정되지 않아야 합니다.
	*/
	// produceCommittingTask deferredCommitRange를 준비합니다.
	produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange

	// prepareCommit, commitRound and postCommit are called when it is time to commit tracker's data.
	// If an error returned the process is aborted.
	/*
		prepareCommit, commitRound and postCommit은 tracker의 데이터를 커밋해야 할 때 호출된다.
	*/

	// prepareCommit aligns the data structures stored in the deferredCommitContext with the current
	// state of the tracker. It allows the tracker to decide what data is going to be persisted
	// on the coming commitRound.
	prepareCommit(*deferredCommitContext) error
	// commitRound is called for each of the trackers after a deferredCommitContext was agreed upon
	// by all the prepareCommit calls. The commitRound is being executed within a single transactional
	// context, and so, if any of the tracker's commitRound calls fails, the transaction is rolled back.
	commitRound(context.Context, *sql.Tx, *deferredCommitContext) error
	// postCommit is called only on a successful commitRound. In that case, each of the trackers have
	// the chance to update it's internal data structures, knowing that the given deferredCommitContext
	// has completed. An optional context is provided for long-running operations.
	postCommit(context.Context, *deferredCommitContext)

	// postCommitUnlocked is called only on a successful commitRound. In that case, each of the trackers have
	// the chance to make changes that aren't state-dependent.
	// An optional context is provided for long-running operations.
	postCommitUnlocked(context.Context, *deferredCommitContext)

	// handleUnorderedCommit is a special method for handling deferred commits that are out of order.
	// Tracker might update own state in this case. For example, account updates tracker cancels
	// scheduled catchpoint writing that deferred commit.
	/*
		handleUnorderedCommit은 순서가 잘못된 지연된 커밋을 처리하기 위한 특별한 방법입니다.
	*/
	handleUnorderedCommit(uint64, basics.Round, basics.Round)

	// close terminates the tracker, reclaiming any resources
	// like open database connections or goroutines.  close may
	// be called even if loadFromDisk() is not called or does
	// not succeed.
	/*
		close는 열린 데이터베이스 연결 또는 고루틴과 같은 리소스를 회수하여 추적기를 종료합니다.
		loadFromDisk()가 호출되지 않거나 성공하지 못한 경우에도 close가 호출될 수 있습니다.
	*/
	close()
}

// ledgerForTracker defines the part of the ledger that a tracker can
// access.  This is particularly useful for testing trackers in isolation.
/*
ledgerForTracker는 트래커가 접근 할 수 있는 원장의 모듈에 대한 기능을 정의 한다.
이것은 특히 트래커를 격리해서 테스트할 때 유용하다.
*/
type ledgerForTracker interface {
	// tracker가 영구정보를 저장하는 db
	trackerDB() db.Pair
	// 원장이 영구정보를 저장하는 db
	blockDB() db.Pair
	trackerLog() logging.Logger
	// trackerEvalVerified는 accountUpdates가 loadFromDisk이 실행되는 동안 주어진 블록에 대한
	// StateDelta를 재구성하기 위해 사용하는 메소드이다.
	trackerEvalVerified(bookkeeping.Block, internal.LedgerForEvaluator) (ledgercore.StateDelta, error)

	// 가장 최근 라운드
	Latest() basics.Round
	// 해당 라운드의 블록 정보
	Block(basics.Round) (bookkeeping.Block, error)
	// 해당 라운드의 블록 헤더 정보
	BlockHdr(basics.Round) (bookkeeping.BlockHeader, error)
	// 제네시스 해시 정보
	GenesisHash() crypto.Digest
	// 제네시스 합의 정보
	GenesisProto() config.ConsensusParams
	// 제네시스 계정정보
	GenesisAccounts() map[basics.Address]basics.AccountData
}

// 트래커 정보 및 설정 정보 등을 가지고 있는 레지스트리
type trackerRegistry struct {
	// ledgerTracker들을 저장한 배열
	trackers []ledgerTracker

	// the accts has some exceptional usages in the tracker registry.
	accts *accountUpdates

	// ctx is the context for the committing go-routine.
	ctx context.Context
	// ctxCancel is the canceling function for canceling the committing go-routine ( i.e. signaling the committing go-routine that it's time to abort )
	ctxCancel context.CancelFunc

	// deferredCommits is the channel of pending deferred commits
	deferredCommits chan *deferredCommitContext

	// commitSyncerClosed is the blocking channel for synchronizing closing the commitSyncer goroutine. Once it's closed, the
	// commitSyncer can be assumed to have aborted.
	commitSyncerClosed chan struct{}

	// accountsWriting provides synchronization around the background writing of account balances.
	accountsWriting sync.WaitGroup

	// dbRound is always exactly accountsRound(),
	// cached to avoid SQL queries.
	dbRound basics.Round

	dbs db.Pair
	log logging.Logger

	// the synchronous mode that would be used for the account database.
	synchronousMode db.SynchronousMode

	// the synchronous mode that would be used while the accounts database is being rebuilt.
	accountsRebuildSynchronousMode db.SynchronousMode

	mu deadlock.RWMutex

	// lastFlushTime is the time we last flushed updates to
	// the accounts DB (bumping dbRound).
	lastFlushTime time.Time
}

// deferredCommitRange is used during the calls to produceCommittingTask, and used as a data structure
// to syncronize the various trackers and create a uniformity around which rounds need to be persisted
// next.
/*
deferredCommitRange는 productionCommittingTask를 호출하는 동안 사용되며
다양한 트래커를 동기화하고 다음에 지속되어야 하는 라운드에 대한 균일성을 생성하기 위한 데이터 구조로 사용됩니다.
*/
type deferredCommitRange struct {
	offset   uint64
	oldBase  basics.Round
	lookback basics.Round

	// pendingDeltas is the number of accounts that were modified within this commit context.
	// note that in this number we might have the same account being modified several times.
	pendingDeltas int

	isCatchpointRound bool

	// catchpointWriting is a pointer to a varible with the same name in the catchpointTracker.
	// it's used in order to reset the catchpointWriting flag from the acctupdates's
	// prepareCommit/commitRound ( which is called before the corresponding catchpoint tracker method )
	catchpointWriting *int32
}

// deferredCommitContext is used in order to syncornize the persistence of a given deferredCommitRange.
// prepareCommit, commitRound and postCommit are all using it to exchange data.
/*

deferredCommitContext는 주어진 deferredCommitRange의 영속성을 동기화하기 위해 사용됩니다.
prepareCommit, commitRound 및 postCommit은 모두 데이터를 교환하기위해 이것을 사용한다.
*/
type deferredCommitContext struct {
	deferredCommitRange

	newBase   basics.Round
	flushTime time.Time

	genesisProto config.ConsensusParams

	deltas                 []ledgercore.AccountDeltas
	roundTotals            ledgercore.AccountTotals
	compactAccountDeltas   compactAccountDeltas
	compactCreatableDeltas map[basics.CreatableIndex]ledgercore.ModifiedCreatable

	updatedPersistedAccounts []persistedAccountData

	committedRoundDigest     crypto.Digest
	trieBalancesHash         crypto.Digest
	updatingBalancesDuration time.Duration
	catchpointLabel          string

	stats       telemetryspec.AccountsUpdateMetrics
	updateStats bool
}

var errMissingAccountUpdateTracker = errors.New("initializeTrackerCaches : called without a valid accounts update tracker")

func (tr *trackerRegistry) initialize(l ledgerForTracker, trackers []ledgerTracker, cfg config.Local) (err error) {
	tr.dbs = l.trackerDB()
	tr.log = l.trackerLog()

	err = tr.dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		tr.dbRound, err = accountsRound(tx)
		return err
	})

	if err != nil {
		return err
	}

	tr.ctx, tr.ctxCancel = context.WithCancel(context.Background())
	tr.deferredCommits = make(chan *deferredCommitContext, 1)
	tr.commitSyncerClosed = make(chan struct{})
	tr.synchronousMode = db.SynchronousMode(cfg.LedgerSynchronousMode)
	tr.accountsRebuildSynchronousMode = db.SynchronousMode(cfg.AccountsRebuildSynchronousMode)
	go tr.commitSyncer(tr.deferredCommits)

	tr.trackers = append([]ledgerTracker{}, trackers...)

	for _, tracker := range tr.trackers {
		if accts, ok := tracker.(*accountUpdates); ok {
			tr.accts = accts
			break
		}
	}
	return
}

func (tr *trackerRegistry) loadFromDisk(l ledgerForTracker) error {
	tr.mu.RLock()
	dbRound := tr.dbRound
	tr.mu.RUnlock()

	for _, lt := range tr.trackers {
		err := lt.loadFromDisk(l, dbRound)
		if err != nil {
			// find the tracker name.
			trackerName := reflect.TypeOf(lt).String()
			return fmt.Errorf("tracker %s failed to loadFromDisk : %v", trackerName, err)
		}
	}

	err := tr.initializeTrackerCaches(l)
	if err != nil {
		return err
	}
	// the votes have a special dependency on the account updates, so we need to initialize these separetly.
	tr.accts.voters = &votersTracker{}
	err = tr.accts.voters.loadFromDisk(l, tr.accts)
	return err
}

func (tr *trackerRegistry) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	for _, lt := range tr.trackers {
		lt.newBlock(blk, delta)
	}
}

func (tr *trackerRegistry) committedUpTo(rnd basics.Round) basics.Round {
	minBlock := rnd
	maxLookback := basics.Round(0)
	for _, lt := range tr.trackers {
		retainRound, lookback := lt.committedUpTo(rnd)
		if retainRound < minBlock {
			minBlock = retainRound
		}
		if lookback > maxLookback {
			maxLookback = lookback
		}
	}

	tr.scheduleCommit(rnd, maxLookback)

	return minBlock
}

func (tr *trackerRegistry) scheduleCommit(blockqRound, maxLookback basics.Round) {
	tr.mu.RLock()
	dbRound := tr.dbRound
	tr.mu.RUnlock()

	dcc := &deferredCommitContext{
		deferredCommitRange: deferredCommitRange{
			lookback: maxLookback,
		},
	}
	cdr := &dcc.deferredCommitRange
	for _, lt := range tr.trackers {
		base := cdr.oldBase
		offset := cdr.offset
		cdr = lt.produceCommittingTask(blockqRound, dbRound, cdr)
		if cdr == nil {
			break
		}
		if offset > 0 && cdr.offset > offset {
			tr.log.Warnf("tracker %T produced offset %d but expected not greater than %d, dbRound %d, latestRound %d", lt, cdr.offset, offset, dbRound, blockqRound)
		}
		if base > 0 && base != cdr.oldBase {
			tr.log.Warnf("tracker %T modified oldBase %d that expected to be %d, dbRound %d, latestRound %d", lt, cdr.oldBase, base, dbRound, blockqRound)
		}
	}
	if cdr != nil {
		dcc.deferredCommitRange = *cdr
	} else {
		dcc = nil
	}

	tr.mu.RLock()
	// If we recently flushed, wait to aggregate some more blocks.
	// ( unless we're creating a catchpoint, in which case we want to flush it right away
	//   so that all the instances of the catchpoint would contain exactly the same data )
	flushTime := time.Now()
	if dcc != nil && !flushTime.After(tr.lastFlushTime.Add(balancesFlushInterval)) && !dcc.isCatchpointRound && dcc.pendingDeltas < pendingDeltasFlushThreshold {
		dcc = nil
	}
	tr.mu.RUnlock()

	if dcc != nil {
		tr.accountsWriting.Add(1)
		tr.deferredCommits <- dcc
	}
}

// waitAccountsWriting waits for all the pending ( or current ) account writing to be completed.
func (tr *trackerRegistry) waitAccountsWriting() {
	tr.accountsWriting.Wait()
}

func (tr *trackerRegistry) close() {
	if tr.ctxCancel != nil {
		tr.ctxCancel()
	}

	// close() is called from reloadLedger() when and trackerRegistry is not initialized yet
	if tr.commitSyncerClosed != nil {
		tr.waitAccountsWriting()
		// this would block until the commitSyncerClosed channel get closed.
		<-tr.commitSyncerClosed
	}

	for _, lt := range tr.trackers {
		lt.close()
	}
	tr.trackers = nil
	tr.accts = nil
}

// commitSyncer is the syncer go-routine function which perform the database updates. Internally, it dequeues deferredCommits and
// send the tasks to commitRound for completing the operation.
func (tr *trackerRegistry) commitSyncer(deferredCommits chan *deferredCommitContext) {
	defer close(tr.commitSyncerClosed)
	for {
		select {
		case commit, ok := <-deferredCommits:
			if !ok {
				return
			}
			tr.commitRound(commit)
		case <-tr.ctx.Done():
			// drain the pending commits queue:
			drained := false
			for !drained {
				select {
				case <-deferredCommits:
					tr.accountsWriting.Done()
				default:
					drained = true
				}
			}
			return
		}
	}
}

// commitRound commits the given deferredCommitContext via the trackers.
func (tr *trackerRegistry) commitRound(dcc *deferredCommitContext) {
	defer tr.accountsWriting.Done()
	tr.mu.RLock()

	offset := dcc.offset
	dbRound := dcc.oldBase
	lookback := dcc.lookback

	// we can exit right away, as this is the result of mis-ordered call to committedUpTo.
	if tr.dbRound < dbRound || offset < uint64(tr.dbRound-dbRound) {
		tr.log.Warnf("out of order deferred commit: offset %d, dbRound %d but current tracker DB round is %d", offset, dbRound, tr.dbRound)
		for _, lt := range tr.trackers {
			lt.handleUnorderedCommit(offset, dbRound, lookback)
		}
		tr.mu.RUnlock()
		return
	}

	// adjust the offset according to what happened meanwhile..
	offset -= uint64(tr.dbRound - dbRound)

	// if this iteration need to flush out zero rounds, just return right away.
	// this usecase can happen when two subsequent calls to committedUpTo concludes that the same rounds range need to be
	// flush, without the commitRound have a chance of committing these rounds.
	if offset == 0 {
		tr.mu.RUnlock()
		return
	}

	dbRound = tr.dbRound
	newBase := basics.Round(offset) + dbRound

	dcc.offset = offset
	dcc.oldBase = dbRound
	dcc.newBase = newBase
	dcc.flushTime = time.Now()

	for _, lt := range tr.trackers {
		err := lt.prepareCommit(dcc)
		if err != nil {
			tr.log.Errorf(err.Error())
			tr.mu.RUnlock()
			return
		}
	}
	tr.mu.RUnlock()

	start := time.Now()
	ledgerCommitroundCount.Inc(nil)
	err := tr.dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		for _, lt := range tr.trackers {
			err0 := lt.commitRound(ctx, tx, dcc)
			if err0 != nil {
				return err0
			}
		}

		err = updateAccountsRound(tx, dbRound+basics.Round(offset))
		if err != nil {
			return err
		}

		return nil
	})
	ledgerCommitroundMicros.AddMicrosecondsSince(start, nil)

	if err != nil {
		tr.log.Warnf("unable to advance tracker db snapshot (%d-%d): %v", dbRound, dbRound+basics.Round(offset), err)
		return
	}

	tr.mu.Lock()
	tr.dbRound = newBase
	for _, lt := range tr.trackers {
		lt.postCommit(tr.ctx, dcc)
	}
	tr.lastFlushTime = dcc.flushTime
	tr.mu.Unlock()

	for _, lt := range tr.trackers {
		lt.postCommitUnlocked(tr.ctx, dcc)
	}

}

// initializeTrackerCaches fills up the accountUpdates cache with the most recent ~320 blocks ( on normal execution ).
// the method also support balances recovery in cases where the difference between the lastBalancesRound and the lastestBlockRound
// is far greater than 320; in these cases, it would flush to disk periodically in order to avoid high memory consumption.
func (tr *trackerRegistry) initializeTrackerCaches(l ledgerForTracker) (err error) {
	lastestBlockRound := l.Latest()
	lastBalancesRound := tr.dbRound

	var blk bookkeeping.Block
	var delta ledgercore.StateDelta

	if tr.accts == nil {
		return errMissingAccountUpdateTracker
	}

	accLedgerEval := accountUpdatesLedgerEvaluator{
		au: tr.accts,
	}

	if lastBalancesRound < lastestBlockRound {
		accLedgerEval.prevHeader, err = l.BlockHdr(lastBalancesRound)
		if err != nil {
			return err
		}
	}

	skipAccountCacheMessage := make(chan struct{})
	writeAccountCacheMessageCompleted := make(chan struct{})
	defer func() {
		close(skipAccountCacheMessage)
		select {
		case <-writeAccountCacheMessageCompleted:
			if err == nil {
				tr.log.Infof("initializeTrackerCaches completed initializing account data caches")
			}
		default:
		}
	}()

	catchpointInterval := uint64(0)
	for _, tracker := range tr.trackers {
		if catchpointTracker, ok := tracker.(*catchpointTracker); ok {
			catchpointInterval = catchpointTracker.catchpointInterval
			break
		}
	}

	// this goroutine logs a message once if the parent function have not completed in initializingAccountCachesMessageTimeout seconds.
	// the message is important, since we're blocking on the ledger block database here, and we want to make sure that we log a message
	// within the above timeout.
	go func() {
		select {
		case <-time.After(initializingAccountCachesMessageTimeout):
			tr.log.Infof("initializeTrackerCaches is initializing account data caches")
			close(writeAccountCacheMessageCompleted)
		case <-skipAccountCacheMessage:
		}
	}()

	blocksStream := make(chan bookkeeping.Block, initializeCachesReadaheadBlocksStream)
	blockEvalFailed := make(chan struct{}, 1)
	var blockRetrievalError error
	go func() {
		defer close(blocksStream)
		for roundNumber := lastBalancesRound + 1; roundNumber <= lastestBlockRound; roundNumber++ {
			blk, blockRetrievalError = l.Block(roundNumber)
			if blockRetrievalError != nil {
				return
			}
			select {
			case blocksStream <- blk:
			case <-blockEvalFailed:
				return
			}
		}
	}()

	lastFlushedRound := lastBalancesRound
	const accountsCacheLoadingMessageInterval = 5 * time.Second
	lastProgressMessage := time.Now().Add(-accountsCacheLoadingMessageInterval / 2)

	// rollbackSynchronousMode ensures that we switch to "fast writing mode" when we start flushing out rounds to disk, and that
	// we exit this mode when we're done.
	rollbackSynchronousMode := false
	defer func() {
		if rollbackSynchronousMode {
			// restore default synchronous mode
			err0 := tr.dbs.Wdb.SetSynchronousMode(context.Background(), tr.synchronousMode, tr.synchronousMode >= db.SynchronousModeFull)
			// override the returned error only in case there is no error - since this
			// operation has a lower criticality.
			if err == nil {
				err = err0
			}
		}
	}()

	for blk := range blocksStream {
		delta, err = l.trackerEvalVerified(blk, &accLedgerEval)
		if err != nil {
			close(blockEvalFailed)
			return
		}
		tr.newBlock(blk, delta)

		// flush to disk if any of the following applies:
		// 1. if we have loaded up more than initializeCachesRoundFlushInterval rounds since the last time we flushed the data to disk
		// 2. if we completed the loading and we loaded up more than 320 rounds.
		flushIntervalExceed := blk.Round()-lastFlushedRound > initializeCachesRoundFlushInterval
		loadCompleted := (lastestBlockRound == blk.Round() && lastBalancesRound+basics.Round(blk.ConsensusProtocol().MaxBalLookback) < lastestBlockRound)
		if flushIntervalExceed || loadCompleted {
			// adjust the last flush time, so that we would not hold off the flushing due to "working too fast"
			tr.lastFlushTime = time.Now().Add(-balancesFlushInterval)

			if !rollbackSynchronousMode {
				// switch to rebuild synchronous mode to improve performance
				err0 := tr.dbs.Wdb.SetSynchronousMode(context.Background(), tr.accountsRebuildSynchronousMode, tr.accountsRebuildSynchronousMode >= db.SynchronousModeFull)
				if err0 != nil {
					tr.log.Warnf("initializeTrackerCaches was unable to switch to rbuild synchronous mode : %v", err0)
				} else {
					// flip the switch to rollback the synchronous mode once we're done.
					rollbackSynchronousMode = true
				}
			}

			var roundsBehind basics.Round

			// flush the account data
			tr.scheduleCommit(blk.Round(), basics.Round(config.Consensus[blk.BlockHeader.CurrentProtocol].MaxBalLookback))
			// wait for the writing to complete.
			tr.waitAccountsWriting()

			func() {
				tr.mu.RLock()
				defer tr.mu.RUnlock()

				// The au.dbRound after writing should be ~320 behind the block round.
				roundsBehind = blk.Round() - tr.dbRound
			}()

			// are we too far behind ? ( taking into consideration the catchpoint writing, which can stall the writing for quite a bit )
			if roundsBehind > initializeCachesRoundFlushInterval+basics.Round(catchpointInterval) {
				// we're unable to persist changes. This is unexpected, but there is no point in keep trying batching additional changes since any further changes
				// would just accumulate in memory.
				close(blockEvalFailed)
				tr.log.Errorf("initializeTrackerCaches was unable to fill up the account caches accounts round = %d, block round = %d. See above error for more details.", blk.Round()-roundsBehind, blk.Round())
				err = fmt.Errorf("initializeTrackerCaches failed to initialize the account data caches")
				return
			}

			// and once we flushed it to disk, update the lastFlushedRound
			lastFlushedRound = blk.Round()
		}

		// if enough time have passed since the last time we wrote a message to the log file then give the user an update about the progess.
		if time.Since(lastProgressMessage) > accountsCacheLoadingMessageInterval {
			// drop the initial message if we're got to this point since a message saying "still initializing" that comes after "is initializing" doesn't seems to be right.
			select {
			case skipAccountCacheMessage <- struct{}{}:
				// if we got to this point, we should be able to close the writeAccountCacheMessageCompleted channel to have the "completed initializing" message written.
				close(writeAccountCacheMessageCompleted)
			default:
			}
			tr.log.Infof("initializeTrackerCaches is still initializing account data caches, %d rounds loaded out of %d rounds", blk.Round()-lastBalancesRound, lastestBlockRound-lastBalancesRound)
			lastProgressMessage = time.Now()
		}

		// prepare for the next iteration.
		accLedgerEval.prevHeader = *delta.Hdr
	}

	if blockRetrievalError != nil {
		err = blockRetrievalError
	}
	return

}
