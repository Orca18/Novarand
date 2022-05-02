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

package verify

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/data/transactions/logic"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/execpool"
	"github.com/Orca18/novarand/util/metrics"
)

// 모든 트랜잭션 실행의 성공, 거절, 에러여부를 나타내는 변수
var logicGoodTotal = metrics.MakeCounter(metrics.MetricName{Name: "algod_ledger_logic_ok", Description: "Total transaction scripts executed and accepted"})
var logicRejTotal = metrics.MakeCounter(metrics.MetricName{Name: "algod_ledger_logic_rej", Description: "Total transaction scripts executed and rejected"})
var logicErrTotal = metrics.MakeCounter(metrics.MetricName{Name: "algod_ledger_logic_err", Description: "Total transaction scripts executed and errored"})

// The PaysetGroups is taking large set of transaction groups and attempt to verify their validity using multiple go-routines.
// When doing so, it attempts to break these into smaller "worksets" where each workset takes about 2ms of execution time in order
// to avoid context switching overhead while providing good validation cancelation responsiveness. Each one of these worksets is
// "populated" with roughly txnPerWorksetThreshold transactions. ( note that the real evaluation time is unknown, but benchmarks
// show that these are realistic numbers )
/*
PaysetGroups은 많은 수의 트랜잭션으로 구성된 그룹이며 여러개의 고루틴(쓰레드 역할)을 사용하여 자신들의 유효성을 검증한다.
이 작업 수행 시 작은 규모의 workset들로 나뉘고 각 workset은 문맥 교환을 피하기 위해 2ms정도의 처리 시간을 부여 받는다.
각 workset은 대략 txnPerWorksetThreshold명 정도로 구성된다.
*/
const txnPerWorksetThreshold = 32

// When the PaysetGroups is generating worksets, it enqueues up to concurrentWorksets entries to the execution pool. This serves several
// purposes :
// - if the verification task need to be aborted, there are only concurrentWorksets entries that are currently redundant on the execution pool queue.
// - that number of concurrent tasks would not get beyond the capacity of the execution pool back buffer.
// - if we were to "redundantly" execute all these during context cancelation, we would spent at most 2ms * 16 = 32ms time.
// - it allows us to linearly scan the input, and process elements only once we're going to queue them into the pool.
/*
PaysetGroup들이 워크셋을 생성할 때 몇개의 워크셋들을 동시에 실행풀의 큐에 집어넣는다.
이는 몇가지 목적을 갖는다.
1. 만약 검증 작업이 중단되어야 한다면 실행풀의 큐에는 오직 현재 불필요한 동시작업자들의 항목만 존재한다.
2. 동시 작업의 수는 실행 풀 백 버퍼의 용량을 초과하지 않는다.
3. 컨텍스트 취소 중에 이 모든 것을 "불필요하게" 실행한다면 최대 2ms * 16 = 32ms 시간을 소비할 것입니다.
4. 이를 통해 입력을 선형으로 스캔하고 오직 풀의 큐에 workset들을 넣을 때만 요소들을 처리할 수 있다.
*/
const concurrentWorksets = 16

// GroupContext is the set of parameters external to a transaction which
// stateless checks are performed against.
//
// For efficient caching, these parameters should either be constant
// or change slowly over time.
//
// Group data are omitted because they are committed to in the
// transaction and its ID.
/*
	GroupContext는 statelss 검사가 수행되는 트랜잭션 외부의 매개 변수 집합이다.
	효율적인 캐싱을 위해 이러한 매개변수는 상수이거나 천천히 변경되어야 한다
	트랜잭션 그룹의 실제 데이터를 트랜잭션 객체와 트랜잭션 아이디에 포함되어있기 때문에 생략한다.
*/
type GroupContext struct {
	// 트랜잭션 수수료를 처리하는 계정의 주소
	specAddrs transactions.SpecialAddresses

	// 합의 버전
	consensusVersion protocol.ConsensusVersion

	// 합의 관련 변수
	consensusParams config.ConsensusParams

	// 최소 TEAL 버전
	minTealVersion uint64

	// 서명된 트랜잭션 그룹(이것들을 검증해야 함!!!!)
	signedGroupTxns []transactions.SignedTxn
}

