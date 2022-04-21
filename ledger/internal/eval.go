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

package internal

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/crypto/compactcert"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/data/transactions/logic"
	"github.com/Orca18/novarand/data/transactions/verify"
	"github.com/Orca18/novarand/ledger/apply"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
	"github.com/Orca18/novarand/util/execpool"
)

// LedgerForCowBase represents subset of Ledger functionality needed for cow business
/*
LedgerForCowBase는 cow(copy on write) 비즈니스에 필요한 Ledger 기능의 하위 집합을 나타냅니다
*/
type LedgerForCowBase interface {
	BlockHdr(basics.Round) (bookkeeping.BlockHeader, error)
	CheckDup(config.ConsensusParams, basics.Round, basics.Round, basics.Round, transactions.Txid, ledgercore.Txlease) error
	LookupWithoutRewards(basics.Round, basics.Address) (basics.AccountData, basics.Round, error)
	GetCreatorForRound(basics.Round, basics.CreatableIndex, basics.CreatableType) (basics.Address, bool, error)
}

// ErrRoundZero is self-explanatory
var ErrRoundZero = errors.New("cannot start evaluator for round 0")

// averageEncodedTxnSizeHint is an estimation for the encoded transaction size
// which is used for preallocating memory upfront in the payset. Preallocating
// helps to avoid re-allocating storage during the evaluation/validation which
// is considerably slower.
const averageEncodedTxnSizeHint = 150

// asyncAccountLoadingThreadCount controls how many go routines would be used
// to load the account data before the Eval() start processing individual
// transaction group.
const asyncAccountLoadingThreadCount = 4

// Creatable represent a single creatable object.
type creatable struct {
	cindex basics.CreatableIndex
	ctype  basics.CreatableType
}

// foundAddress is a wrapper for an address and a boolean.
type foundAddress struct {
	address basics.Address
	exists  bool
}

type roundCowBase struct {
	l LedgerForCowBase

	// The round number of the previous block, for looking up prior state.
	rnd basics.Round

	// TxnCounter from previous block header.
	txnCount uint64

	// Round of the next expected compact cert.  In the common case this
	// is CompactCertNextRound from previous block header, except when
	// compact certs are first enabled, in which case this gets set
	// appropriately at the first block where compact certs are enabled.
	compactCertNextRnd basics.Round

	// The current protocol consensus params.
	proto config.ConsensusParams

	// The accounts that we're already accessed during this round evaluation. This is a caching
	// buffer used to avoid looking up the same account data more than once during a single evaluator
	// execution. The AccountData is always an historical one, then therefore won't be changing.
	// The underlying (accountupdates) infrastucture may provide additional cross-round caching which
	// are beyond the scope of this cache.
	// The account data store here is always the account data without the rewards.
	accounts map[basics.Address]basics.AccountData

	// Similar cache for asset/app creators.
	creators map[creatable]foundAddress
}

func makeRoundCowBase(l LedgerForCowBase, rnd basics.Round, txnCount uint64, compactCertNextRnd basics.Round, proto config.ConsensusParams) *roundCowBase {
	return &roundCowBase{
		l:                  l,
		rnd:                rnd,
		txnCount:           txnCount,
		compactCertNextRnd: compactCertNextRnd,
		proto:              proto,
		accounts:           make(map[basics.Address]basics.AccountData),
		creators:           make(map[creatable]foundAddress),
	}
}

func (x *roundCowBase) getCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	creatable := creatable{cindex: cidx, ctype: ctype}

	if foundAddress, ok := x.creators[creatable]; ok {
		return foundAddress.address, foundAddress.exists, nil
	}

	address, exists, err := x.l.GetCreatorForRound(x.rnd, cidx, ctype)
	if err != nil {
		return basics.Address{}, false, fmt.Errorf(
			"roundCowBase.getCreator() cidx: %d ctype: %v err: %w", cidx, ctype, err)
	}

	x.creators[creatable] = foundAddress{address: address, exists: exists}
	return address, exists, nil
}

// lookup returns the non-rewarded account data for the provided account address. It uses the internal per-round cache
// first, and if it cannot find it there, it would defer to the underlaying implementation.
// note that errors in accounts data retrivals are not cached as these typically cause the transaction evaluation to fail.
func (x *roundCowBase) lookup(addr basics.Address) (basics.AccountData, error) {
	if accountData, found := x.accounts[addr]; found {
		return accountData, nil
	}

	accountData, _, err := x.l.LookupWithoutRewards(x.rnd, addr)
	if err == nil {
		x.accounts[addr] = accountData
	}
	return accountData, err
}

func (x *roundCowBase) checkDup(firstValid, lastValid basics.Round, txid transactions.Txid, txl ledgercore.Txlease) error {
	return x.l.CheckDup(x.proto, x.rnd+1, firstValid, lastValid, txid, txl)
}

func (x *roundCowBase) txnCounter() uint64 {
	return x.txnCount
}

func (x *roundCowBase) compactCertNext() basics.Round {
	return x.compactCertNextRnd
}

func (x *roundCowBase) blockHdr(r basics.Round) (bookkeeping.BlockHeader, error) {
	return x.l.BlockHdr(r)
}

func (x *roundCowBase) allocated(addr basics.Address, aidx basics.AppIndex, global bool) (bool, error) {
	acct, err := x.lookup(addr)
	if err != nil {
		return false, err
	}

	// For global, check if app params exist
	if global {
		_, ok := acct.AppParams[aidx]
		return ok, nil
	}

	// Otherwise, check app local states
	_, ok := acct.AppLocalStates[aidx]
	return ok, nil
}

// getKey gets the value for a particular key in some storage
// associated with an application globally or locally
func (x *roundCowBase) getKey(addr basics.Address, aidx basics.AppIndex, global bool, key string, accountIdx uint64) (basics.TealValue, bool, error) {
	ad, err := x.lookup(addr)
	if err != nil {
		return basics.TealValue{}, false, err
	}

	exist := false
	kv := basics.TealKeyValue{}
	if global {
		var app basics.AppParams
		if app, exist = ad.AppParams[aidx]; exist {
			kv = app.GlobalState
		}
	} else {
		var ls basics.AppLocalState
		if ls, exist = ad.AppLocalStates[aidx]; exist {
			kv = ls.KeyValue
		}
	}
	if !exist {
		err = fmt.Errorf("cannot fetch key, %v", errNoStorage(addr, aidx, global))
		return basics.TealValue{}, false, err
	}

	val, exist := kv[key]
	return val, exist, nil
}

// getStorageCounts counts the storage types used by some account
// associated with an application globally or locally
func (x *roundCowBase) getStorageCounts(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error) {
	ad, err := x.lookup(addr)
	if err != nil {
		return basics.StateSchema{}, err
	}

	count := basics.StateSchema{}
	exist := false
	kv := basics.TealKeyValue{}
	if global {
		var app basics.AppParams
		if app, exist = ad.AppParams[aidx]; exist {
			kv = app.GlobalState
		}
	} else {
		var ls basics.AppLocalState
		if ls, exist = ad.AppLocalStates[aidx]; exist {
			kv = ls.KeyValue
		}
	}
	if !exist {
		return count, nil
	}

	for _, v := range kv {
		if v.Type == basics.TealUintType {
			count.NumUint++
		} else {
			count.NumByteSlice++
		}
	}
	return count, nil
}

func (x *roundCowBase) getStorageLimits(addr basics.Address, aidx basics.AppIndex, global bool) (basics.StateSchema, error) {
	creator, exists, err := x.getCreator(basics.CreatableIndex(aidx), basics.AppCreatable)
	if err != nil {
		return basics.StateSchema{}, err
	}

	// App doesn't exist, so no storage may be allocated.
	if !exists {
		return basics.StateSchema{}, nil
	}

	record, err := x.lookup(creator)
	if err != nil {
		return basics.StateSchema{}, err
	}

	params, ok := record.AppParams[aidx]
	if !ok {
		// This should never happen. If app exists then we should have
		// found the creator successfully.
		err = fmt.Errorf("app %d not found in account %s", aidx, creator.String())
		return basics.StateSchema{}, err
	}

	if global {
		return params.GlobalStateSchema, nil
	}
	return params.LocalStateSchema, nil
}

