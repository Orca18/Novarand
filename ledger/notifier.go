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
	"sync"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/ledgercore"
)

// BlockListener represents an object that needs to get notified on new blocks.
// BlockListener는 새 블록에 대해 알림을 받아야 하는 개체를 나타냅니다.
/*
	BlockListener는 새 블록에 대한 알림을 받아야 하는 개체를 나타냅니다.
	즉, 블록이 생성되면 그것을 인지할 수 있어야 한다.
*/
type BlockListener interface {
	OnNewBlock(block bookkeeping.Block, delta ledgercore.StateDelta)
}

type blockDeltaPair struct {
	block bookkeeping.Block
	delta ledgercore.StateDelta
}

type blockNotifier struct {
	mu            deadlock.Mutex
	cond          *sync.Cond
	listeners     []BlockListener
	pendingBlocks []blockDeltaPair
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

func (bn *blockNotifier) worker() {
	defer bn.closing.Done()
	bn.mu.Lock()

	for {
		for bn.running && len(bn.pendingBlocks) == 0 {
			bn.cond.Wait()
		}

		if !bn.running {
			bn.mu.Unlock()
			return
		}

		blocks := bn.pendingBlocks
		listeners := bn.listeners
		bn.pendingBlocks = nil
		bn.mu.Unlock()

		for _, blk := range blocks {
			for _, listener := range listeners {
				listener.OnNewBlock(blk.block, blk.delta)
			}
		}

		bn.mu.Lock()
	}
}

func (bn *blockNotifier) close() {
	bn.mu.Lock()
	if bn.running {
		bn.running = false
		bn.cond.Broadcast()
	}
	bn.mu.Unlock()
	bn.closing.Wait()
}

func (bn *blockNotifier) loadFromDisk(l ledgerForTracker, _ basics.Round) error {
	bn.cond = sync.NewCond(&bn.mu)
	bn.running = true
	bn.pendingBlocks = nil
	bn.closing.Add(1)
	go bn.worker()
	return nil
}

// 새로운 블록이 추가됐을 때 호출 될 리스너를 추가한다.
func (bn *blockNotifier) register(listeners []BlockListener) {
	bn.mu.Lock()
	defer bn.mu.Unlock()

	bn.listeners = append(bn.listeners, listeners...)
}

// 새로운 블록 생성 시 호출하는 메소드
// pendingBlocks에 블록데이터를 추가한 후 브로드 캐스팅 한다.
func (bn *blockNotifier) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	bn.mu.Lock()
	defer bn.mu.Unlock()
	bn.pendingBlocks = append(bn.pendingBlocks, blockDeltaPair{block: blk, delta: delta})
	bn.cond.Broadcast()
}

func (bn *blockNotifier) committedUpTo(rnd basics.Round) (retRound, lookback basics.Round) {
	return rnd, basics.Round(0)
}

func (bn *blockNotifier) prepareCommit(dcc *deferredCommitContext) error {
	return nil
}

func (bn *blockNotifier) commitRound(context.Context, *sql.Tx, *deferredCommitContext) error {
	return nil
}

func (bn *blockNotifier) postCommit(ctx context.Context, dcc *deferredCommitContext) {
}

func (bn *blockNotifier) postCommitUnlocked(ctx context.Context, dcc *deferredCommitContext) {
}

func (bn *blockNotifier) handleUnorderedCommit(uint64, basics.Round, basics.Round) {
}

func (bn *blockNotifier) produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	return dcr
}
