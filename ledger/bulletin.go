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
	"sync/atomic"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/ledgercore"
)

// bulletin은 고시, 공보라는 뜻이다. 즉, 무언가를 알려주는 역할을 하는 것 같다.

// notifier is a struct that encapsulates a single-shot channel; it will only be signaled once.
/*
notifier는 단일 샷 채널을 캡슐화하는 구조체입니다. 그것은 한 번만 신호됩니다
chan은 고루틴(쓰레드)사용 시 데이터를 전달할 때 사용하는 채널을 의미한다 chan뒤의 자료형의 데이터를 이동시킬 수 있다.
여기선 구조체를 이동할 수 있다!
*/
type notifier struct {
	signal   chan struct{}
	notified uint32
}

// makeNotifier constructs a notifier that has not been signaled.
func makeNotifier() notifier {
	return notifier{signal: make(chan struct{}), notified: 0}
}

// notify signals the channel if it hasn't already done so
/*
이전에 하지 않았다면 채널에 신호를 보낸다.
*/
func (notifier *notifier) notify() {
	if atomic.CompareAndSwapUint32(&notifier.notified, 0, 1) {
		close(notifier.signal)
	}
}

// bulletin provides an easy way to wait on a round to be written to the ledger.
// To use it, call <-Wait(round)
/*
bulletin은 원장에 기록되기 위한 라운드를 기다리는 쉬운 방법을 제공합니다. 그것을 사용하려면 <-Wait(round)를 호출해야 한다.
*/
type bulletin struct {
	mu                          deadlock.Mutex
	pendingNotificationRequests map[basics.Round]notifier
	latestRound                 basics.Round
}

func makeBulletin() *bulletin {
	b := new(bulletin)
	b.pendingNotificationRequests = make(map[basics.Round]notifier)
	return b
}

// Wait returns a channel which gets closed when the ledger reaches a given round.
/*
Wait은 원장이 주어진 라운드에 도달하면 닫히는 채널을 반환합니다.(흠.. 무슨 역할을 하는거지 진짜...)
*/
func (b *bulletin) Wait(round basics.Round) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Return an already-closed channel if we already have the block.
	/*
		이미 블록이 있는 경우 이미 닫힌 채널을 반환합니다.
	*/
	if round <= b.latestRound {
		closed := make(chan struct{})
		close(closed)
		return closed
	}

	signal, exists := b.pendingNotificationRequests[round]
	if !exists {
		signal = makeNotifier()
		b.pendingNotificationRequests[round] = signal
	}
	return signal.signal
}

func (b *bulletin) loadFromDisk(l ledgerForTracker, _ basics.Round) error {
	b.pendingNotificationRequests = make(map[basics.Round]notifier)
	b.latestRound = l.Latest()
	return nil
}

func (b *bulletin) close() {
}

func (b *bulletin) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
}

func (b *bulletin) committedUpTo(rnd basics.Round) (retRound, lookback basics.Round) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for pending, signal := range b.pendingNotificationRequests {
		if pending > rnd {
			continue
		}

		delete(b.pendingNotificationRequests, pending)
		signal.notify()
	}

	b.latestRound = rnd
	return rnd, basics.Round(0)
}

func (b *bulletin) prepareCommit(dcc *deferredCommitContext) error {
	return nil
}

func (b *bulletin) commitRound(context.Context, *sql.Tx, *deferredCommitContext) error {
	return nil
}

func (b *bulletin) postCommit(ctx context.Context, dcc *deferredCommitContext) {
}

func (b *bulletin) postCommitUnlocked(ctx context.Context, dcc *deferredCommitContext) {
}

func (b *bulletin) handleUnorderedCommit(uint64, basics.Round, basics.Round) {
}
func (b *bulletin) produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	return dcr
}