// wrappers for roundCowState to satisfy the (current) apply.Balances interface
func (cs *roundCowState) Get(addr basics.Address, withPendingRewards bool) (basics.AccountData, error) {
	acct, err := cs.lookup(addr)
	if err != nil {
		return basics.AccountData{}, err
	}
	if withPendingRewards {
		acct = acct.WithUpdatedRewards(cs.proto, cs.rewardsLevel())
	}
	return acct, nil
}

func (cs *roundCowState) GetCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	return cs.getCreator(cidx, ctype)
}

func (cs *roundCowState) Put(addr basics.Address, acct basics.AccountData) error {
	cs.mods.Accts.Upsert(addr, acct)
	return nil
}

func (cs *roundCowState) Move(from basics.Address, to basics.Address, amt basics.MicroAlgos, fromRewards *basics.MicroAlgos, toRewards *basics.MicroAlgos) error {
	rewardlvl := cs.rewardsLevel()

	fromBal, err := cs.lookup(from)
	if err != nil {
		return err
	}
	fromBalNew := fromBal.WithUpdatedRewards(cs.proto, rewardlvl)

	if fromRewards != nil {
		var ot basics.OverflowTracker
		newFromRewards := ot.AddA(*fromRewards, ot.SubA(fromBalNew.MicroAlgos, fromBal.MicroAlgos))
		if ot.Overflowed {
			return fmt.Errorf("overflowed tracking of fromRewards for account %v: %d + (%d - %d)", from, *fromRewards, fromBalNew.MicroAlgos, fromBal.MicroAlgos)
		}
		*fromRewards = newFromRewards
	}

	var overflowed bool
	fromBalNew.MicroAlgos, overflowed = basics.OSubA(fromBalNew.MicroAlgos, amt)
	if overflowed {
		return fmt.Errorf("overspend (account %v, data %+v, tried to spend %v)", from, fromBal, amt)
	}
	cs.Put(from, fromBalNew)

	toBal, err := cs.lookup(to)
	if err != nil {
		return err
	}
	toBalNew := toBal.WithUpdatedRewards(cs.proto, rewardlvl)

	if toRewards != nil {
		var ot basics.OverflowTracker
		newToRewards := ot.AddA(*toRewards, ot.SubA(toBalNew.MicroAlgos, toBal.MicroAlgos))
		if ot.Overflowed {
			return fmt.Errorf("overflowed tracking of toRewards for account %v: %d + (%d - %d)", to, *toRewards, toBalNew.MicroAlgos, toBal.MicroAlgos)
		}
		*toRewards = newToRewards
	}

	toBalNew.MicroAlgos, overflowed = basics.OAddA(toBalNew.MicroAlgos, amt)
	if overflowed {
		return fmt.Errorf("balance overflow (account %v, data %+v, was going to receive %v)", to, toBal, amt)
	}
	cs.Put(to, toBalNew)

	return nil
}

func (cs *roundCowState) ConsensusParams() config.ConsensusParams {
	return cs.proto
}

func (cs *roundCowState) compactCert(certRnd basics.Round, certType protocol.CompactCertType, cert compactcert.Cert, atRound basics.Round, validate bool) error {
	if certType != protocol.CompactCertBasic {
		return fmt.Errorf("compact cert type %d not supported", certType)
	}

	nextCertRnd := cs.compactCertNext()

	certHdr, err := cs.blockHdr(certRnd)
	if err != nil {
		return err
	}

	proto := config.Consensus[certHdr.CurrentProtocol]

	if validate {
		votersRnd := certRnd.SubSaturate(basics.Round(proto.CompactCertRounds))
		votersHdr, err := cs.blockHdr(votersRnd)
		if err != nil {
			return err
		}

		err = validateCompactCert(certHdr, cert, votersHdr, nextCertRnd, atRound)
		if err != nil {
			return err
		}
	}

	cs.setCompactCertNext(certRnd + basics.Round(proto.CompactCertRounds))
	return nil
}

// BlockEvaluator represents an in-progress evaluation of a block
// against the ledger.
/*
	BlockEvaluator는 원장에 등록할 블록을 검증하는데 사용하는 객체이다.
*/
type BlockEvaluator struct {
	// 현재 라운드의 블록 상태인 것 같은데 정확하진 않음.
	state *roundCowState

	// 블록 헤더 검증여부
	validate bool

	// blockHeader.제네시스 해시, blockHeader.rewardState를 생성할건지 여부
	generate bool

	// 이전 블록 해더
	prevHeader  bookkeeping.BlockHeader // cached
	proto       config.ConsensusParams
	genesisHash crypto.Digest

	// 검증할 블록
	block        bookkeeping.Block
	blockTxBytes int
	specials     transactions.SpecialAddresses

	blockGenerated bool // prevent repeated GenerateBlock calls

	// evaluator에게 필요한 ledger인터페이스
	l LedgerForEvaluator

	maxTxnBytesPerBlock int
}

// LedgerForEvaluator defines the ledger interface needed by the evaluator.
type LedgerForEvaluator interface {
	LedgerForCowBase
	GenesisHash() crypto.Digest
	GenesisProto() config.ConsensusParams
	LatestTotals() (basics.Round, ledgercore.AccountTotals, error)
	CompactCertVoters(basics.Round) (*ledgercore.VotersForRound, error)
}

// EvaluatorOptions defines the evaluator creation options
type EvaluatorOptions struct {
	PaysetHint          int
	Validate            bool
	Generate            bool
	MaxTxnBytesPerBlock int
	ProtoParams         *config.ConsensusParams
}

