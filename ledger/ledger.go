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
	"fmt"
	"github.com/algorand/go-deadlock"
	"os"
	"time"

	"github.com/Orca18/novarand/agreement"
	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/crypto/compactcert"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/data/transactions/verify"
	"github.com/Orca18/novarand/ledger/apply"
	"github.com/Orca18/novarand/ledger/internal"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/db"
	"github.com/Orca18/novarand/util/execpool"
	"github.com/Orca18/novarand/util/metrics"
)

// Ledger is a database storing the contents of the ledger.
// 원장 객체는 원장의 컨텐츠들을 저장하는 데이터베이스이다.
/*
데이터베이스 연결정보와 블록이 임시로 저장될 큐와 데이터베이스 관리를 위한 변수,
제네시스 해시 및 계정과 초기값, 트래커에 대한 정보를 가지고 있다.
원장에 블록을 저장하고 관리하기 위한 메소드를 가지고 있다.
*/

type Ledger struct {
	// Database connections to the DBs storing blocks and tracker state. We use potentially different databases to avoid SQLite contention during catchup.
	// 블록과 트래커의 상태를 저장하는 db에 대한 커넥션이다. 캐치업(기존 블록체인 정보 다운로드)동안 SQLite연결의 충돌을 방지하기 위해 다른 db를 사용한다.
	trackerDBs db.Pair
	blockDBs   db.Pair

	// blockQ is the buffer of added blocks that will be flushed to persistent storage
	// 블록Q는 영구저장영역(디스크겠지?)에 저장될 블록들이 추가될 버퍼공간이다.
	blockQ *blockQueue

	log logging.Logger

	// archival determines whether the ledger keeps all blocks forever
	// (archival mode) or trims older blocks to save space (non-archival).
	// true이면 모든 블록정보를 저장한다(풀노드), false이면 가장 최근 1000개만 저장한다.
	archival bool

	// the synchronous mode that would be used for the ledger databases.
	// ledger databases를 위해 사용한다(비동기모드면 어떻게 작동하는거지?)
	synchronousMode db.SynchronousMode

	// the synchronous mode that would be used while the accounts database is being rebuilt.
	//accounts database가 재생성되는 동안 사용되는 동기화 모드
	accountsRebuildSynchronousMode db.SynchronousMode

	// genesisHash stores the genesis hash for this ledger.
	genesisHash crypto.Digest

	// genesis.json에 저장된 초기 계정보
	genesisAccounts map[basics.Address]basics.AccountData

	// 초기 합의 정보
	genesisProto config.ConsensusParams

	// State-machine trackers
	accts      accountUpdates
	catchpoint catchpointTracker
	txTail     txTail
	bulletin   bulletin
	notifier   blockNotifier
	metrics    metricsTracker
	//(추가)
	//statedelta stateDeltaTracker

	// 트래커 정보 및 설정 정보 등을 가지고 있는 레지스트리
	// []ledgerTracker을 가지고 있다.
	trackers  trackerRegistry
	trackerMu deadlock.RWMutex

	headerCache heapLRUCache

	// verifiedTxnCache holds all the verified transactions state
	verifiedTxnCache verify.VerifiedTransactionCache

	// 노드 인스턴스의 설정 정보
	cfg config.Local
	//현재 노드 루트 디렉토리
	rootDir string
	//보상받는 주소들 = []베이직.어드레스들 = transaction안에 certvote 주소
	certVoteAddresses []basics.Address
}

