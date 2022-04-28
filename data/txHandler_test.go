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

package data

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Orca18/novarand/components/mocks"
	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/pools"
	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/execpool"
)

func BenchmarkTxHandlerProcessDecoded(b *testing.B) {
	b.StopTimer()
	b.ResetTimer()
	// 사용자 수
	const numUsers = 100

	log := logging.TestingLog(b)
	// 각 사용자들의 secret(서명 위해 필요) 슬라이스
	secrets := make([]*crypto.SignatureSecrets, numUsers)
	// 각 사용자들의 주소 슬라이스
	addresses := make([]basics.Address, numUsers)

	// key: 주소, val: AccountData인 맵을 만든다
	genesis := make(map[basics.Address]basics.AccountData)

	//
	for i := 0; i < numUsers; i++ {
		// 메시지에 서명하기 위한 키쌍 생성
		secret := keypair()
		// pubkey(secret.SignatureVerifier)를 넣어서 주소 생성
		addr := basics.Address(secret.SignatureVerifier)
		secrets[i] = secret
		addresses[i] = addr

		// 블록체인의 초기 생성 시 존재하는 계정 만들어 줌
		genesis[addr] = basics.AccountData{
			Status:     basics.Online,
			MicroNovas: basics.MicroNovas{Raw: 10000000000000},
		}
	}

	// 사용자외에 pool주소 추가(아 rewardpool)
	genesis[poolAddr] = basics.AccountData{
		Status:     basics.NotParticipating,
		MicroNovas: basics.MicroNovas{Raw: config.Consensus[protocol.ConsensusCurrentVersion].MinBalance},
	}

	// 계정이 사용자수 + 1(pool)만큼 잘 생성됐는지 확인
	require.Equal(b, len(genesis), numUsers+1)

	// sinkAddr: feeSink , poolAddr: reward
	// 시작 시 필요한 알고 반환
	genBal := bookkeeping.MakeGenesisBalances(genesis, sinkAddr, poolAddr)
	ledgerName := fmt.Sprintf("%s-mem-%d", b.Name(), b.N)
	const inMem = true
	// 로컬에서 사용할 세팅값 가져옴
	cfg := config.GetDefaultLocal()
	cfg.Archival = true

	// 원장 생성
	ledger, err := LoadLedger(log, ledgerName, inMem, protocol.ConsensusCurrentVersion, genBal, genesisID, genesisHash, nil, cfg)
	require.NoError(b, err)

	l := ledger

	cfg.TxPoolSize = 20000
	cfg.EnableProcessBlockStats = false
	// 트랜잭션풀 생성
	tp := pools.MakeTransactionPool(l.Ledger, cfg, logging.Base())
	// 서명된 tx를 저장할 슬라이스 생성
	signedTransactions := make([]transactions.SignedTxn, 0, b.N)

	for i := 0; i < b.N/numUsers; i++ {
		for u := 0; u < numUsers; u++ {
			// generate transactions
			tx := transactions.Transaction{
				// 트랜잭션 타입 지정
				Type: protocol.PaymentTx,
				// 해더정보 입력
				Header: transactions.Header{
					Sender:     addresses[u],
					Fee:        basics.MicroNovas{Raw: proto.MinTxnFee * 2},
					FirstValid: 0,
					LastValid:  basics.Round(proto.MaxTxnLife),
					Note:       make([]byte, 2),
				},
				PaymentTxnFields: transactions.PaymentTxnFields{
					Receiver: addresses[(u+1)%numUsers],
					Amount:   basics.MicroNovas{Raw: mockBalancesMinBalance + (rand.Uint64() % 10000)},
				},
			}
			// 트랜잭션에 서명
			signedTx := tx.Sign(secrets[u])
			// 슬라이스에 서명된 tx 추가
			signedTransactions = append(signedTransactions, signedTx)
		}
	}
	backlogPool := execpool.MakeBacklog(nil, 0, execpool.LowPriority, nil)
	txHandler := MakeTxHandler(tp, l, &mocks.MockNetwork{}, "", crypto.Digest{}, backlogPool)
	b.StartTimer()
	// 서명된 트랜잭션의 크기만큼 반복
	for _, signedTxn := range signedTransactions {
		// 서명된 트랜잭션이 검증 => 캐시저장 => 캐시의 1레벨에 고정됐다면(VerifiedTransactionCache참조) outgoing메시지 리턴
		txHandler.processDecoded([]transactions.SignedTxn{signedTxn})
	}
}

func BenchmarkTimeAfter(b *testing.B) {
	b.StopTimer()
	b.ResetTimer()
	deadline := time.Now().Add(5 * time.Second)
	after := 0
	before := 0
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if time.Now().After(deadline) {
			after++
		} else {
			before++
		}
	}
}