// StartEvaluator creates a BlockEvaluator, given a ledger and a block header
// of the block that the caller is planning to evaluate. If the length of the
// payset being evaluated is known in advance, a paysetHint >= 0 can be
// passed, avoiding unnecessary payset slice growth.
/*
	검증하길 원하는 블록의 블록헤더와 주어진 원장과 대한 BlockEvaluator를 생성한다.
*/
func StartEvaluator(l LedgerForEvaluator, hdr bookkeeping.BlockHeader, evalOpts EvaluatorOptions) (*BlockEvaluator, error) {
	var proto config.ConsensusParams
	if evalOpts.ProtoParams == nil {
		var ok bool
		proto, ok = config.Consensus[hdr.CurrentProtocol]
		if !ok {
			return nil, protocol.Error(hdr.CurrentProtocol)
		}
	} else {
		proto = *evalOpts.ProtoParams
	}

	// if the caller did not provide a valid block size limit, default to the consensus params defaults.
	if evalOpts.MaxTxnBytesPerBlock <= 0 || evalOpts.MaxTxnBytesPerBlock > proto.MaxTxnBytesPerBlock {
		evalOpts.MaxTxnBytesPerBlock = proto.MaxTxnBytesPerBlock
	}

	if hdr.Round == 0 {
		return nil, ErrRoundZero
	}

	prevHeader, err := l.BlockHdr(hdr.Round - 1)
	if err != nil {
		return nil, fmt.Errorf(
			"can't evaluate block %d without previous header: %v", hdr.Round, err)
	}

	prevProto, ok := config.Consensus[prevHeader.CurrentProtocol]
	if !ok {
		return nil, protocol.Error(prevHeader.CurrentProtocol)
	}

	// Round that lookups come from is previous block.  We validate
	// the block at this round below, so underflow will be caught.
	// If we are not validating, we must have previously checked
	// an agreement.Certificate attesting that hdr is valid.
	base := makeRoundCowBase(
		l, hdr.Round-1, prevHeader.TxnCounter, basics.Round(0), proto)

	eval := &BlockEvaluator{
		validate:   evalOpts.Validate,
		generate:   evalOpts.Generate,
		prevHeader: prevHeader,
		block:      bookkeeping.Block{BlockHeader: hdr},
		specials: transactions.SpecialAddresses{
			FeeSink:     hdr.FeeSink,
			RewardsPool: hdr.RewardsPool,
		},
		proto:               proto,
		genesisHash:         l.GenesisHash(),
		l:                   l,
		maxTxnBytesPerBlock: evalOpts.MaxTxnBytesPerBlock,
	}

	// Preallocate space for the payset so that we don't have to
	// dynamically grow a slice (if evaluating a whole block).
	if evalOpts.PaysetHint > 0 {
		maxPaysetHint := evalOpts.MaxTxnBytesPerBlock / averageEncodedTxnSizeHint
		if evalOpts.PaysetHint > maxPaysetHint {
			evalOpts.PaysetHint = maxPaysetHint
		}
		eval.block.Payset = make([]transactions.SignedTxnInBlock, 0, evalOpts.PaysetHint)
	}

	base.compactCertNextRnd = eval.prevHeader.CompactCert[protocol.CompactCertBasic].CompactCertNextRound

	// Check if compact certs are being enabled as of this block.
	if base.compactCertNextRnd == 0 && proto.CompactCertRounds != 0 {
		// Determine the first block that will contain a Merkle
		// commitment to the voters.  We need to account for the
		// fact that the voters come from CompactCertVotersLookback
		// rounds ago.
		votersRound := (hdr.Round + basics.Round(proto.CompactCertVotersLookback)).RoundUpToMultipleOf(basics.Round(proto.CompactCertRounds))

		// The first compact cert will appear CompactCertRounds after that.
		base.compactCertNextRnd = votersRound + basics.Round(proto.CompactCertRounds)
	}

	latestRound, prevTotals, err := l.LatestTotals()
	if err != nil {
		return nil, err
	}
	if latestRound != eval.prevHeader.Round {
		return nil, ledgercore.ErrNonSequentialBlockEval{EvaluatorRound: hdr.Round, LatestRound: latestRound}
	}

	poolAddr := eval.prevHeader.RewardsPool
	// get the reward pool account data without any rewards
	incentivePoolData, _, err := l.LookupWithoutRewards(eval.prevHeader.Round, poolAddr)
	if err != nil {
		return nil, err
	}

	// this is expected to be a no-op, but update the rewards on the rewards pool if it was configured to receive rewards ( unlike mainnet ).
	incentivePoolData = incentivePoolData.WithUpdatedRewards(prevProto, eval.prevHeader.RewardsLevel)

	if evalOpts.Generate {
		if eval.proto.SupportGenesisHash {
			eval.block.BlockHeader.GenesisHash = eval.genesisHash
		}
		eval.block.BlockHeader.RewardsState = eval.prevHeader.NextRewardsState(hdr.Round, proto, incentivePoolData.MicroAlgos, prevTotals.RewardUnits(), logging.Base())
	}
	// set the eval state with the current header
	eval.state = makeRoundCowState(base, eval.block.BlockHeader, proto, eval.prevHeader.TimeStamp, prevTotals, evalOpts.PaysetHint)

	if evalOpts.Validate {
		err := eval.block.BlockHeader.PreCheck(eval.prevHeader)
		if err != nil {
			return nil, err
		}

		// Check that the rewards rate, level and residue match expected values
		expectedRewardsState := eval.prevHeader.NextRewardsState(hdr.Round, proto, incentivePoolData.MicroAlgos, prevTotals.RewardUnits(), logging.Base())
		if eval.block.RewardsState != expectedRewardsState {
			return nil, fmt.Errorf("bad rewards state: %+v != %+v", eval.block.RewardsState, expectedRewardsState)
		}

		// For backwards compatibility: introduce Genesis Hash value
		if eval.proto.SupportGenesisHash && eval.block.BlockHeader.GenesisHash != eval.genesisHash {
			return nil, fmt.Errorf("wrong genesis hash: %s != %s", eval.block.BlockHeader.GenesisHash, eval.genesisHash)
		}
	}

	// Withdraw rewards from the incentive pool
	var ot basics.OverflowTracker
	rewardsPerUnit := ot.Sub(eval.block.BlockHeader.RewardsLevel, eval.prevHeader.RewardsLevel)
	if ot.Overflowed {
		return nil, fmt.Errorf("overflowed subtracting rewards(%d, %d) levels for block %v", eval.block.BlockHeader.RewardsLevel, eval.prevHeader.RewardsLevel, hdr.Round)
	}

	poolOld, err := eval.state.Get(poolAddr, true)
	if err != nil {
		return nil, err
	}

	// hotfix for testnet stall 08/26/2019; move some algos from testnet bank to rewards pool to give it enough time until protocol upgrade occur.
	// hotfix for testnet stall 11/07/2019; the same bug again, account ran out before the protocol upgrade occurred.
	poolOld, err = eval.workaroundOverspentRewards(poolOld, hdr.Round)
	if err != nil {
		return nil, err
	}

	poolNew := poolOld
	poolNew.MicroAlgos = ot.SubA(poolOld.MicroAlgos, basics.MicroAlgos{Raw: ot.Mul(prevTotals.RewardUnits(), rewardsPerUnit)})
	if ot.Overflowed {
		return nil, fmt.Errorf("overflowed subtracting reward unit for block %v", hdr.Round)
	}

	err = eval.state.Put(poolAddr, poolNew)
	if err != nil {
		return nil, err
	}

	// ensure that we have at least MinBalance after withdrawing rewards
	ot.SubA(poolNew.MicroAlgos, basics.MicroAlgos{Raw: proto.MinBalance})
	if ot.Overflowed {
		// TODO this should never happen; should we panic here?
		return nil, fmt.Errorf("overflowed subtracting rewards for block %v", hdr.Round)
	}

	return eval, nil
}

// hotfix for testnet stall 08/26/2019; move some algos from testnet bank to rewards pool to give it enough time until protocol upgrade occur.
// hotfix for testnet stall 11/07/2019; do the same thing
func (eval *BlockEvaluator) workaroundOverspentRewards(rewardPoolBalance basics.AccountData, headerRound basics.Round) (poolOld basics.AccountData, err error) {
	// verify that we patch the correct round.
	if headerRound != 1499995 && headerRound != 2926564 {
		return rewardPoolBalance, nil
	}
	// verify that we're patching the correct genesis ( i.e. testnet )
	testnetGenesisHash, _ := crypto.DigestFromString("JBR3KGFEWPEE5SAQ6IWU6EEBZMHXD4CZU6WCBXWGF57XBZIJHIRA")
	if eval.genesisHash != testnetGenesisHash {
		return rewardPoolBalance, nil
	}

	// get the testnet bank ( dispenser ) account address.
	bankAddr, _ := basics.UnmarshalChecksumAddress("GD64YIY3TWGDMCNPP553DZPPR6LDUSFQOIJVFDPPXWEG3FVOJCCDBBHU5A")
	amount := basics.MicroAlgos{Raw: 20000000000}
	err = eval.state.Move(bankAddr, eval.prevHeader.RewardsPool, amount, nil, nil)
	if err != nil {
		err = fmt.Errorf("unable to move funds from testnet bank to incentive pool: %v", err)
		return
	}
	poolOld, err = eval.state.Get(eval.prevHeader.RewardsPool, true)

	return
}

// PaySetSize returns the number of top-level transactions that have been added to the block evaluator so far.
func (eval *BlockEvaluator) PaySetSize() int {
	return len(eval.block.Payset)
}

// Round returns the round number of the block being evaluated by the BlockEvaluator.
func (eval *BlockEvaluator) Round() basics.Round {
	return eval.block.Round()
}