// OpenLedger creates a Ledger object, using SQLite database filenames
// based on dbPathPrefix (in-memory if dbMem is true). genesisInitState.Blocks and
// genesisInitState.Accounts specify the initial blocks and accounts to use if the
// database wasn't initialized before.
/*
	원장객체를 생성한다. db가 이전에 초기화 된적이 없다면 최초 블록들과 계정들을 초기화한다.
*/
func OpenLedger(
	log logging.Logger, dbPathPrefix string, dbMem bool, genesisInitState ledgercore.InitState, cfg config.Local,
) (*Ledger, error) {
	var err error
	verifiedCacheSize := cfg.VerifiedTranscationsCacheSize
	if verifiedCacheSize < cfg.TxPoolSize {
		verifiedCacheSize = cfg.TxPoolSize
		log.Warnf("The VerifiedTranscationsCacheSize in the config file was misconfigured to have smaller size then the TxPoolSize; The verified cache size was adjusted from %d to %d.", cfg.VerifiedTranscationsCacheSize, cfg.TxPoolSize)
	}

	l := &Ledger{
		log:                            log,
		archival:                       cfg.Archival,
		genesisHash:                    genesisInitState.GenesisHash,
		genesisAccounts:                genesisInitState.Accounts,
		genesisProto:                   config.Consensus[genesisInitState.Block.CurrentProtocol],
		synchronousMode:                db.SynchronousMode(cfg.LedgerSynchronousMode),
		accountsRebuildSynchronousMode: db.SynchronousMode(cfg.AccountsRebuildSynchronousMode),
		verifiedTxnCache:               verify.MakeVerifiedTransactionCache(verifiedCacheSize),
		cfg:                            cfg,
	}

	l.headerCache.maxEntries = 10

	defer func() {
		if err != nil {
			l.Close()
		}
	}()

	// 트래커, 블록정보를 저장할 db에 접근할 수 있는 접근자 생성.
	l.trackerDBs, l.blockDBs, err = openLedgerDB(dbPathPrefix, dbMem)

	if err != nil {
		err = fmt.Errorf("OpenLedger.openLedgerDB %v", err)
		return nil, err
	}
	// 각 접근자에게 로거 세팅
	l.trackerDBs.Rdb.SetLogger(log)
	l.trackerDBs.Wdb.SetLogger(log)
	l.blockDBs.Rdb.SetLogger(log)
	l.blockDBs.Wdb.SetLogger(log)

	// 동기화모드 세팅 => 비동기화면 어떻게 되는거지?
	l.setSynchronousMode(context.Background(), l.synchronousMode)

	start := time.Now()
	// 블록카운트를 초기화한다. => 블록 생성시마다 올려주나?
	ledgerInitblocksdbCount.Inc(nil)
	err = l.blockDBs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		// 블록 db를 초기화한다.
		return initBlocksDB(tx, l, []bookkeeping.Block{genesisInitState.Block}, cfg.Archival)
	})

	ledgerInitblocksdbMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		err = fmt.Errorf("OpenLedger.initBlocksDB %v", err)
		return nil, err
	}

	// 제네시스 계정이 nil이라면 key: 주소 val: AccountData인 맵을 만들어준다.
	if l.genesisAccounts == nil {
		l.genesisAccounts = make(map[basics.Address]basics.AccountData)
	}

	// accountUpdates 초기화
	l.accts.initialize(cfg)
	// catchpointTracker structure 초기화
	l.catchpoint.initialize(cfg, dbPathPrefix)

	// 블록큐, 트래커등을 초기화 한다.
	err = l.reloadLedger()
	if err != nil {
		return nil, err
	}

	return l, nil
}

