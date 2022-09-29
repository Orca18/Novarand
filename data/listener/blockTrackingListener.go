package listener

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/db"
	"github.com/algorand/go-deadlock"
)

/*
	블록 생성 시 stateDelta객체를 저장하는 객체
*/
type BlockTrackingListener struct {
	ledger *ledger.Ledger

	mu deadlock.Mutex
	// 컨디션 변수
	cond sync.Cond

	log logging.Logger
}

/*
	리스너를 만든다.
*/
func MakeBlockTrackingListener(ledger *ledger.Ledger, log logging.Logger) *BlockTrackingListener {
	listener := BlockTrackingListener{
		ledger: ledger,
		log:    log,
	}
	listener.cond.L = &listener.mu

	return &listener
}

// OnNewBlock excises transactions from the pool that are included in the specified Block or if they've expired
// 새로운 블록이 원장에 저장되면 해당 메소드를 호출 한다 => 원장 저장 => 트랜잭션풀에서 트랜잭션 가져와서 새로운 블록생성의 순서인 것 같다!!
func (listener *BlockTrackingListener) OnNewBlock2(block bookkeeping.Block, delta ledgercore.StateDelta) {
	listener.mu.Lock()

	defer listener.mu.Unlock()
	defer listener.cond.Broadcast()

	l := listener.ledger
	var trackerDb db.Pair
	// db.Pair 잘가져오는가?
	trackerDb = l.GetTrackerDbs()

	// 스테이트델타 정보 db에 저장 잘하는가?
	trackerDb.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {

		// tx 세팅
		result, err := tx.Exec("INSERT INTO statedelta (rnd, accts, hbr, compactCertNext, prevTimestamp, totals) VALUES (?, ?, ?, ?, ?, ?)",
			delta.Hdr.Round,
			protocol.Encode(&delta.Accts),
			protocol.Encode(&block.BlockHeader),
			delta.CompactCertNext,
			delta.PrevTimestamp,
			protocol.Encode(&delta.Totals),
		)

		// 에러 처리
		resultInt, err := result.RowsAffected()

		if err != nil {
			return fmt.Errorf("statedelta db에 데이터 넣기 실패: %w", err)
		} else {
			// db에 데이터 저장 성공 시
			fmt.Println(delta.Hdr.TimeStamp, " [라운드 ", delta.Hdr.Round, "] statedelta db에 데이터 insert ", resultInt, "개 성공!")
		}

		return nil
	})

	/*
		출력해야할 데이터

	*/
}