// PrepareGroupContext prepares a verification group parameter object for a given transaction
// group.
func PrepareGroupContext(group []transactions.SignedTxn, contextHdr bookkeeping.BlockHeader) (*GroupContext, error) {
	if len(group) == 0 {
		return nil, nil
	}
	consensusParams, ok := config.Consensus[contextHdr.CurrentProtocol]
	if !ok {
		return nil, protocol.Error(contextHdr.CurrentProtocol)
	}
	return &GroupContext{
		specAddrs: transactions.SpecialAddresses{
			FeeSink:     contextHdr.FeeSink,
			RewardsPool: contextHdr.RewardsPool,
		},
		consensusVersion: contextHdr.CurrentProtocol,
		consensusParams:  consensusParams,
		minTealVersion:   logic.ComputeMinTealVersion(transactions.WrapSignedTxnsWithAD(group), false),
		signedGroupTxns:  group,
	}, nil
}

// Equal compares two group contexts to see if they would represent the same verification context for a given transaction.
func (g *GroupContext) Equal(other *GroupContext) bool {
	return g.specAddrs == other.specAddrs &&
		g.consensusVersion == other.consensusVersion &&
		g.minTealVersion == other.minTealVersion
}

// Txn verifies a SignedTxn as being signed and having no obviously inconsistent data.
// Block-assembly time checks of LogicSig and accounting rules may still block the txn.
/*
Txn은 서명된 트랜잭션이 올바르게 서명되었고 잘못된 데이터를 가지고 있지 않은지 증명한다.
Block-assembly time은 LogicSig를 검사하고 accounting rules은 트랜잭션을 차단할 수 있다.
*/
func Txn(s *transactions.SignedTxn, txnIdx int, groupCtx *GroupContext) error {
	useBatchVerification := groupCtx.consensusParams.EnableBatchVerification
	batchVerifier := crypto.MakeBatchVerifierDefaultSize(useBatchVerification)
	if err := TxnBatchVerify(s, txnIdx, groupCtx, batchVerifier); err != nil {
		return err
	}

	// this case is used for comapact certificate where no signature is supplied
	if batchVerifier.GetNumberOfEnqueuedSignatures() == 0 {
		return nil
	}
	if err := batchVerifier.Verify(); err != nil {
		return err
	}
	return nil
}

// TxnBatchVerify verifies a SignedTxn having no obviously inconsistent data.
// Block-assembly time checks of LogicSig and accounting rules may still block the txn.
// it is the caller responsibility to call batchVerifier.verify()
func TxnBatchVerify(s *transactions.SignedTxn, txnIdx int, groupCtx *GroupContext, verifier *crypto.BatchVerifier) error {
	if !groupCtx.consensusParams.SupportRekeying && (s.AuthAddr != basics.Address{}) {
		return errors.New("nonempty AuthAddr but rekeying not supported")
	}

	if err := s.Txn.WellFormed(groupCtx.specAddrs, groupCtx.consensusParams); err != nil {
		return err
	}

	return stxnVerifyCore(s, txnIdx, groupCtx, verifier)
}

// TxnGroup verifies a []SignedTxn as being signed and having no obviously inconsistent data.
/*
서명된 트랜잭션 그룹을 검증한다.
*/
func TxnGroup(stxs []transactions.SignedTxn, contextHdr bookkeeping.BlockHeader, cache VerifiedTransactionCache) (groupCtx *GroupContext, err error) {

	currentVersion := contextHdr.CurrentProtocol
	useBatchVerification := config.Consensus[currentVersion].EnableBatchVerification
	batchVerifier := crypto.MakeBatchVerifierDefaultSize(useBatchVerification)

	if groupCtx, err = TxnGroupBatchVerify(stxs, contextHdr, cache, batchVerifier); err != nil {
		return nil, err
	}

	if batchVerifier.GetNumberOfEnqueuedSignatures() == 0 {
		return groupCtx, nil
	}

	if err := batchVerifier.Verify(); err != nil {
		return nil, err
	}

	return
}

