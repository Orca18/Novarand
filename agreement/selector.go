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

package agreement

import (
	"fmt"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/committee"
	"github.com/Orca18/novarand/protocol"
)

// A Selector is the input used to define proposers and members of voting
// committees.
/*
	Selector는 제안자 및 투표위원회를 선택하기 위한 객체이다.
*/
type selector struct {
	_struct struct{} `codec:""` // not omitempty
	// VRF의 인풋으로 사용될 인풋 값
	Seed committee.Seed `codec:"seed"`
	// 제안 혹은 투표할 라운드
	Round basics.Round `codec:"rnd"`
	// 프로토콜에서 주어진 라운드의 진행 상황을 추적하는 데 사용된다.
	Period period `codec:"per"`
	// 알고랜드의 개별 단계를 나타내는 시퀀스 번호이다(propose, soft, cert, next)
	Step step `codec:"step"`
}

// ToBeHashed implements the crypto.Hashable interface.
func (sel selector) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.AgreementSelector, protocol.Encode(&sel)
}

// CommitteeSize returns the size of the committee, which is determined by
// Selector.Step.
func (sel selector) CommitteeSize(proto config.ConsensusParams) uint64 {
	return sel.Step.committeeSize(proto)
}

func balanceRound(r basics.Round, cparams config.ConsensusParams) basics.Round {
	return r.SubSaturate(basics.Round(2 * cparams.SeedRefreshInterval * cparams.SeedLookback))
}

func seedRound(r basics.Round, cparams config.ConsensusParams) basics.Round {
	return r.SubSaturate(basics.Round(cparams.SeedLookback))
}

// a helper function for obtaining membership verification parameters.
/*
	멤버십 증명 파라미터를 획득하기 위한 함수
	올바른 주소이며, 라운드에 대한 전체 알고를 알 수 있고, 라운드 시드를 알 수 있다면 무조건 반환한다.
	즉, 계정이 올바르면 무조건 반환하는거라고 봐야겠네?
*/
func membership(l LedgerReader, addr basics.Address, r basics.Round, p period, s step) (m committee.Membership, err error) {
	cparams, err := l.ConsensusParams(ParamsRound(r))
	if err != nil {
		return
	}
	balanceRound := balanceRound(r, cparams)
	seedRound := seedRound(r, cparams)

	record, err := l.LookupAgreement(balanceRound, addr)
	if err != nil {
		err = fmt.Errorf("Service.initializeVote (r=%d): Failed to obtain balance record for address %v in round %d: %w", r, addr, balanceRound, err)
		return
	}

	// 특정 라운드의 전체 algo
	total, err := l.Circulation(balanceRound)
	if err != nil {
		err = fmt.Errorf("Service.initializeVote (r=%d): Failed to obtain total circulation in round %d: %v", r, balanceRound, err)
		return
	}

	seed, err := l.Seed(seedRound)
	if err != nil {
		err = fmt.Errorf("Service.initializeVote (r=%d): Failed to obtain seed in round %d: %v", r, seedRound, err)
		return
	}

	m.Record = committee.BalanceRecord{OnlineAccountData: record, Addr: addr}
	m.Selector = selector{Seed: seed, Round: r, Period: p, Step: s}
	m.TotalMoney = total
	return m, nil
}