/*
블록큐, 트래커등을 초기화 한다.
*/
func (l *Ledger) reloadLedger() error {
	// similar to the Close function, we want to start by closing the blockQ first. The
	// blockQ is having a sync goroutine which indirectly calls other trackers. We want to eliminate that go-routine first,
	// and follow up by taking the trackers lock.
	if l.blockQ != nil {
		l.blockQ.close()
		l.blockQ = nil
	}

	// take the trackers lock. This would ensure that no other goroutine is using the trackers.
	l.trackerMu.Lock()
	defer l.trackerMu.Unlock()

	// close the trackers.
	l.trackers.close()

	// init block queue
	var err error
	l.blockQ, err = bqInit(l)
	if err != nil {
		err = fmt.Errorf("reloadLedger.bqInit %v", err)
		return err
	}

	// init tracker db
	trackerDBInitParams, err := trackerDBInitialize(l, l.catchpoint.catchpointEnabled(), l.catchpoint.dbDirectory)
	if err != nil {
		return err
	}

	// set account updates tracker as a driver to calculate tracker db round and committing offsets
	trackers := []ledgerTracker{
		&l.accts,      // update the balances
		&l.catchpoint, // catchpoints tracker : update catchpoint labels, create catchpoint files
		&l.txTail,     // update the transaction tail, tracking the recent 1000 txn
		&l.bulletin,   // provide closed channel signaling support for completed rounds
		&l.notifier,   // send OnNewBlocks to subscribers
		&l.metrics,    // provides metrics reporting support
		//&l.statedelta,    // stateDelta Tracker 추가
	}

	err = l.trackers.initialize(l, trackers, l.cfg)
	if err != nil {
		return err
	}

	err = l.trackers.loadFromDisk(l)
	if err != nil {
		err = fmt.Errorf("reloadLedger.loadFromDisk %v", err)
		return err
	}

	// post-init actions
	if trackerDBInitParams.vacuumOnStartup || l.cfg.OptimizeAccountsDatabaseOnStartup {
		err = l.accts.vacuumDatabase(context.Background())
		if err != nil {
			return err
		}
	}

	// Check that the genesis hash, if present, matches.
	err = l.verifyMatchingGenesisHash()
	if err != nil {
		return err
	}
	return nil
}

// verifyMatchingGenesisHash tests to see that the latest block header pointing to the same genesis hash provided in genesisHash.
func (l *Ledger) verifyMatchingGenesisHash() (err error) {
	// Check that the genesis hash, if present, matches.
	start := time.Now()
	ledgerVerifygenhashCount.Inc(nil)
	err = l.blockDBs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		latest, err := blockLatest(tx)
		if err != nil {
			return err
		}

		hdr, err := blockGetHdr(tx, latest)
		if err != nil {
			return err
		}

		params := config.Consensus[hdr.CurrentProtocol]
		if params.SupportGenesisHash && hdr.GenesisHash != l.genesisHash {
			return fmt.Errorf(
				"latest block %d genesis hash %v does not match expected genesis hash %v",
				latest, hdr.GenesisHash, l.genesisHash,
			)
		}
		return nil
	})
	ledgerVerifygenhashMicros.AddMicrosecondsSince(start, nil)
	return
}

// sqlite파일을 생성한다.
func openLedgerDB(dbPathPrefix string, dbMem bool) (trackerDBs db.Pair, blockDBs db.Pair, err error) {
	// Backwards compatibility: we used to store both blocks and tracker
	// state in a single SQLite db file.
	var trackerDBFilename string
	var blockDBFilename string

	if !dbMem {
		commonDBFilename := dbPathPrefix + ".sqlite"
		_, err = os.Stat(commonDBFilename)
		if !os.IsNotExist(err) {
			// before launch, we used to have both blocks and tracker
			// state in a single SQLite db file. We don't have that anymore,
			// and we want to fail when that's the case.
			err = fmt.Errorf("A single ledger database file '%s' was detected. This is no longer supported by current binary", commonDBFilename)
			return
		}
	}

	// 새로운 sqlite만드려면 여기에서 생성하면 된다.

	trackerDBFilename = dbPathPrefix + ".tracker.sqlite"
	blockDBFilename = dbPathPrefix + ".block.sqlite"

	outErr := make(chan error, 2)
	go func() {
		var lerr error
		// 트래커 정보 조회, 저장할 수 있는 db접근자 생성
		trackerDBs, lerr = db.OpenPair(trackerDBFilename, dbMem)
		outErr <- lerr
	}()

	go func() {
		var lerr error
		// 블록 정보 조회, 저장할 수 있는 db접근자 생성
		blockDBs, lerr = db.OpenPair(blockDBFilename, dbMem)
		outErr <- lerr
	}()

	err = <-outErr
	if err != nil {
		return
	}
	err = <-outErr
	return
}

