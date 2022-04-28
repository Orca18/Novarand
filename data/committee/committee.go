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

package committee

import (
	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/protocol"
)

// A Selector deterministically defines a cryptographic sortition committee. It
// contains both the input to the sortition VRF and the size of the sortition
// committee.
/*
	Selector는 결정적으로(뭐가 결정적?) 위원회를 정의한다.
	추첨 VFR 인풋과 위원회의 사이즈 정보를 가지고 있다.
*/
type Selector interface {
	// The hash of a struct which implements Selector is used as the input
	// to the VRF.
	// Selector 인터페이스를 구현하는 구조체의 해시값
	crypto.Hashable

	// CommitteeSize returns the size of the committee determined by this
	// Selector.
	/*
		이 셀렉터에 의해 결정된 위원회의 수
	*/
	CommitteeSize(config.ConsensusParams) uint64
}

// BalanceRecord pairs an account's address with its associated data.
//
// This struct is used to decouple LedgerReader.AccountData from basics.BalanceRecord.
//msgp:ignore BalanceRecord
/*
basics.BalanceRecord에서 LedgerReader.AccountData를 분리시키기 위해 사용되는 구조체
온라인 계정정보와 그 주소값을 가지고 있다.
*/
type BalanceRecord struct {
	basics.OnlineAccountData
	Addr basics.Address
}

// Membership encodes the parameters used to verify membership in a committee.
/*
Membership구조체는 위원회의 멤버라는 것을 증명하기 위해 사용되는 파라미터를 인코딩한 결과(?)
*/
type Membership struct {
	Record BalanceRecord
	// Selector는 제안자 및 투표위원회를 선택하기 위한 객체이다.
	Selector Selector

	// 라운드가 끝날 때 보유하고 있는 총 알고양
	TotalMoney basics.MicroNovas
}

// A Seed contains cryptographic entropy which can be used to determine a
// committee.
/*
위원회 선정 시 사용되는 SEED값
*/
type Seed [32]byte

// ToBeHashed implements the crypto.Hashable interface
func (s Seed) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.Seed, s[:]
}