// TxnGroupBatchVerify verifies a []SignedTxn having no obviously inconsistent data.
// it is the caller responsibility to call batchVerifier.verify()
/*
	트랜잭션 그룹을 검증한다.
*/
func TxnGroupBatchVerify(stxs []transactions.SignedTxn, contextHdr bookkeeping.BlockHeader, cache VerifiedTransactionCache, verifier *crypto.BatchVerifier) (groupCtx *GroupContext, err error) {
	groupCtx, err = PrepareGroupContext(stxs, contextHdr)
	if err != nil {
		return nil, err
	}

	minFeeCount := uint64(0)
	feesPaid := uint64(0)
	for i, stxn := range stxs {
		err = TxnBatchVerify(&stxn, i, groupCtx, verifier)
		if err != nil {
			err = fmt.Errorf("transaction %+v invalid : %w", stxn, err)
			return
		}
		if stxn.Txn.Type != protocol.CompactCertTx {
			minFeeCount++
		}
		feesPaid = basics.AddSaturate(feesPaid, stxn.Txn.Fee.Raw)
	}
	feeNeeded, overflow := basics.OMul(groupCtx.consensusParams.MinTxnFee, minFeeCount)
	if overflow {
		err = fmt.Errorf("txgroup fee requirement overflow")
		return
	}
	// feesPaid may have saturated. That's ok. Since we know
	// feeNeeded did not overflow, simple comparison tells us
	// feesPaid was enough.
	if feesPaid < feeNeeded {
		err = fmt.Errorf("txgroup had %d in fees, which is less than the minimum %d * %d",
			feesPaid, minFeeCount, groupCtx.consensusParams.MinTxnFee)
		return
	}

	if cache != nil {
		cache.Add(stxs, groupCtx)
	}
	return
}

func stxnVerifyCore(s *transactions.SignedTxn, txnIdx int, groupCtx *GroupContext, batchVerifier *crypto.BatchVerifier) error {
	numSigs := 0
	hasSig := false
	hasMsig := false
	hasLogicSig := false
	if s.Sig != (crypto.Signature{}) {
		numSigs++
		hasSig = true
	}
	if !s.Msig.Blank() {
		numSigs++
		hasMsig = true
	}
	if !s.Lsig.Blank() {
		numSigs++
		hasLogicSig = true
	}
	if numSigs == 0 {
		// Special case: special sender address can issue special transaction
		// types (compact cert txn) without any signature.  The well-formed
		// check ensures that this transaction cannot pay any fee, and
		// cannot have any other interesting fields, except for the compact
		// cert payload.
		if s.Txn.Sender == transactions.CompactCertSender && s.Txn.Type == protocol.CompactCertTx {
			return nil
		}

		return errors.New("signedtxn has no sig")
	}
	if numSigs > 1 {
		return errors.New("signedtxn should only have one of Sig or Msig or LogicSig")
	}

	if hasSig {
		batchVerifier.EnqueueSignature(crypto.SignatureVerifier(s.Authorizer()), s.Txn, s.Sig)
		return nil
	}
	if hasMsig {
		if ok, _ := crypto.MultisigBatchVerify(s.Txn,
			crypto.Digest(s.Authorizer()),
			s.Msig,
			batchVerifier); ok {
			return nil
		}
		return errors.New("multisig validation failed")
	}
	if hasLogicSig {
		return logicSigBatchVerify(s, txnIdx, groupCtx, batchVerifier)
	}
	return errors.New("has one mystery sig. WAT?")
}

// LogicSigSanityCheck checks that the signature is valid and that the program is basically well formed.
// It does not evaluate the logic.
func LogicSigSanityCheck(txn *transactions.SignedTxn, groupIndex int, groupCtx *GroupContext) error {
	useBatchVerification := groupCtx.consensusParams.EnableBatchVerification
	batchVerifier := crypto.MakeBatchVerifierDefaultSize(useBatchVerification)

	if err := LogicSigSanityCheckBatchVerify(txn, groupIndex, groupCtx, batchVerifier); err != nil {
		return err
	}

	// in case of contract account the signature len might 0. that's ok
	if batchVerifier.GetNumberOfEnqueuedSignatures() == 0 {
		return nil
	}

	if err := batchVerifier.Verify(); err != nil {
		return err
	}
	return nil
}