// setSynchronousMode sets the writing database connections synchronous mode to the specified mode
// 몇개의 SynchronousMode가 있는데 그 중 어떤것으로 지정한다.
func (l *Ledger) setSynchronousMode(ctx context.Context, synchronousMode db.SynchronousMode) {
	if synchronousMode < db.SynchronousModeOff || synchronousMode > db.SynchronousModeExtra {
		l.log.Warnf("ledger.setSynchronousMode unable to set synchronous mode : requested value %d is invalid", synchronousMode)
		return
	}

	err := l.blockDBs.Wdb.SetSynchronousMode(ctx, synchronousMode, synchronousMode >= db.SynchronousModeFull)
	if err != nil {
		l.log.Warnf("ledger.setSynchronousMode unable to set synchronous mode on blocks db: %v", err)
		return
	}

	err = l.trackerDBs.Wdb.SetSynchronousMode(ctx, synchronousMode, synchronousMode >= db.SynchronousModeFull)
	if err != nil {
		l.log.Warnf("ledger.setSynchronousMode unable to set synchronous mode on trackers db: %v", err)
		return
	}
}

// initBlocksDB performs DB initialization:
// - creates and populates it with genesis blocks
// - ensures DB is in good shape for archival mode and resets it if not
/*
	블록 db 초기화를 진행한다.
*/
func initBlocksDB(tx *sql.Tx, l *Ledger, initBlocks []bookkeeping.Block, isArchival bool) (err error) {
	err = blockInit(tx, initBlocks)
	if err != nil {
		err = fmt.Errorf("initBlocksDB.blockInit %v", err)
		return err
	}

	// in archival mode check if DB contains all blocks up to the latest
	if isArchival {
		earliest, err := blockEarliest(tx)
		if err != nil {
			err = fmt.Errorf("initBlocksDB.blockEarliest %v", err)
			return err
		}

		// Detect possible problem - archival node needs all block but have only subsequence of them
		// So reset the DB and init it again
		if earliest != basics.Round(0) {
			l.log.Warnf("resetting blocks DB (earliest block is %v)", earliest)
			err := blockResetDB(tx)
			if err != nil {
				err = fmt.Errorf("initBlocksDB.blockResetDB %v", err)
				return err
			}
			err = blockInit(tx, initBlocks)
			if err != nil {
				err = fmt.Errorf("initBlocksDB.blockInit 2 %v", err)
				return err
			}
		}
	}

	return nil
}

// Close reclaims resources used by the ledger (namely, the database connection
// and goroutines used by trackers).
func (l *Ledger) Close() {
	// we shut the the blockqueue first, since it's sync goroutine dispatches calls
	// back to the trackers.
	if l.blockQ != nil {
		l.blockQ.close()
		l.blockQ = nil
	}

	// take the trackers lock. This would ensure that no other goroutine is using the trackers.
	l.trackerMu.Lock()
	defer l.trackerMu.Unlock()

	// then, we shut down the trackers and their corresponding goroutines.
	l.trackers.close()

	// last, we close the underlying database connections.
	l.blockDBs.Close()
	l.trackerDBs.Close()
}

// RegisterBlockListeners registers listeners that will be called when a new block is added to the ledger.
// RegisterBlockListeners 는 새 블록이 원장에 추가될 때 호출될 수신기를 등록합니다.
func (l *Ledger) RegisterBlockListeners(listeners []BlockListener) {
	l.notifier.register(listeners)
}

// notifyCommit informs the trackers that all blocks up to r have been
// written to disk.  Returns the minimum block number that must be kept
// in the database.
func (l *Ledger) notifyCommit(r basics.Round) basics.Round {
	l.trackerMu.Lock()
	defer l.trackerMu.Unlock()
	minToSave := l.trackers.committedUpTo(r)

	if l.archival {
		// Do not forget any blocks.
		minToSave = 0
	}

	return minToSave
}

// GetLastCatchpointLabel returns the latest catchpoint label that was written to the
// database.
func (l *Ledger) GetLastCatchpointLabel() string {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.catchpoint.GetLastCatchpointLabel()
}

