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
	"sync"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/ledgercore"
)

// BlockListener represents an object that needs to get notified on new blocks.
/*
	BlockListener는 새 블록에 대한 알림을 받아야 하는 개체를 나타냅니다.
	즉, 블록이 생성되면 그것을 인지할 수 있어야 한다.
*/

type ValidateBlockListener interface {
	OnNewBlock2(block bookkeeping.Block, delta ledgercore.StateDelta)
}

type blockDeltaPair2 struct {
	block bookkeeping.Block
	delta ledgercore.StateDelta
}

type stateDeltaTracker struct {
	mu            deadlock.Mutex
	cond          *sync.Cond
	listener      ValidateBlockListener
	pendingBlocks []blockDeltaPair2
	running       bool
	// closing is the waitgroup used to synchronize closing the worker goroutine.
	// It's being increased during loadFromDisk, and the worker is responsible to call Done on it once it's aborting it's goroutine.
	// The close function waits on this to complete.

	/*
		닫기는 작업자 고루틴 닫기를 동기화하는 데 사용되는 대기 그룹입니다.
		loadFromDisk 동안 증가되고 작업자는 고루틴 작업을 중단하면 Done을 ​​호출할 책임이 있습니다.
		닫기 기능은 이 작업이 완료될 때까지 기다립니다.
		=>  결국 모든 고루틴이 종료될 때까지 기다리는 것 같다.
	*/
	closing sync.WaitGroup
}

/*
	트래커가 시작할 때 동일하게 시작하는 고루틴으로써 블록이 원장에 저장됐을 때 해당 블록과 스테이트델타 정보를
	blockTrackingTracker에게 전달하는 역할을 한다.
*/
func (sdt *stateDeltaTracker) stateDeltaWorker() {
	defer sdt.closing.Done()
	sdt.mu.Lock()

	for {
		for sdt.running && len(sdt.pendingBlocks) == 0 {
			sdt.cond.Wait()
		}
		if !sdt.running {
			sdt.mu.Unlock()
			return
		}
		blocks := sdt.pendingBlocks
		listener := sdt.listener
		sdt.pendingBlocks = nil
		sdt.mu.Unlock()
		// 블록의 갯수만큼 BlockTrackingListener위 OnNewBlock()를 호출한다.
		for _, blk := range blocks {
			// 얘가 nil이구나!!
			// 얘가 nil이려면 MakeFull()에서 이 객체가 안만들어져야되는구나.
			if listener != nil {
				listener.OnNewBlock2(blk.block, blk.delta)
			}
		}
		sdt.mu.Lock()
	}
}

/*
	해당 트래커를 종료한다
	stateDeltaWorker또한 종료한다.
*/
func (sdt *stateDeltaTracker) close() {
	sdt.mu.Lock()
	if sdt.running {
		sdt.running = false
		sdt.cond.Broadcast()
	}
	sdt.mu.Unlock()
	sdt.closing.Wait()
}

/*
	트래커를 시작하고 초기화한다.
*/
func (sdt *stateDeltaTracker) loadFromDisk(l ledgerForTracker, _ basics.Round) error {
	sdt.cond = sync.NewCond(&sdt.mu)
	sdt.running = true
	sdt.pendingBlocks = nil
	sdt.closing.Add(1)
	fmt.Println("시작!")
	go sdt.stateDeltaWorker()
	return nil
}

// 새로운 블록이 추가됐을 때 호출 될 리스너를 추가한다.
func (sdt *stateDeltaTracker) register(listener ValidateBlockListener) {
	sdt.mu.Lock()
	defer sdt.mu.Unlock()

	sdt.listener = listener
}

// 새로운 블록 생성 시 호출하는 메소드
// pendingBlocks에 블록데이터를 추가한 후 브로드 캐스팅 한다.
func (sdt *stateDeltaTracker) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	sdt.mu.Lock()
	defer sdt.mu.Unlock()
	sdt.pendingBlocks = append(sdt.pendingBlocks, blockDeltaPair2{block: blk, delta: delta})
	sdt.cond.Broadcast()
}

func (sdt *stateDeltaTracker) committedUpTo(rnd basics.Round) (retRound, lookback basics.Round) {
	return rnd, basics.Round(0)
}

func (sdt *stateDeltaTracker) prepareCommit(dcc *deferredCommitContext) error {
	return nil
}

func (sdt *stateDeltaTracker) commitRound(context.Context, *sql.Tx, *deferredCommitContext) error {
	return nil
}

func (sdt *stateDeltaTracker) postCommit(ctx context.Context, dcc *deferredCommitContext) {
}

func (sdt *stateDeltaTracker) postCommitUnlocked(ctx context.Context, dcc *deferredCommitContext) {
}

func (sdt *stateDeltaTracker) handleUnorderedCommit(uint64, basics.Round, basics.Round) {
}

func (sdt *stateDeltaTracker) produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	return dcr
}