// ResetTxnBytes resets the number of bytes tracked by the BlockEvaluator to
// zero.  This is a specialized operation used by the transaction pool to
// simulate the effect of putting pending transactions in multiple blocks.
func (eval *BlockEvaluator) ResetTxnBytes() {
	eval.blockTxBytes = 0
}

// TestTransactionGroup performs basic duplicate detection and well-formedness checks
// on a transaction group, but does not actually add the transactions to the block
// evaluator, or modify the block evaluator state in any other visible way.
func (eval *BlockEvaluator) TestTransactionGroup(txgroup []transactions.SignedTxn) error {
	// Nothing to do if there are no transactions.
	if len(txgroup) == 0 {
		return nil
	}

	if len(txgroup) > eval.proto.MaxTxGroupSize {
		return fmt.Errorf("group size %d exceeds maximum %d", len(txgroup), eval.proto.MaxTxGroupSize)
	}

	cow := eval.state.child(len(txgroup))

	var group transactions.TxGroup
	for gi, txn := range txgroup {
		err := eval.TestTransaction(txn, cow)
		if err != nil {
			return err
		}

		// Make sure all transactions in group have the same group value
		if txn.Txn.Group != txgroup[0].Txn.Group {
			return fmt.Errorf("transactionGroup: inconsistent group values: %v != %v",
				txn.Txn.Group, txgroup[0].Txn.Group)
		}

		if !txn.Txn.Group.IsZero() {
			txWithoutGroup := txn.Txn
			txWithoutGroup.Group = crypto.Digest{}

			group.TxGroupHashes = append(group.TxGroupHashes, crypto.HashObj(txWithoutGroup))
		} else if len(txgroup) > 1 {
			return fmt.Errorf("transactionGroup: [%d] had zero Group but was submitted in a group of %d", gi, len(txgroup))
		}
	}

	// If we had a non-zero Group value, check that all group members are present.
	if group.TxGroupHashes != nil {
		if txgroup[0].Txn.Group != crypto.HashObj(group) {
			return fmt.Errorf("transactionGroup: incomplete group: %v != %v (%v)",
				txgroup[0].Txn.Group, crypto.HashObj(group), group)
		}
	}

	return nil
}

// TestTransaction performs basic duplicate detection and well-formedness checks
// on a single transaction, but does not actually add the transaction to the block
// evaluator, or modify the block evaluator state in any other visible way.
func (eval *BlockEvaluator) TestTransaction(txn transactions.SignedTxn, cow *roundCowState) error {
	// Transaction valid (not expired)?
	err := txn.Txn.Alive(eval.block)
	if err != nil {
		return err
	}

	err = txn.Txn.WellFormed(eval.specials, eval.proto)
	if err != nil {
		return fmt.Errorf("transaction %v: malformed: %v", txn.ID(), err)
	}

	// Transaction already in the ledger?
	txid := txn.ID()
	err = cow.checkDup(txn.Txn.First(), txn.Txn.Last(), txid, ledgercore.Txlease{Sender: txn.Txn.Sender, Lease: txn.Txn.Lease})
	if err != nil {
		return err
	}

	return nil
}

// Transaction tentatively adds a new transaction as part of this block evaluation.
// If the transaction cannot be added to the block without violating some constraints,
// an error is returned and the block evaluator state is unchanged.
func (eval *BlockEvaluator) Transaction(txn transactions.SignedTxn, ad transactions.ApplyData) error {
	return eval.TransactionGroup([]transactions.SignedTxnWithAD{
		{
			SignedTxn: txn,
			ApplyData: ad,
		},
	})
}

// TransactionGroup tentatively adds a new transaction group as part of this block evaluation.
// If the transaction group cannot be added to the block without violating some constraints,
// an error is returned and the block evaluator state is unchanged.
func (eval *BlockEvaluator) TransactionGroup(txads []transactions.SignedTxnWithAD) error {
	return eval.transactionGroup(txads)
}

// transactionGroup tentatively executes a group of transactions as part of this block evaluation.
// If the transaction group cannot be added to the block without violating some constraints,
// an error is returned and the block evaluator state is unchanged.
func (eval *BlockEvaluator) transactionGroup(txgroup []transactions.SignedTxnWithAD) error {
	// Nothing to do if there are no transactions.
	if len(txgroup) == 0 {
		return nil
	}

	if len(txgroup) > eval.proto.MaxTxGroupSize {
		return fmt.Errorf("group size %d exceeds maximum %d", len(txgroup), eval.proto.MaxTxGroupSize)
	}

	// 블록안에 있는 서명된 트랜잭션
	var txibs []transactions.SignedTxnInBlock
	// 트랜잭션 그룹에 포함된 트랜잭션들의 해시값 배열
	var group transactions.TxGroup
	var groupTxBytes int

	cow := eval.state.child(len(txgroup))
	evalParams := logic.NewEvalParams(txgroup, &eval.proto, &eval.specials)

	// Evaluate each transaction in the group
	txibs = make([]transactions.SignedTxnInBlock, 0, len(txgroup))
	for gi, txad := range txgroup {
		var txib transactions.SignedTxnInBlock

		err := eval.transaction(txad.SignedTxn, evalParams, gi, txad.ApplyData, cow, &txib)
		if err != nil {
			return err
		}

		txibs = append(txibs, txib)

		if eval.validate {
			groupTxBytes += txib.GetEncodedLength()
			if eval.blockTxBytes+groupTxBytes > eval.maxTxnBytesPerBlock {
				return ledgercore.ErrNoSpace
			}
		}

		// Make sure all transactions in group have the same group value
		if txad.SignedTxn.Txn.Group != txgroup[0].SignedTxn.Txn.Group {
			return fmt.Errorf("transactionGroup: inconsistent group values: %v != %v",
				txad.SignedTxn.Txn.Group, txgroup[0].SignedTxn.Txn.Group)
		}

		if !txad.SignedTxn.Txn.Group.IsZero() {
			txWithoutGroup := txad.SignedTxn.Txn
			txWithoutGroup.Group = crypto.Digest{}

			group.TxGroupHashes = append(group.TxGroupHashes, crypto.HashObj(txWithoutGroup))
		} else if len(txgroup) > 1 {
			return fmt.Errorf("transactionGroup: [%d] had zero Group but was submitted in a group of %d", gi, len(txgroup))
		}
	}

	// If we had a non-zero Group value, check that all group members are present.
	if group.TxGroupHashes != nil {
		if txgroup[0].SignedTxn.Txn.Group != crypto.HashObj(group) {
			return fmt.Errorf("transactionGroup: incomplete group: %v != %v (%v)",
				txgroup[0].SignedTxn.Txn.Group, crypto.HashObj(group), group)
		}
	}

	eval.block.Payset = append(eval.block.Payset, txibs...)
	eval.blockTxBytes += groupTxBytes
	cow.commitToParent()

	return nil
}

// Check the minimum balance requirement for the modified accounts in `cow`.
func (eval *BlockEvaluator) checkMinBalance(cow *roundCowState) error {
	rewardlvl := cow.rewardsLevel()
	for _, addr := range cow.modifiedAccounts() {
		// Skip FeeSink, RewardsPool, and CompactCertSender MinBalance checks here.
		// There's only a few accounts, so space isn't an issue, and we don't
		// expect them to have low balances, but if they do, it may cause
		// surprises.
		if addr == eval.block.FeeSink || addr == eval.block.RewardsPool ||
			addr == transactions.CompactCertSender {
			continue
		}

		data, err := cow.lookup(addr)
		if err != nil {
			return err
		}

		// It's always OK to have the account move to an empty state,
		// because the accounts DB can delete it.  Otherwise, we will
		// enforce MinBalance.
		if data.IsZero() {
			continue
		}

		dataNew := data.WithUpdatedRewards(eval.proto, rewardlvl)
		effectiveMinBalance := dataNew.MinBalance(&eval.proto)
		if dataNew.MicroAlgos.Raw < effectiveMinBalance.Raw {
			return fmt.Errorf("account %v balance %d below min %d (%d assets)",
				addr, dataNew.MicroAlgos.Raw, effectiveMinBalance.Raw, len(dataNew.Assets))
		}

		// Check if we have exceeded the maximum minimum balance
		if eval.proto.MaximumMinimumBalance != 0 {
			if effectiveMinBalance.Raw > eval.proto.MaximumMinimumBalance {
				return fmt.Errorf("account %v would use too much space after this transaction. Minimum balance requirements would be %d (greater than max %d)", addr, effectiveMinBalance.Raw, eval.proto.MaximumMinimumBalance)
			}
		}
	}

	return nil
}