// GetCreatorForRound takes a CreatableIndex and a CreatableType and tries to
// look up a creator address, setting ok to false if the query succeeded but no
// creator was found.
func (l *Ledger) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (creator basics.Address, ok bool, err error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.GetCreatorForRound(rnd, cidx, ctype)
}

// GetCreator is like GetCreatorForRound, but for the latest round and race-free
// with respect to ledger.Latest()
func (l *Ledger) GetCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.GetCreatorForRound(l.blockQ.latest(), cidx, ctype)
}

// CompactCertVoters returns the top online accounts at round rnd.
// The result might be nil, even with err=nil, if there are no voters
// for that round because compact certs were not enabled.
func (l *Ledger) CompactCertVoters(rnd basics.Round) (*ledgercore.VotersForRound, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.voters.getVoters(rnd)
}

// ListAssets takes a maximum asset index and maximum result length, and
// returns up to that many CreatableLocators from the database where app idx is
// less than or equal to the maximum.
func (l *Ledger) ListAssets(maxAssetIdx basics.AssetIndex, maxResults uint64) (results []basics.CreatableLocator, err error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.ListAssets(maxAssetIdx, maxResults)
}

// ListApplications takes a maximum app index and maximum result length, and
// returns up to that many CreatableLocators from the database where app idx is
// less than or equal to the maximum.
func (l *Ledger) ListApplications(maxAppIdx basics.AppIndex, maxResults uint64) (results []basics.CreatableLocator, err error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.ListApplications(maxAppIdx, maxResults)
}

// Lookup uses the accounts tracker to return the account state for a
// given account in a particular round.  The account values reflect
// the changes of all blocks up to and including rnd.
/*
Lookup은 특정 라운드의 해당 계정의 상태를 반환하기 위해 accounts tracker를 사용 계정 값은 rnd를 포함한 이전 모든라운드의 변화를 반영한다.
*/
func (l *Ledger) Lookup(rnd basics.Round, addr basics.Address) (basics.AccountData, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()

	// Intentionally apply (pending) rewards up to rnd.
	data, err := l.accts.LookupWithRewards(rnd, addr)
	if err != nil {
		return basics.AccountData{}, err
	}

	return data, nil
}

// LookupAgreement returns account data used by agreement.
func (l *Ledger) LookupAgreement(rnd basics.Round, addr basics.Address) (basics.OnlineAccountData, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()

	// Intentionally apply (pending) rewards up to rnd.
	data, err := l.accts.LookupWithRewards(rnd, addr)
	if err != nil {
		return basics.OnlineAccountData{}, err
	}

	return data.OnlineAccountData(), nil
}

// LookupWithoutRewards is like Lookup but does not apply pending rewards up
// to the requested round rnd.
func (l *Ledger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()

	data, validThrough, err := l.accts.LookupWithoutRewards(rnd, addr)
	if err != nil {
		return basics.AccountData{}, basics.Round(0), err
	}

	return data, validThrough, nil
}

// LatestTotals returns the totals of all accounts for the most recent round, as well as the round number.
/*
LatestTotals은 가장 최신라운드와 그 라운드에 있는 모든 계정이 가지고 있는 알고양을 반환(온라인, 오프라인, 참여안하는 계정별로 나눠서)
*/
func (l *Ledger) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.accts.LatestTotals()
}

// OnlineTotals returns the online totals of all accounts at the end of round rnd.
func (l *Ledger) OnlineTotals(rnd basics.Round) (basics.MicroAlgos, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	totals, err := l.accts.Totals(rnd)
	if err != nil {
		return basics.MicroAlgos{}, err
	}
	return totals.Online.Money, nil
}

// CheckDup return whether a transaction is a duplicate one.
func (l *Ledger) CheckDup(currentProto config.ConsensusParams, current basics.Round, firstValid basics.Round, lastValid basics.Round, txid transactions.Txid, txl ledgercore.Txlease) error {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.txTail.checkDup(currentProto, current, firstValid, lastValid, txid, txl)
}

