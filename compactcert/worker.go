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

package compactcert

import (
	"context"
	"database/sql"
	"sync"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/crypto/compactcert"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/network"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/db"
)

type builder struct {
	*compactcert.Builder

	voters    *ledgercore.VotersForRound
	votersHdr bookkeeping.BlockHeader
}

// Worker builds compact certificates, by broadcasting signatures using this node's participation keys, by collecting signatures sent by others, and by sending out the resulting compact certs in a transaction.
// 작업자는 이 노드의 참여 키를 사용하여 서명을 브로드캐스트하고, 다른 사람이 보낸 서명을 수집하고, 트랜잭션에서 결과 압축 인증서를 전송하여 컴팩트 인증서를 빌드합니다.
type Worker struct {
	// The mutex serializes concurrent message handler invocations
	// from the network stack.
	mu deadlock.Mutex

	db        db.Accessor
	log       logging.Logger
	accts     Accounts
	ledger    Ledger
	net       Network
	txnSender TransactionSender

	// builders is indexed by the round of the block being signed.
	builders map[basics.Round]builder

	ctx      context.Context
	shutdown context.CancelFunc
	wg       sync.WaitGroup

	signed   basics.Round
	signedCh chan struct{}
}

// NewWorker constructs a new Worker, as used by the node.
// NewWorker는 노드에서 사용되는 새 Worker를 생성합니다.
func NewWorker(db db.Accessor, log logging.Logger, accts Accounts, ledger Ledger, net Network, txnSender TransactionSender) *Worker {
	ctx, cancel := context.WithCancel(context.Background())

	return &Worker{
		db:        db,
		log:       log,
		accts:     accts,
		ledger:    ledger,
		net:       net,
		txnSender: txnSender,
		builders:  make(map[basics.Round]builder),
		ctx:       ctx,
		shutdown:  cancel,
		signedCh:  make(chan struct{}, 1),
	}
}

// Start starts the goroutines for the worker.
func (ccw *Worker) Start() {
	err := ccw.db.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		return initDB(tx)
	})
	if err != nil {
		ccw.log.Warnf("ccw.Start(): initDB: %v", err)
		return
	}

	ccw.initBuilders()

	handlers := []network.TaggedMessageHandler{
		{Tag: protocol.CompactCertSigTag, MessageHandler: network.HandlerFunc(ccw.handleSigMessage)},
	}
	ccw.net.RegisterHandlers(handlers)

	latest := ccw.ledger.Latest()

	ccw.wg.Add(1)
	go ccw.signer(latest)

	ccw.wg.Add(1)
	go ccw.builder(latest)
}

// Shutdown stops any goroutines associated with this worker.
func (ccw *Worker) Shutdown() {
	ccw.shutdown()
	ccw.wg.Wait()
	ccw.db.Close()
}