// transaction tentatively executes a new transaction as part of this block evaluation.
// If the transaction cannot be added to the block without violating some constraints,
// an error is returned and the block evaluator state is unchanged.
/*
	불록평가의 한 부분으로써 새로운 트랜잭션을 실행한다.
*/
func (eval *BlockEvaluator) transaction(txn transactions.SignedTxn, evalParams *logic.EvalParams, gi int, ad transactions.ApplyData, cow *roundCowState, txib *transactions.SignedTxnInBlock) error {
	var err error

	// Only compute the TxID once
	txid := txn.ID()

	// AddBlock()에서 validate는 false이므로 아래코드 실행 안함
	if eval.validate {
		err = txn.Txn.Alive(eval.block)
		if err != nil {
			return err
		}

		// Transaction already in the ledger?
		err := cow.checkDup(txn.Txn.First(), txn.Txn.Last(), txid, ledgercore.Txlease{Sender: txn.Txn.Sender, Lease: txn.Txn.Lease})
		if err != nil {
			return err
		}

		// Does the address that authorized the transaction actually match whatever address the sender has rekeyed to?
		// i.e., the sig/lsig/msig was checked against the txn.Authorizer() address, but does this match the sender's balrecord.AuthAddr?
		acctdata, err := cow.lookup(txn.Txn.Sender)
		if err != nil {
			return err
		}
		correctAuthorizer := acctdata.AuthAddr
		if (correctAuthorizer == basics.Address{}) {
			correctAuthorizer = txn.Txn.Sender
		}
		if txn.Authorizer() != correctAuthorizer {
			return fmt.Errorf("transaction %v: should have been authorized by %v but was actually authorized by %v", txn.ID(), correctAuthorizer, txn.Authorizer())
		}
	}

	// Apply the transaction, updating the cow balances
	applyData, err := eval.applyTransaction(txn.Txn, cow, evalParams, gi, cow.txnCounter())
	if err != nil {
		return fmt.Errorf("transaction %v: %v", txid, err)
	}

	// Validate applyData if we are validating an existing block.
	// If we are validating and generating, we have no ApplyData yet.
	if eval.validate && !eval.generate {
		if eval.proto.ApplyData {
			if !ad.Equal(applyData) {
				return fmt.Errorf("transaction %v: applyData mismatch: %v != %v", txid, ad, applyData)
			}
		} else {
			if !ad.Equal(transactions.ApplyData{}) {
				return fmt.Errorf("transaction %v: applyData not supported", txid)
			}
		}
	}

	// Check if the transaction fits in the block, now that we can encode it.
	*txib, err = eval.block.EncodeSignedTxn(txn, applyData)
	if err != nil {
		return err
	}

	// Check if any affected accounts dipped below MinBalance (unless they are
	// completely zero, which means the account will be deleted.)
	// Only do those checks if we are validating or generating. It is useful to skip them
	// if we cannot provide account data that contains enough information to
	// compute the correct minimum balance (the case with indexer which does not store it).
	if eval.validate || eval.generate {
		err := eval.checkMinBalance(cow)
		if err != nil {
			return fmt.Errorf("transaction %v: %w", txid, err)
		}
	}

	// Remember this txn
	cow.addTx(txn.Txn, txid)

	return nil
}

// applyTransaction changes the balances according to this transaction.
/*
	트랜잭션을 실행한다.
*/
func (eval *BlockEvaluator) applyTransaction(tx transactions.Transaction, balances *roundCowState, evalParams *logic.EvalParams, gi int, ctr uint64) (ad transactions.ApplyData, err error) {
	params := balances.ConsensusParams()

	// move fee to pool
	/*
		수수료를 FeeSink계정에게 전달한다.
	*/
	err = balances.Move(tx.Sender, eval.specials.FeeSink, tx.Fee, &ad.SenderRewards, nil)
	if err != nil {
		return
	}

	err = apply.Rekey(balances, &tx)
	if err != nil {
		return
	}

	switch tx.Type {
	case protocol.PaymentTx:
		err = apply.Payment(tx.PaymentTxnFields, tx.Header, balances, eval.specials, &ad)

	case protocol.KeyRegistrationTx:
		err = apply.Keyreg(tx.KeyregTxnFields, tx.Header, balances, eval.specials, &ad, balances.round())

	case protocol.AssetConfigTx:
		err = apply.AssetConfig(tx.AssetConfigTxnFields, tx.Header, balances, eval.specials, &ad, ctr)

	case protocol.AssetTransferTx:
		err = apply.AssetTransfer(tx.AssetTransferTxnFields, tx.Header, balances, eval.specials, &ad)

	case protocol.AssetFreezeTx:
		err = apply.AssetFreeze(tx.AssetFreezeTxnFields, tx.Header, balances, eval.specials, &ad)

	case protocol.ApplicationCallTx:
		err = apply.ApplicationCall(tx.ApplicationCallTxnFields, tx.Header, balances, &ad, gi, evalParams, ctr)

	case protocol.CompactCertTx:
		// in case of a CompactCertTx transaction, we want to "apply" it only in validate or generate mode. This will deviate the cow's CompactCertNext depending of
		// whether we're in validate/generate mode or not, however - given that this variable in only being used in these modes, it would be safe.
		// The reason for making this into an exception is that during initialization time, the accounts update is "converting" the recent 320 blocks into deltas to
		// be stored in memory. These deltas don't care about the compact certificate, and so we can improve the node load time. Additionally, it save us from
		// performing the validation during catchup, which is another performance boost.
		if eval.validate || eval.generate {
			err = balances.compactCert(tx.CertRound, tx.CertType, tx.Cert, tx.Header.FirstValid, eval.validate)
		}

	default:
		err = fmt.Errorf("unknown transaction type %v", tx.Type)
	}

	// Record first, so that details can all be used in logic evaluation, even
	// if cleared below. For example, `gaid`, introduced in v28 is now
	// implemented in terms of the AD fields introduced in v30.
	evalParams.RecordAD(gi, ad)

	// If the protocol does not support rewards in ApplyData,
	// clear them out.
	if !params.RewardsInApplyData {
		ad.SenderRewards = basics.MicroAlgos{}
		ad.ReceiverRewards = basics.MicroAlgos{}
		ad.CloseRewards = basics.MicroAlgos{}
	}

	// No separate config for activating these AD fields because inner
	// transactions require their presence, so the consensus update to add
	// inners also stores these IDs.
	if params.MaxInnerTransactions == 0 {
		ad.ApplicationID = 0
		ad.ConfigAsset = 0
	}

	return
}

// compactCertVotersAndTotal returns the expected values of CompactCertVoters
// and CompactCertVotersTotal for a block.
func (eval *BlockEvaluator) compactCertVotersAndTotal() (root crypto.GenericDigest, total basics.MicroAlgos, err error) {
	if eval.proto.CompactCertRounds == 0 {
		return
	}

	if eval.block.Round()%basics.Round(eval.proto.CompactCertRounds) != 0 {
		return
	}

	lookback := eval.block.Round().SubSaturate(basics.Round(eval.proto.CompactCertVotersLookback))
	voters, err := eval.l.CompactCertVoters(lookback)
	if err != nil {
		return
	}

	if voters != nil {
		root, total = voters.Tree.Root(), voters.TotalWeight
	}

	return
}