// Latest returns the latest known block round added to the ledger.
func (l *Ledger) Latest() basics.Round {
	return l.blockQ.latest()
}

// LatestCommitted returns the last block round number written to
// persistent storage.  This block, and all previous blocks, are
// guaranteed to be available after a crash. In addition, it returns
// the latest block round number added to the ledger ( which will be
// flushed to persistent storage later on )
func (l *Ledger) LatestCommitted() (basics.Round, basics.Round) {
	return l.blockQ.latestCommitted()
}

// Block returns the block for round rnd.
func (l *Ledger) Block(rnd basics.Round) (blk bookkeeping.Block, err error) {
	return l.blockQ.getBlock(rnd)
}

// BlockHdr returns the BlockHeader of the block for round rnd.
func (l *Ledger) BlockHdr(rnd basics.Round) (blk bookkeeping.BlockHeader, err error) {
	value, exists := l.headerCache.Get(rnd)
	if exists {
		blk = value.(bookkeeping.BlockHeader)
		return
	}

	blk, err = l.blockQ.getBlockHdr(rnd)
	if err == nil {
		l.headerCache.Put(rnd, blk)
	}
	return
}

// EncodedBlockCert returns the encoded block and the corresponding encoded certificate of the block for round rnd.
func (l *Ledger) EncodedBlockCert(rnd basics.Round) (blk []byte, cert []byte, err error) {
	return l.blockQ.getEncodedBlockCert(rnd)
}

// BlockCert returns the block and the certificate of the block for round rnd.
func (l *Ledger) BlockCert(rnd basics.Round) (blk bookkeeping.Block, cert agreement.Certificate, err error) {
	return l.blockQ.getBlockCert(rnd)
}

// AddBlock adds a new block to the ledger.  The block is stored in an
// in-memory queue and is written to the disk in the background.  An error
// is returned if this is not the expected next block number.
/*
	새로운 블록을 원장에 등록한다. 블록은 in-memory큐에 저장되고(블록큐!아 이게 heap에 있나보네 즉, 프로세스에서만 유지되는!)
	백그라운드에서 디스크(영구저장영역 - hdd나 sdd)에 저장도니다.
*/
func (l *Ledger) AddBlock(blk bookkeeping.Block, cert agreement.Certificate) error {
	// passing nil as the executionPool is ok since we've asking the evaluator to skip verification.
	//certVote한 Sender목록을 슬라이스에 담아서 Eval로 넘긴다.
	var rwAddresses []basics.Address
	for _, address := range cert.Votes {
		rwAddress := address.Sender
		rwAddresses = append(rwAddresses, rwAddress)
	}
	l.certVoteAddresses = rwAddresses
	// 블록을 검증하고 stateDelta를 반환한다.
	updates, err := internal.Eval(context.Background(), l, blk, false,
		l.verifiedTxnCache, nil, l.certVoteAddresses)
	if err != nil {
		if errNSBE, ok := err.(ledgercore.ErrNonSequentialBlockEval); ok && errNSBE.EvaluatorRound <= errNSBE.LatestRound {
			return ledgercore.BlockInLedgerError{
				LastRound: errNSBE.EvaluatorRound,
				NextRound: errNSBE.LatestRound + 1}
		}
		return err
	}
	// 검증된 블록을 생성한다.
	vb := ledgercore.MakeValidatedBlock(blk, updates)
	return l.AddValidatedBlock(vb, cert)
}

// AddValidatedBlock adds a new block to the ledger, after the block has
// been validated by calling Ledger.Validate().  This saves the cost of
// having to re-compute the effect of the block on the ledger state, if
// the block has previously been validated.  Otherwise, AddValidatedBlock
// behaves like AddBlock.
/*
	검증된 블록을 원장에 저장한다. 만약 검증된 블록이라면 검증을 위한 계산을 하지 않지만
	그렇지 않다면 AddBlock와 동일하게 작동한다(검증을 하는 것 같다.)
*/
func (l *Ledger) AddValidatedBlock(vb ledgercore.ValidatedBlock, cert agreement.Certificate) error {
	// Grab the tracker lock first, to ensure newBlock() is notified before committedUpTo().
	l.trackerMu.Lock()
	defer l.trackerMu.Unlock()

	blk := vb.Block()

	err := l.blockQ.putBlock(blk, cert)
	if err != nil {
		return err
	}
	l.headerCache.Put(blk.Round(), blk.BlockHeader)
	l.trackers.newBlock(blk, vb.Delta())
	l.log.Debugf("ledger.AddValidatedBlock: added blk %d", blk.Round())
	return nil
}

