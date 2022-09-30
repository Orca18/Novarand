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

package bookkeeping

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/committee"
	"github.com/Orca18/novarand/data/transactions"
	"github.com/Orca18/novarand/protocol"
)

const (
	// MaxInitialGenesisAllocationSize is the maximum number of accounts that are supported when
	// bootstrapping a new network. The number of account *can* grow further after the bootstrapping.
	// This value is used exclusively for the messagepack decoder, and has no affect on the network
	// capabilities/capacity in any way.
	MaxInitialGenesisAllocationSize = 100000000
)

// A Genesis object defines an Algorand "universe" -- a set of nodes that can
// talk to each other, agree on the ledger contents, etc.  This is defined
// by the initial account states (GenesisAllocation), the initial
// consensus protocol (GenesisProto), and the schema of the ledger.
/*
Genesis객체는 알고랜드의 세계를 정의한다 - 서로 통신할 수 있고, 원장 콘텐츠에 대해 동의 하는 등의 일을 수행할 수 있는 노드
이것은 최초 계정 상태(GenesisAllocation)와 최초 합의 프로토콜(GenesisProto)과 원장의 스키마에 의해 정의된다.
*/
type Genesis struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	// The SchemaID allows nodes to store data specific to a particular
	// universe (in case of upgrades at development or testing time),
	// and as an optimization to quickly check if two nodes are in
	// the same universe.
	SchemaID string `codec:"id"`

	// Network identifies the unique algorand network for which the ledger
	// is valid.
	// Note the Network name should not include a '-', as we generate the
	// GenesisID from "<Network>-<SchemaID>"; the '-' makes it easy
	// to distinguish between the network and schema.
	Network protocol.NetworkID `codec:"network"`

	// Proto is the consensus protocol in use at the genesis block.
	Proto protocol.ConsensusVersion `codec:"proto"`

	// Allocation determines the initial accounts and their state.
	Allocation []GenesisAllocation `codec:"alloc,allocbound=MaxInitialGenesisAllocationSize"`

	// RewardsPool is the address of the rewards pool.
	RewardsPool string `codec:"rwd"`

	// FeeSink is the address of the fee sink.
	FeeSink string `codec:"fees"`

	// Timestamp for the genesis block
	Timestamp int64 `codec:"timestamp"`

	// Arbitrary genesis comment string - will be excluded from file if empty
	Comment string `codec:"comment"`

	// DevMode defines whether this network operates in a developer mode or not. Developer mode networks
	// are a single node network, that operates without the agreement service being active. In liue of the
	// agreement service, a new block is generated each time a node receives a transaction group. The
	// default value for this field is "false", which makes this field empty from it's encoding, and
	// therefore backward compatible.
	DevMode bool `codec:"devmode"`
}

// LoadGenesisFromFile attempts to load a Genesis structure from a (presumably) genesis.json file.
func LoadGenesisFromFile(genesisFile string) (genesis Genesis, err error) {
	// Load genesis.json
	genesisText, err := ioutil.ReadFile(genesisFile)
	if err != nil {
		return
	}

	err = protocol.DecodeJSON(genesisText, &genesis)
	return
}

// ID is the effective Genesis identifier - the combination
// of the network and the ledger schema version
func (genesis Genesis) ID() string {
	return string(genesis.Network) + "-" + genesis.SchemaID
}

// A GenesisAllocation object represents an allocation of algos to
// an address in the genesis block.  Address is the checksummed
// short address.  Comment is a note about what this address is
// representing, and is purely informational.  State is the initial
// account state.
/*
GenesisAllocation은 제네시스블록(최초블록)의 주소에 할당된 알고의양을 나타낸다.
State는 최초 계정의 상태이다(AccountData)
*/
type GenesisAllocation struct {
	// Unfortunately we forgot to specify omitempty, and now
	// this struct must be encoded without omitempty for the
	// Address, Comment, and State fields..
	_struct struct{} `codec:""`

	Address string             `codec:"addr"`
	Comment string             `codec:"comment"`
	State   basics.AccountData `codec:"state"`
}

// ToBeHashed impements the crypto.Hashable interface.
func (genesis Genesis) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.Genesis, protocol.Encode(&genesis)
}

// GenesisBalances contains the information needed to generate a new ledger
// GenesisBalances는 새로운 원장을 생성하기위해 필요한 정보들을 포함한다.
type GenesisBalances struct {
	Balances    map[basics.Address]basics.AccountData
	FeeSink     basics.Address
	RewardsPool basics.Address
	Timestamp   int64
}

// MakeGenesisBalances returns the information needed to bootstrap the ledger based on the current time
func MakeGenesisBalances(balances map[basics.Address]basics.AccountData, feeSink, rewardsPool basics.Address) GenesisBalances {
	return MakeTimestampedGenesisBalances(balances, feeSink, rewardsPool, time.Now().Unix())
}

// MakeTimestampedGenesisBalances returns the information needed to bootstrap the ledger based on a given time
func MakeTimestampedGenesisBalances(balances map[basics.Address]basics.AccountData, feeSink, rewardsPool basics.Address, timestamp int64) GenesisBalances {
	return GenesisBalances{Balances: balances, FeeSink: feeSink, RewardsPool: rewardsPool, Timestamp: timestamp}
}

// MakeGenesisBlock creates a genesis block, including setup of RewardsState.
func MakeGenesisBlock(proto protocol.ConsensusVersion, genesisBal GenesisBalances, genesisID string, genesisHash crypto.Digest) (Block, error) {
	params, ok := config.Consensus[proto]
	if !ok {
		return Block{}, fmt.Errorf("unsupported protocol %s", proto)
	}

	genesisRewardsState := RewardsState{
		FeeSink:                   genesisBal.FeeSink,
		RewardsPool:               genesisBal.RewardsPool,
		RewardsLevel:              0,
		RewardsResidue:            0,
		RewardsRecalculationRound: basics.Round(params.RewardsRateRefreshInterval),
	}

	initialRewards := genesisBal.Balances[genesisBal.RewardsPool].MicroNovas.Raw
	if params.InitialRewardsRateCalculation {
		genesisRewardsState.RewardsRate = basics.SubSaturate(initialRewards, params.MinBalance) / uint64(params.RewardsRateRefreshInterval)
	} else {
		genesisRewardsState.RewardsRate = initialRewards / uint64(params.RewardsRateRefreshInterval)
	}

	blk := Block{
		BlockHeader: BlockHeader{
			Round:        0,
			Branch:       BlockHash{},
			Seed:         committee.Seed(genesisHash),
			TxnRoot:      transactions.Payset{}.CommitGenesis(),
			TimeStamp:    genesisBal.Timestamp,
			GenesisID:    genesisID,
			RewardsState: genesisRewardsState,
			UpgradeState: UpgradeState{
				CurrentProtocol: proto,
			},
			UpgradeVote: UpgradeVote{},
		},
	}

	if params.SupportGenesisHash {
		blk.BlockHeader.GenesisHash = genesisHash
	}

	return blk, nil
}