// LogicSigSanityCheckBatchVerify checks that the signature is valid and that the program is basically well formed.
// It does not evaluate the logic.
// it is the caller responsibility to call batchVerifier.verify()
func LogicSigSanityCheckBatchVerify(txn *transactions.SignedTxn, groupIndex int, groupCtx *GroupContext, batchVerifier *crypto.BatchVerifier) error {
	lsig := txn.Lsig

	if groupCtx.consensusParams.LogicSigVersion == 0 {
		return errors.New("LogicSig not enabled")
	}
	if len(lsig.Logic) == 0 {
		return errors.New("LogicSig.Logic empty")
	}
	version, vlen := binary.Uvarint(lsig.Logic)
	if vlen <= 0 {
		return errors.New("LogicSig.Logic bad version")
	}
	if version > groupCtx.consensusParams.LogicSigVersion {
		return errors.New("LogicSig.Logic version too new")
	}
	if uint64(lsig.Len()) > groupCtx.consensusParams.LogicSigMaxSize {
		return errors.New("LogicSig.Logic too long")
	}

	if groupIndex < 0 {
		return errors.New("Negative groupIndex")
	}
	txngroup := transactions.WrapSignedTxnsWithAD(groupCtx.signedGroupTxns)
	ep := logic.EvalParams{
		Proto:          &groupCtx.consensusParams,
		TxnGroup:       txngroup,
		MinTealVersion: &groupCtx.minTealVersion,
	}
	err := logic.CheckSignature(groupIndex, &ep)
	if err != nil {
		return err
	}

	hasMsig := false
	numSigs := 0
	if lsig.Sig != (crypto.Signature{}) {
		numSigs++
	}
	if !lsig.Msig.Blank() {
		hasMsig = true
		numSigs++
	}
	if numSigs == 0 {
		// if the txn.Authorizer() == hash(Logic) then this is a (potentially) valid operation on a contract-only account
		program := logic.Program(lsig.Logic)
		lhash := crypto.HashObj(&program)
		if crypto.Digest(txn.Authorizer()) == lhash {
			return nil
		}
		return errors.New("LogicNot signed and not a Logic-only account")
	}
	if numSigs > 1 {
		return errors.New("LogicSig should only have one of Sig or Msig but has more than one")
	}

	if !hasMsig {
		program := logic.Program(lsig.Logic)
		batchVerifier.EnqueueSignature(crypto.PublicKey(txn.Authorizer()), &program, lsig.Sig)
	} else {
		program := logic.Program(lsig.Logic)
		if ok, _ := crypto.MultisigBatchVerify(&program, crypto.Digest(txn.Authorizer()), lsig.Msig, batchVerifier); !ok {
			return errors.New("logic multisig validation failed")
		}
	}
	return nil
}

// logicSigBatchVerify checks that the signature is valid, executing the program.
// it is the caller responsibility to call batchVerifier.verify()
func logicSigBatchVerify(txn *transactions.SignedTxn, groupIndex int, groupCtx *GroupContext, batchverifier *crypto.BatchVerifier) error {
	err := LogicSigSanityCheck(txn, groupIndex, groupCtx)
	if err != nil {
		return err
	}

	if groupIndex < 0 {
		return errors.New("Negative groupIndex")
	}
	ep := logic.EvalParams{
		Proto:          &groupCtx.consensusParams,
		TxnGroup:       transactions.WrapSignedTxnsWithAD(groupCtx.signedGroupTxns),
		MinTealVersion: &groupCtx.minTealVersion,
	}
	pass, err := logic.EvalSignature(groupIndex, &ep)
	if err != nil {
		logicErrTotal.Inc(nil)
		return fmt.Errorf("transaction %v: rejected by logic err=%v", txn.ID(), err)
	}
	if !pass {
		logicRejTotal.Inc(nil)
		return fmt.Errorf("transaction %v: rejected by logic", txn.ID())
	}
	logicGoodTotal.Inc(nil)
	return nil

}