// WaitForCommit waits until block r (and block before r) are durably
// written to disk.
func (l *Ledger) WaitForCommit(r basics.Round) {
	l.blockQ.waitCommit(r)
}

// Wait returns a channel that closes once a given round is stored
// durably in the ledger.
// When <-l.Wait(r) finishes, ledger is guaranteed to have round r,
// and will not lose round r after a crash.
// This makes it easy to use in a select{} statement.
func (l *Ledger) Wait(r basics.Round) chan struct{} {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.bulletin.Wait(r)
}

// GenesisHash returns the genesis hash for this ledger.
func (l *Ledger) GenesisHash() crypto.Digest {
	return l.genesisHash
}

// GenesisProto returns the initial protocol for this ledger.
func (l *Ledger) GenesisProto() config.ConsensusParams {
	return l.genesisProto
}

// GenesisAccounts returns initial accounts for this ledger.
func (l *Ledger) GenesisAccounts() map[basics.Address]basics.AccountData {
	return l.genesisAccounts
}

// GetCatchpointCatchupState returns the current state of the catchpoint catchup.
// GetCatchpointCatchupState는 catchpoint catchup의 현재 상태를 반환합니다.
func (l *Ledger) GetCatchpointCatchupState(ctx context.Context) (state CatchpointCatchupState, err error) {
	return MakeCatchpointCatchupAccessor(l, l.log).GetState(ctx)
}

// GetCatchpointStream returns a ReadCloseSizer file stream from which the catchpoint file
// for the provided round could be retrieved. If no such stream can be generated, a non-nil
// error is returned. The io.ReadCloser and the error are mutually exclusive -
// if error is returned, the file stream is guaranteed to be nil, and vice versa,
// if the file stream is not nil, the error is guaranteed to be nil.
func (l *Ledger) GetCatchpointStream(round basics.Round) (ReadCloseSizer, error) {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.catchpoint.GetCatchpointStream(round)
}

// ledgerForTracker methods
func (l *Ledger) trackerDB() db.Pair {
	return l.trackerDBs
}

// ledgerForTracker methods
func (l *Ledger) blockDB() db.Pair {
	return l.blockDBs
}

func (l *Ledger) trackerLog() logging.Logger {
	return l.log
}

// trackerEvalVerified is used by the accountUpdates to reconstruct the ledgercore.StateDelta from a given block during it's loadFromDisk execution.
// when this function is called, the trackers mutex is expected already to be taken. The provided accUpdatesLedger would allow the
// evaluator to shortcut the "main" ledger ( i.e. this struct ) and avoid taking the trackers lock a second time.
/*
trackerEvalVerified는 accountUpdates가 loadFromDisk이 실행되는 동안 주어진 블록에 대한 StateDelta를 재구성하기 위해 사용하는 메소드이다.
*/
func (l *Ledger) trackerEvalVerified(blk bookkeeping.Block, accUpdatesLedger internal.LedgerForEvaluator) (ledgercore.StateDelta, error) {
	// passing nil as the executionPool is ok since we've asking the evaluator to skip verification.
	return internal.Eval(context.Background(), accUpdatesLedger, blk, false, l.verifiedTxnCache, nil, l.certVoteAddresses)
}

// IsWritingCatchpointFile returns true when a catchpoint file is being generated. The function is used by the catchup service
// to avoid memory pressure until the catchpoint file writing is complete.
func (l *Ledger) IsWritingCatchpointFile() bool {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.catchpoint.IsWritingCatchpointFile()
}