// TestingTxnCounter - the method returns the current evaluator transaction counter. The method is used for testing purposes only.
func (eval *BlockEvaluator) TestingTxnCounter() uint64 {
	return eval.state.txnCounter()
}

// Call "endOfBlock" after all the block's rewards and transactions are processed.
/*
	모든 트랜잭션이 처리되고 블록에 대한 보상을 책정했다면(?) 이 메소드를 호출한다.
*/
func (eval *BlockEvaluator) endOfBlock() error {
	if eval.generate {
		var err error
		// 블록의 커밋된 payset에 대한 해시값 반환
		eval.block.TxnRoot, err = eval.block.PaysetCommit()
		if err != nil {
			return err
		}

		// 원장에 커밋된 트랜잭션의 수를 저장
		if eval.proto.TxnCounter {
			eval.block.TxnCounter = eval.state.txnCounter()
		} else {
			eval.block.TxnCounter = 0
		}

		// 종료된 OnlineAccount를 생성한다.
		eval.generateExpiredOnlineAccountsList()

		if eval.proto.CompactCertRounds > 0 {
			var basicCompactCert bookkeeping.CompactCertState
			basicCompactCert.CompactCertVoters, basicCompactCert.CompactCertVotersTotal, err = eval.compactCertVotersAndTotal()
			if err != nil {
				return err
			}

			basicCompactCert.CompactCertNextRound = eval.state.compactCertNext()

			eval.block.CompactCert = make(map[protocol.CompactCertType]bookkeeping.CompactCertState)
			eval.block.CompactCert[protocol.CompactCertBasic] = basicCompactCert
		}
	}

	err := eval.validateExpiredOnlineAccounts()
	if err != nil {
		return err
	}

	err = eval.resetExpiredOnlineAccountsParticipationKeys()
	if err != nil {
		return err
	}

	eval.state.mods.OptimizeAllocatedMemory(eval.proto)

	if eval.validate {
		// check commitments
		txnRoot, err := eval.block.PaysetCommit()
		if err != nil {
			return err
		}
		if txnRoot != eval.block.TxnRoot {
			return fmt.Errorf("txn root wrong: %v != %v", txnRoot, eval.block.TxnRoot)
		}

		var expectedTxnCount uint64
		if eval.proto.TxnCounter {
			expectedTxnCount = eval.state.txnCounter()
		}
		if eval.block.TxnCounter != expectedTxnCount {
			return fmt.Errorf("txn count wrong: %d != %d", eval.block.TxnCounter, expectedTxnCount)
		}

		expectedVoters, expectedVotersWeight, err := eval.compactCertVotersAndTotal()
		if err != nil {
			return err
		}
		if !eval.block.CompactCert[protocol.CompactCertBasic].CompactCertVoters.IsEqual(expectedVoters) {
			return fmt.Errorf("CompactCertVoters wrong: %v != %v", eval.block.CompactCert[protocol.CompactCertBasic].CompactCertVoters, expectedVoters)
		}
		if eval.block.CompactCert[protocol.CompactCertBasic].CompactCertVotersTotal != expectedVotersWeight {
			return fmt.Errorf("CompactCertVotersTotal wrong: %v != %v", eval.block.CompactCert[protocol.CompactCertBasic].CompactCertVotersTotal, expectedVotersWeight)
		}
		if eval.block.CompactCert[protocol.CompactCertBasic].CompactCertNextRound != eval.state.compactCertNext() {
			return fmt.Errorf("CompactCertNextRound wrong: %v != %v", eval.block.CompactCert[protocol.CompactCertBasic].CompactCertNextRound, eval.state.compactCertNext())
		}
		for ccType := range eval.block.CompactCert {
			if ccType != protocol.CompactCertBasic {
				return fmt.Errorf("CompactCertType %d unexpected", ccType)
			}
		}
	}

	err = eval.state.CalculateTotals()
	if err != nil {
		return err
	}

	return nil
}

// generateExpiredOnlineAccountsList creates the list of the expired participation accounts by traversing over the
// modified accounts in the state deltas and testing if any of them needs to be reset.
func (eval *BlockEvaluator) generateExpiredOnlineAccountsList() {
	if !eval.generate {
		return
	}
	// We are going to find the list of modified accounts and the
	// current round that is being evaluated.
	// Then we are going to go through each modified account and
	// see if it meets the criteria for adding it to the expired
	// participation accounts list.
	modifiedAccounts := eval.state.mods.Accts.ModifiedAccounts()
	currentRound := eval.Round()

	expectedMaxNumberOfExpiredAccounts := eval.proto.MaxProposedExpiredOnlineAccounts

	for i := 0; i < len(modifiedAccounts) && len(eval.block.ParticipationUpdates.ExpiredParticipationAccounts) < expectedMaxNumberOfExpiredAccounts; i++ {
		accountAddr := modifiedAccounts[i]
		acctDelta, found := eval.state.mods.Accts.Get(accountAddr)
		if !found {
			continue
		}

		// true if the account is online
		isOnline := acctDelta.Status == basics.Online
		// true if the accounts last valid round has passed
		pastCurrentRound := acctDelta.VoteLastValid < currentRound

		if isOnline && pastCurrentRound {
			eval.block.ParticipationUpdates.ExpiredParticipationAccounts = append(
				eval.block.ParticipationUpdates.ExpiredParticipationAccounts,
				accountAddr,
			)
		}
	}
}

// validateExpiredOnlineAccounts tests the expired online accounts specified in ExpiredParticipationAccounts, and verify
// that they have all expired and need to be reset.
func (eval *BlockEvaluator) validateExpiredOnlineAccounts() error {
	if !eval.validate {
		return nil
	}
	expectedMaxNumberOfExpiredAccounts := eval.proto.MaxProposedExpiredOnlineAccounts
	lengthOfExpiredParticipationAccounts := len(eval.block.ParticipationUpdates.ExpiredParticipationAccounts)

	// If the length of the array is strictly greater than our max then we have an error.
	// This works when the expected number of accounts is zero (i.e. it is disabled) as well
	if lengthOfExpiredParticipationAccounts > expectedMaxNumberOfExpiredAccounts {
		return fmt.Errorf("length of expired accounts (%d) was greater than expected (%d)",
			lengthOfExpiredParticipationAccounts, expectedMaxNumberOfExpiredAccounts)
	}

	// For security reasons, we need to make sure that all addresses in the expired participation accounts
	// are unique.  We make this map to keep track of previously seen address
	addressSet := make(map[basics.Address]bool, lengthOfExpiredParticipationAccounts)

	// Validate that all expired accounts meet the current criteria
	currentRound := eval.Round()
	for _, accountAddr := range eval.block.ParticipationUpdates.ExpiredParticipationAccounts {

		if _, exists := addressSet[accountAddr]; exists {
			// We shouldn't have duplicate addresses...
			return fmt.Errorf("duplicate address found: %v", accountAddr)
		}

		// Record that we have seen this address
		addressSet[accountAddr] = true

		acctData, err := eval.state.lookup(accountAddr)
		if err != nil {
			return fmt.Errorf("endOfBlock was unable to retrieve account %v : %w", accountAddr, err)
		}

		// true if the account is online
		isOnline := acctData.Status == basics.Online
		// true if the accounts last valid round has passed
		pastCurrentRound := acctData.VoteLastValid < currentRound

		if !isOnline {
			return fmt.Errorf("endOfBlock found %v was not online but %v", accountAddr, acctData.Status)
		}

		if !pastCurrentRound {
			return fmt.Errorf("endOfBlock found %v round (%d) was not less than current round (%d)", accountAddr, acctData.VoteLastValid, currentRound)
		}
	}
	return nil
}