// PaysetGroups verifies that the payset have a good signature and that the underlying
// transactions are properly constructed.
// Note that this does not check whether a payset is valid against the ledger:
// a PaysetGroups may be well-formed, but a payset might contain an overspend.
//
// This version of verify is performing the verification over the provided execution pool.
/*
	PaysetGroups은 해당 payset이 올바른 서명을 가지고 있고 적절하게 구성된 트랜잭션인지 검증한다.
	이 함수는 원장에 대해 이 payset이 유효한지 검증하지 않는다. 이 payset은 well-formed됐을 것이다(다른 부분에서 검증하나보다!!!)
	따라서 이 검증은 execution pool에 관한것이다!!!!
*/
func PaysetGroups(ctx context.Context, payset [][]transactions.SignedTxn, blkHeader bookkeeping.BlockHeader, verificationPool execpool.BacklogPool, cache VerifiedTransactionCache) (err error) {
	if len(payset) == 0 {
		return nil
	}

	// prepare up to 16 concurrent worksets.
	worksets := make(chan struct{}, concurrentWorksets)
	worksDoneCh := make(chan interface{}, concurrentWorksets)
	processing := 0
	currentVersion := blkHeader.CurrentProtocol
	useBatchVerification := config.Consensus[currentVersion].EnableBatchVerification

	tasksCtx, cancelTasksCtx := context.WithCancel(ctx)
	defer cancelTasksCtx()
	builder := worksetBuilder{payset: payset}
	var nextWorkset [][]transactions.SignedTxn
	for processing >= 0 {
		// see if we need to get another workset
		if len(nextWorkset) == 0 && !builder.completed() {
			// 다음 워크셋(검증할 트랜잭션들)이 존재한다면 nextWorkset에 넣어준다.
			nextWorkset = builder.next()
		}

		select {
		case <-tasksCtx.Done():
			return tasksCtx.Err()
		case worksets <- struct{}{}:
			// 실질적인 검증을 진행하는 부분
			if len(nextWorkset) > 0 {
				// 백로그풀에 넣어준다. => 수행할 어떤 작업을 넣어주는 풀
				// 여기서 하나씩 꺼내서 실제 작업을 수행한다.
				err := verificationPool.EnqueueBacklog(ctx, func(arg interface{}) interface{} {
					var grpErr error
					// check if we've canceled the request while this was in the queue.
					if tasksCtx.Err() != nil {
						return tasksCtx.Err()
					}

					txnGroups := arg.([][]transactions.SignedTxn)
					groupCtxs := make([]*GroupContext, len(txnGroups))

					// 검증할 트랜잭션 그룹(payset)을 넣어준다.
					batchVerifier := crypto.MakeBatchVerifier(len(payset), useBatchVerification)
					for i, signTxnsGrp := range txnGroups {
						groupCtxs[i], grpErr = TxnGroupBatchVerify(signTxnsGrp, blkHeader, nil, batchVerifier)
						// abort only if it's a non-cache error.
						if grpErr != nil {
							return grpErr
						}
					}
					if batchVerifier.GetNumberOfEnqueuedSignatures() != 0 {
						verifyErr := batchVerifier.Verify()
						if verifyErr != nil {
							return verifyErr
						}
					}
					// 검증이 완료되면 cache에 트랜잭션 그룹을 추가한다.
					cache.AddPayset(txnGroups, groupCtxs)
					return nil
				}, nextWorkset, worksDoneCh)
				if err != nil {
					return err
				}
				processing++
				nextWorkset = nil
			}
		case processingResult := <-worksDoneCh:
			processing--
			<-worksets
			// if there is nothing in the queue, the nextWorkset doesn't contain any work and the builder has no more entries, then we're done.
			/*
				queue에 들어있던 모든 작업을 끝내서 큐에 아무것도 들어있지 않다면 종료를 한다.
			*/
			if processing == 0 && builder.completed() && len(nextWorkset) == 0 {
				// we're done.
				processing = -1
			}
			if processingResult != nil {
				err = processingResult.(error)
				if err != nil {
					return err
				}
			}
		}

	}
	return err
}

// worksetBuilder is a helper struct used to construct well sized worksets for the execution pool to process
/*
	worksetBuilder는 실행 풀이 처리할 workset을 구성하는 데 사용되는 도우미 구조체입니다.
	처리할 트랜잭션은 한번에 하나씩 오는게 아니라 많은수가 한꺼번에 들어온다. 이것들을 한꺼번에 처리하지 않고 적절한 수의 워크셋으로 나눠서 각각 처리하게 한다.
*/
type worksetBuilder struct {
	// 검증해야할 서명된 트랜잭션 배열
	payset [][]transactions.SignedTxn
	// 위 배열의 인덱스 - 워크셋을 나눌 때 사용하기 위해 저장한다.
	idx int
}

func (w *worksetBuilder) next() (txnGroups [][]transactions.SignedTxn) {
	// how many transaction we already included in the current workset.
	// scan starting from the current position until we filled up the workset.
	txnCounter := 0
	for i := w.idx; i < len(w.payset); i++ {
		if txnCounter+len(w.payset[i]) > txnPerWorksetThreshold {
			if i == w.idx {
				i++
			}
			txnGroups = w.payset[w.idx:i]
			w.idx = i
			return
		}
		if i == len(w.payset)-1 {
			txnGroups = w.payset[w.idx:]
			w.idx = len(w.payset)
			return
		}
		txnCounter += len(w.payset[i])
	}
	// we can reach here only if w.idx >= len(w.payset). This is not really a usecase, but just
	// for code-completeness, we'll return an empty array here.
	return nil
}

// test to see if we have any more worksets we can extract from our payset.
func (w *worksetBuilder) completed() bool {
	return w.idx >= len(w.payset)
}