// VerifiedTransactionCache returns the verify.VerifiedTransactionCache
func (l *Ledger) VerifiedTransactionCache() verify.VerifiedTransactionCache {
	return l.verifiedTxnCache
}

// StartEvaluator creates a BlockEvaluator, given a ledger and a block header
// of the block that the caller is planning to evaluate. If the length of the
// payset being evaluated is known in advance, a paysetHint >= 0 can be
// passed, avoiding unnecessary payset slice growth. The optional maxTxnBytesPerBlock parameter
// provides a cap on the size of a single generated block size, when a non-zero value is passed.
// If a value of zero or less is passed to maxTxnBytesPerBlock, the consensus MaxTxnBytesPerBlock would
// be used instead.
func (l *Ledger) StartEvaluator(hdr bookkeeping.BlockHeader, paysetHint, maxTxnBytesPerBlock int) (*internal.BlockEvaluator, error) {
	return internal.StartEvaluator(l, hdr,
		internal.EvaluatorOptions{
			PaysetHint:          paysetHint,
			Generate:            true,
			Validate:            true,
			MaxTxnBytesPerBlock: maxTxnBytesPerBlock,
		})
}

// Validate uses the ledger to validate block blk as a candidate next block.
// It returns an error if blk is not the expected next block, or if blk is
// not a valid block (e.g., it has duplicate transactions, overspends some
// account, etc).
func (l *Ledger) Validate(ctx context.Context, blk bookkeeping.Block, executionPool execpool.BacklogPool) (*ledgercore.ValidatedBlock, error) {
	var emptyCertVoteAddresses []basics.Address
	delta, err := internal.Eval(ctx, l, blk, true, l.verifiedTxnCache, executionPool, emptyCertVoteAddresses)
	if err != nil {
		return nil, err
	}
	vb := ledgercore.MakeValidatedBlock(blk, delta)
	return &vb, nil
}

// CompactCertParams computes the parameters for building or verifying
// a compact cert for block hdr, using voters from block votersHdr.
func CompactCertParams(votersHdr bookkeeping.BlockHeader, hdr bookkeeping.BlockHeader) (res compactcert.Params, err error) {
	return internal.CompactCertParams(votersHdr, hdr)
}

// AcceptableCompactCertWeight computes the acceptable signed weight
// of a compact cert if it were to appear in a transaction with a
// particular firstValid round.  Earlier rounds require a smaller cert.
// votersHdr specifies the block that contains the Merkle commitment of
// the voters for this compact cert (and thus the compact cert is for
// votersHdr.Round() + CompactCertRounds).
//
// logger must not be nil; use at least logging.Base()
func AcceptableCompactCertWeight(votersHdr bookkeeping.BlockHeader, firstValid basics.Round, logger logging.Logger) uint64 {
	return internal.AcceptableCompactCertWeight(votersHdr, firstValid, logger)
}

// DebuggerLedger defines the minimal set of method required for creating a debug balances.
type DebuggerLedger = internal.LedgerForCowBase

// MakeDebugBalances creates a ledger suitable for dryrun and debugger
func MakeDebugBalances(l DebuggerLedger, round basics.Round, proto protocol.ConsensusVersion, prevTimestamp int64) apply.Balances {
	return internal.MakeDebugBalances(l, round, proto, prevTimestamp)
}

func (l *Ledger) SetLedgerRootDir(directory string) {
	l.rootDir = directory
}
func (l *Ledger) GetLedgerRootDir() string {
	return l.rootDir
}

var ledgerInitblocksdbCount = metrics.NewCounter("ledger_initblocksdb_count", "calls")
var ledgerInitblocksdbMicros = metrics.NewCounter("ledger_initblocksdb_micros", "µs spent")
var ledgerVerifygenhashCount = metrics.NewCounter("ledger_verifygenhash_count", "calls")
var ledgerVerifygenhashMicros = metrics.NewCounter("ledger_verifygenhash_micros", "µs spent")