// resetExpiredOnlineAccountsParticipationKeys after all transactions and rewards are processed, modify the accounts so that their status is offline
func (eval *BlockEvaluator) resetExpiredOnlineAccountsParticipationKeys() error {
	expectedMaxNumberOfExpiredAccounts := eval.proto.MaxProposedExpiredOnlineAccounts
	lengthOfExpiredParticipationAccounts := len(eval.block.ParticipationUpdates.ExpiredParticipationAccounts)

	// If the length of the array is strictly greater than our max then we have an error.
	// This works when the expected number of accounts is zero (i.e. it is disabled) as well
	if lengthOfExpiredParticipationAccounts > expectedMaxNumberOfExpiredAccounts {
		return fmt.Errorf("length of expired accounts (%d) was greater than expected (%d)",
			lengthOfExpiredParticipationAccounts, expectedMaxNumberOfExpiredAccounts)
	}

	for _, accountAddr := range eval.block.ParticipationUpdates.ExpiredParticipationAccounts {
		acctData, err := eval.state.lookup(accountAddr)
		if err != nil {
			return fmt.Errorf("resetExpiredOnlineAccountsParticipationKeys was unable to retrieve account %v : %w", accountAddr, err)
		}

		// Reset the appropriate account data
		acctData.ClearOnlineState()

		// Update the account information
		err = eval.state.Put(accountAddr, acctData)
		if err != nil {
			return err
		}
	}
	return nil
}

// GenerateBlock produces a complete block from the BlockEvaluator.
// This is used during proposal to get an actual block that will be proposed, after
// feeding in tentative transactions into this block evaluator.
//
// After a call to GenerateBlock, the BlockEvaluator can still be used to
// accept transactions.  However, to guard against reuse, subsequent calls
// to GenerateBlock on the same BlockEvaluator will fail.
/*
GenerateBlock은 BlockEvaluator에서 완전한 블록을 생성합니다.
이것은 proposal될 실제 블록을 얻기위해 proposal 과정에서 사용된다.
GenerateBlock을 호출한 후에도 BlockEvaluator를 사용하여 트랜잭션을 수락할 수 있습니다.
그러나 재사용을 방지하기 위해 동일한 BlockEvaluator에서 GenerateBlock은 한번만 호출할 수 있다.
*/
func (eval *BlockEvaluator) GenerateBlock() (*ledgercore.ValidatedBlock, error) {
	if !eval.generate {
		logging.Base().Panicf("GenerateBlock() called but generate is false")
	}

	if eval.blockGenerated {
		return nil, fmt.Errorf("GenerateBlock already called on this BlockEvaluator")
	}

	err := eval.endOfBlock()
	if err != nil {
		return nil, err
	}

	vb := ledgercore.MakeValidatedBlock(eval.block, eval.state.deltas())
	eval.blockGenerated = true
	proto, ok := config.Consensus[eval.block.BlockHeader.CurrentProtocol]
	if !ok {
		return nil, fmt.Errorf(
			"unknown consensus version: %s", eval.block.BlockHeader.CurrentProtocol)
	}
	eval.state = makeRoundCowState(
		eval.state, eval.block.BlockHeader, proto, eval.prevHeader.TimeStamp, eval.state.mods.Totals,
		len(eval.block.Payset))
	return &vb, nil
}

/*
	트랜잭션 검증자
*/
type evalTxValidator struct {
	txcache          verify.VerifiedTransactionCache
	block            bookkeeping.Block
	verificationPool execpool.BacklogPool

	ctx      context.Context
	txgroups [][]transactions.SignedTxnWithAD
	done     chan error
}

func (validator *evalTxValidator) run() {
	defer close(validator.done)
	specialAddresses := transactions.SpecialAddresses{
		FeeSink:     validator.block.BlockHeader.FeeSink,
		RewardsPool: validator.block.BlockHeader.RewardsPool,
	}

	var unverifiedTxnGroups [][]transactions.SignedTxn
	unverifiedTxnGroups = make([][]transactions.SignedTxn, 0, len(validator.txgroups))
	for _, group := range validator.txgroups {
		signedTxnGroup := make([]transactions.SignedTxn, len(group))
		for j, txn := range group {
			signedTxnGroup[j] = txn.SignedTxn
			err := txn.SignedTxn.Txn.Alive(validator.block)
			if err != nil {
				validator.done <- err
				return
			}
		}
		unverifiedTxnGroups = append(unverifiedTxnGroups, signedTxnGroup)
	}

	// 만약 검증되지 않은 트랜잭션이 있다면 가져온다.
	unverifiedTxnGroups = validator.txcache.GetUnverifiedTranscationGroups(unverifiedTxnGroups, specialAddresses, validator.block.BlockHeader.CurrentProtocol)

	err := verify.PaysetGroups(validator.ctx, unverifiedTxnGroups, validator.block.BlockHeader, validator.verificationPool, validator.txcache)
	if err != nil {
		validator.done <- err
	}
}

// Eval is the main evaluator entrypoint (in addition to StartEvaluator)
// used by Ledger.Validate() Ledger.AddBlock() Ledger.trackerEvalVerified()(accountUpdates.loadFromDisk())
//
// Validate: Eval(ctx, l, blk, true, txcache, executionPool)
// AddBlock: Eval(context.Background(), l, blk, false, txcache, nil)
// tracker:  Eval(context.Background(), l, blk, false, txcache, nil)
/*
	evaluator의 메인 진입점이다.
*/
func Eval(ctx context.Context, l LedgerForEvaluator, blk bookkeeping.Block, validate bool, txcache verify.VerifiedTransactionCache, executionPool execpool.BacklogPool) (ledgercore.StateDelta, error) {
	eval, err := StartEvaluator(l, blk.BlockHeader,
		EvaluatorOptions{
			PaysetHint: len(blk.Payset),
			Validate:   validate,
			Generate:   false,
		})
	if err != nil {
		return ledgercore.StateDelta{}, err
	}

	validationCtx, validationCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		validationCancel()
		wg.Wait()
	}()

	// Next, transactions
	/*
		블록안에 있는 서명된 트랜잭션 그룹정보를 가져온다.
	*/
	paysetgroups, err := blk.DecodePaysetGroups()
	if err != nil {
		return ledgercore.StateDelta{}, err
	}

	accountLoadingCtx, accountLoadingCancel := context.WithCancel(ctx)
	paysetgroupsCh := loadAccounts(accountLoadingCtx, l, blk.Round()-1, paysetgroups, blk.BlockHeader.FeeSink, blk.ConsensusProtocol())
	// ensure that before we exit from this method, the account loading is no longer active.
	defer func() {
		accountLoadingCancel()
		// wait for the paysetgroupsCh to get closed.
		for range paysetgroupsCh {
		}
	}()

	// 트랜잭션풀에 아직 검증되지 않은 트랜잭션이 있는지 확인해서 검증해준다.
	// AddBlock()메소드를 호출할 땐 validate가 false여서 아래작업을 수행하지 않는다!!
	// 왜냐하면 이미 검증된 트랜잭션을 가지고 있기 때문에!!
	var txvalidator evalTxValidator
	if validate {
		_, ok := config.Consensus[blk.CurrentProtocol]
		if !ok {
			return ledgercore.StateDelta{}, protocol.Error(blk.CurrentProtocol)
		}
		txvalidator.txcache = txcache
		txvalidator.block = blk
		txvalidator.verificationPool = executionPool

		txvalidator.ctx = validationCtx
		txvalidator.txgroups = paysetgroups
		txvalidator.done = make(chan error, 1)
		go txvalidator.run()

	}

	base := eval.state.lookupParent.(*roundCowBase)

	// 트랜잭션 그룹의 수만큼 반복한다!!!!
transactionGroupLoop:
	for {
		select {
		// 실질적인 작업을 수행하는 부분!
		case txgroup, ok := <-paysetgroupsCh:
			if !ok {
				break transactionGroupLoop
			} else if txgroup.err != nil {
				return ledgercore.StateDelta{}, txgroup.err
			}

			// roundCowBase의 accounts배열에 각 트랜잭션과 관련된 계정의 주소 및 데이터를 저장한다.
			for _, br := range txgroup.balances {
				base.accounts[br.Addr] = br.AccountData
			}
			err = eval.TransactionGroup(txgroup.group)
			if err != nil {
				return ledgercore.StateDelta{}, err
			}
		case <-ctx.Done():
			return ledgercore.StateDelta{}, ctx.Err()
		case err, open := <-txvalidator.done:
			// if we're not validating, then `txvalidator.done` would be nil, in which case this case statement would never be executed.
			if open && err != nil {
				return ledgercore.StateDelta{}, err
			}
		}
	}

	// Finally, process any pending end-of-block state changes.
	err = eval.endOfBlock()
	if err != nil {
		return ledgercore.StateDelta{}, err
	}

	// If validating, do final block checks that depend on our new state
	if validate {
		// wait for the signature validation to complete.
		select {
		case <-ctx.Done():
			return ledgercore.StateDelta{}, ctx.Err()
		case err, open := <-txvalidator.done:
			if !open {
				break
			}
			if err != nil {
				return ledgercore.StateDelta{}, err
			}
		}
	}

	return eval.state.deltas(), nil
}

// loadedTransactionGroup is a helper struct to allow asynchronous loading of the account data needed by the transaction groups
/*
	transaction groups이 필요한 계정정보들을 비동기적으로 가져올 수 있게 해주는 구조체이다.
*/
type loadedTransactionGroup struct {
	// group is the transaction group
	group []transactions.SignedTxnWithAD
	// balances is a list of all the balances that the transaction group refer to and are needed.
	balances []basics.BalanceRecord
	// err indicates whether any of the balances in this structure have failed to load. In case of an error, at least
	// one of the entries in the balances would be uninitialized.
	err error
}

// Return the maximum number of addresses referenced in any given transaction.
func maxAddressesInTxn(proto *config.ConsensusParams) int {
	return 7 + proto.MaxAppTxnAccounts
}

// loadAccounts loads the account data for the provided transaction group list. It also loads the feeSink account and add it to the first returned transaction group.
// The order of the transaction groups returned by the channel is identical to the one in the input array.
/*
	제공된 트랜잭션 그룹에 대한 계정정보를 가져온다(트랜잭션을 발생시킨 계정정보를 가져오는건가?).
	feeSink계정또한 가져와 첫번째 그룹으로 반환한다.
	채널에 의해 반환된 트랜잭션 그룹의 순서는 배열에 그대로 반영된다.
*/
func loadAccounts(ctx context.Context, l LedgerForEvaluator, rnd basics.Round, groups [][]transactions.SignedTxnWithAD, feeSinkAddr basics.Address, consensusParams config.ConsensusParams) chan loadedTransactionGroup {
	outChan := make(chan loadedTransactionGroup, len(groups))
	go func() {
		// groupTask helps to organize the account loading for each transaction group.
		type groupTask struct {
			// balances contains the loaded balances each transaction group have
			balances []basics.BalanceRecord
			// balancesCount is the number of balances that nees to be loaded per transaction group
			balancesCount int
			// done is a waiting channel for all the account data for the transaction group to be loaded
			done chan error
		}
		// addrTask manage the loading of a single account address.
		type addrTask struct {
			// account address to fetch
			address basics.Address
			// a list of transaction group tasks that depends on this address
			groups []*groupTask
			// a list of indices into the groupTask.balances where the address would be stored
			groupIndices []int
		}
		defer close(outChan)

		accountTasks := make(map[basics.Address]*addrTask)
		addressesCh := make(chan *addrTask, len(groups)*consensusParams.MaxTxGroupSize*maxAddressesInTxn(&consensusParams))
		// totalBalances counts the total number of balances over all the transaction groups
		totalBalances := 0

		initAccount := func(addr basics.Address, wg *groupTask) {
			if addr.IsZero() {
				return
			}
			if task, have := accountTasks[addr]; !have {
				task := &addrTask{
					address:      addr,
					groups:       make([]*groupTask, 1, 4),
					groupIndices: make([]int, 1, 4),
				}
				task.groups[0] = wg
				task.groupIndices[0] = wg.balancesCount

				accountTasks[addr] = task
				addressesCh <- task
			} else {
				task.groups = append(task.groups, wg)
				task.groupIndices = append(task.groupIndices, wg.balancesCount)
			}
			wg.balancesCount++
			totalBalances++
		}
		// add the fee sink address to the accountTasks/addressesCh so that it will be loaded first.
		if len(groups) > 0 {
			task := &addrTask{
				address: feeSinkAddr,
			}
			addressesCh <- task
			accountTasks[feeSinkAddr] = task
		}

		// iterate over the transaction groups and add all their account addresses to the list
		groupsReady := make([]*groupTask, len(groups))
		for i, group := range groups {
			task := &groupTask{}
			groupsReady[i] = task
			for _, stxn := range group {
				// If you add new addresses here, also add them in getTxnAddresses().
				initAccount(stxn.Txn.Sender, task)
				initAccount(stxn.Txn.Receiver, task)
				initAccount(stxn.Txn.CloseRemainderTo, task)
				initAccount(stxn.Txn.AssetSender, task)
				initAccount(stxn.Txn.AssetReceiver, task)
				initAccount(stxn.Txn.AssetCloseTo, task)
				initAccount(stxn.Txn.FreezeAccount, task)
				for _, xa := range stxn.Txn.Accounts {
					initAccount(xa, task)
				}
			}
		}

		// Add fee sink to the first group
		if len(groupsReady) > 0 {
			initAccount(feeSinkAddr, groupsReady[0])
		}
		close(addressesCh)

		// updata all the groups task :
		// allocate the correct number of balances, as well as
		// enough space on the "done" channel.
		allBalances := make([]basics.BalanceRecord, totalBalances)
		usedBalances := 0
		for _, gr := range groupsReady {
			gr.balances = allBalances[usedBalances : usedBalances+gr.balancesCount]
			gr.done = make(chan error, gr.balancesCount)
			usedBalances += gr.balancesCount
		}

		// create few go-routines to load asyncroniously the account data.
		for i := 0; i < asyncAccountLoadingThreadCount; i++ {
			go func() {
				for {
					select {
					case task, ok := <-addressesCh:
						// load the address
						if !ok {
							// the channel got closed, which mean we're done.
							return
						}
						// lookup the account data directly from the ledger.
						acctData, _, err := l.LookupWithoutRewards(rnd, task.address)
						br := basics.BalanceRecord{
							Addr:        task.address,
							AccountData: acctData,
						}
						// if there is no error..
						if err == nil {
							// update all the group tasks with the new acquired balance.
							for i, wg := range task.groups {
								wg.balances[task.groupIndices[i]] = br
								// write a nil to indicate that we're loaded one entry.
								wg.done <- nil
							}
						} else {
							// there was an error loading that entry.
							for _, wg := range task.groups {
								// notify the channel of the error.
								wg.done <- err
							}
						}
					case <-ctx.Done():
						// if the context was canceled, abort right away.
						return
					}

				}
			}()
		}

		// iterate on the transaction groups tasks. This array retains the original order.
		for i, wg := range groupsReady {
			// Wait to receive wg.balancesCount nil error messages, one for each address referenced in this txn group.
			for j := 0; j < wg.balancesCount; j++ {
				select {
				case err := <-wg.done:
					if err != nil {
						// if there is an error, report the error to the output channel.
						outChan <- loadedTransactionGroup{
							group: groups[i],
							err:   err,
						}
						return
					}
				case <-ctx.Done():
					return
				}
			}
			// if we had no error, write the result to the output channel.
			// this write will not block since we preallocated enough space on the channel.
			outChan <- loadedTransactionGroup{
				group:    groups[i],
				balances: wg.balances,
			}
		}
	}()
	return outChan
}
