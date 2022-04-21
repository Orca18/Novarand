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
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/committee/sortition"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/protocol"
)

type (
	// An UnauthenticatedCredential is a Credential which has not yet been
	// authenticated.
	/*
		UnauthenticatedCredential은 아직 권한을 받지 않은 자격증이다.
	*/
	UnauthenticatedCredential struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`
		// vrf 검증 시 사용되는 증명값
		Proof crypto.VrfProof `codec:"pf"`
	}

	// A Credential represents a proof of committee membership.
	//
	// The multiplicity of this membership is specified in the Credential's
	// weight. The VRF output hash (with the owner's address hashed in) is
	// also cached.
	//
	// Upgrades: whether or not domain separation is enabled is cached.
	// If this flag is set, this flag also includes original hashable
	// credential.
	/*
		Credential은 위원회의 멤버십의 증명을 나타낸다.
		weight가 0이상이면 선택이 된것이다(블록제안 혹은 투표 참여겠지?)
		 VRF output hash 또한 캐싱된다.
	*/
	Credential struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`
		// 제안자 혹은 투표자로 선출여부를 판단하기 위한 값
		Weight uint64 `codec:"wt"`
		// RawOut  crypto.VrfOutput의 해시값.
		VrfOut crypto.Digest `codec:"h"`

		// 설정값 중에 하나. 이미 설정된 값을 사용한다.
		DomainSeparationEnabled bool `codec:"ds"`
		//
		Hashable hashableCredential `codec:"hc"`

		// 아직 권한을 받지 않은 자격증
		UnauthenticatedCredential
	}

	// 해싱할 수 있는 vrfoutput, 주소값 => 이 객체를 해싱해서 해당 주소를 사용하는
	// 계정의 위원회 선정여부를 확인한다.
	hashableCredential struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`
		// Vrf에 의해 출력된 VrfOutput. UnauthenticatedCredential.Proof와 함께 검증에 사용
		RawOut crypto.VrfOutput `codec:"v"`
		// 주소
		Member basics.Address `codec:"m"`
		// 반복을 위한 값
		Iter uint64 `codec:"i"`
	}
)

// Verify an unauthenticated Credential that was received from the network.
//
// Verify checks if the given credential is a valid proof of membership
// conditioned on the provided committee membership parameters.
//
// If it is, the returned Credential constitutes a proof of this fact.
// Otherwise, an error is returned.
/*
	네트워크에서 수신한  unauthenticated Credential 검증합니다.
	제공된  unauthenticated Credential이 유효한 멤버십 proof인지 확인합니다.
	유효하다면 Credential을 반환하고 그렇지 않으면 오류가 반환됩니다.
*/
func (cred UnauthenticatedCredential) Verify(proto config.ConsensusParams, m Membership) (res Credential, err error) {
	selectionKey := m.Record.SelectionID

	// proof값이 올바르다면 랜덤한 vrfOutput을 반환한다.
	ok, vrfOut := selectionKey.Verify(cred.Proof, m.Selector)

	hashable := hashableCredential{
		RawOut: vrfOut,
		Member: m.Record.Addr,
	}

	// Also hash in the address. This is necessary to decorrelate the selection of different accounts that have the same VRF key.
	/*

	 */
	var h crypto.Digest
	if proto.CredentialDomainSeparationEnabled {
		h = crypto.HashObj(hashable)
	} else {
		h = crypto.Hash(append(vrfOut[:], m.Record.Addr[:]...))
	}

	if !ok {
		err = fmt.Errorf("UnauthenticatedCredential.Verify: could not verify VRF Proof with %v (parameters = %+v, proof = %#v)", selectionKey, m, cred.Proof)
		return
	}

	var weight uint64
	userMoney := m.Record.VotingStake()
	expectedSelection := float64(m.Selector.CommitteeSize(proto))

	if m.TotalMoney.Raw < userMoney.Raw {
		logging.Base().Panicf("UnauthenticatedCredential.Verify: total money = %v, but user money = %v", m.TotalMoney, userMoney)
	} else if m.TotalMoney.IsZero() || expectedSelection == 0 || expectedSelection > float64(m.TotalMoney.Raw) {
		logging.Base().Panicf("UnauthenticatedCredential.Verify: m.TotalMoney %v, expectedSelection %v", m.TotalMoney.Raw, expectedSelection)
	} else if !userMoney.IsZero() {
		weight = sortition.Select(userMoney.Raw, m.TotalMoney.Raw, expectedSelection, h)
	}

	if weight == 0 {
		err = fmt.Errorf("UnauthenticatedCredential.Verify: credential has weight 0")
	} else {
		res = Credential{
			UnauthenticatedCredential: cred,
			VrfOut:                    h,
			Weight:                    weight,
			DomainSeparationEnabled:   proto.CredentialDomainSeparationEnabled,
		}
		if res.DomainSeparationEnabled {
			res.Hashable = hashable
		}
	}
	return
}

// MakeCredential creates a new unauthenticated Credential given some selector.
/*
	주어진 selector로부터 새로운 unauthenticated Credential를 생성한다.
*/
func MakeCredential(secrets *crypto.VrfPrivkey, sel Selector) UnauthenticatedCredential {
	pf, ok := secrets.Prove(sel)
	if !ok {
		logging.Base().Error("Failed to construct a VRF proof -- participation key may be corrupt")
		return UnauthenticatedCredential{}
	}
	return UnauthenticatedCredential{Proof: pf}
}

// Less returns true if this Credential is less than the other credential; false
// otherwise (i.e., >=).
// Used for breaking ties when there are multiple proposals.
//
// Precondition: both credentials have nonzero weight
func (cred Credential) Less(otherCred Credential) bool {
	i1 := cred.lowestOutput()
	i2 := otherCred.lowestOutput()

	return i1.Cmp(i2) < 0
}

// Equals compares the hash of two Credentials to determine equality and returns
// true if they're equal.
func (cred Credential) Equals(otherCred Credential) bool {
	return cred.VrfOut == otherCred.VrfOut
}

// Selected returns whether this Credential was selected (i.e., if its weight is
// greater than zero).
func (cred Credential) Selected() bool {
	return cred.Weight > 0
}

// lowestOutput is used for breaking ties when there are multiple proposals.
// People will vote for the proposal whose credential has the lowest lowestOutput.
//
// We hash the credential and interpret the output as a bigint.
// For credentials with weight w > 1, we hash the credential w times (with
// different counter values) and use the lowest output.
//
// This is because a weight w credential is simulating being selected to be on the
// leader committee w times, so each of the w proposals would have a different hash,
// and the lowest would win.
/*
여러 제안이 있을 때 관계를 끊는 데 최저출력이 사용됩니다.
사람들은 자격 증명이 가장 낮은 출력을 갖는 제안에 투표할 것입니다.
자격 증명을 해시하고 출력을 bigint로 해석합니다.
가중치 w > 1인 자격 증명의 경우 자격 증명을 w번 해시하고(다른 카운터 값으로) 가장 낮은 출력을 사용합니다.
이것은 가중치 w 자격 증명이 리더 위원회에 w번 선정되도록 시뮬레이션하고 있으므로 w
제안마다 다른 해시를 가지며 가장 낮은 것이 승리하기 때문입니다
*/
func (cred Credential) lowestOutput() *big.Int {
	var lowest big.Int

	h1 := cred.VrfOut
	// It is important that i start at 1 rather than 0 because cred.Hashable
	// was already hashed with iter = 0 earlier (in UnauthenticatedCredential.Verify)
	// for determining the weight of the credential. A nonzero iter provides
	// domain separation between lowestOutput and UnauthenticatedCredential.Verify
	//
	// If we reused the iter = 0 hash output here it would be nonuniformly
	// distributed (because lowestOutput can only get called if weight > 0).
	// In particular if i starts at 0 then weight-1 credentials are at a
	// significant disadvantage because UnauthenticatedCredential.Verify
	// wants the hash to be large but tiebreaking between proposals wants
	// the hash to be small.
	for i := uint64(1); i <= cred.Weight; i++ {
		var h crypto.Digest
		if cred.DomainSeparationEnabled {
			cred.Hashable.Iter = i
			h = crypto.HashObj(cred.Hashable)
		} else {
			var h2 crypto.Digest
			binary.BigEndian.PutUint64(h2[:], i)
			h = crypto.Hash(append(h1[:], h2[:]...))
		}

		if i == 1 {
			lowest.SetBytes(h[:])
		} else {
			var temp big.Int
			temp.SetBytes(h[:])
			if temp.Cmp(&lowest) < 0 {
				lowest.Set(&temp)
			}
		}
	}

	return &lowest
}

// LowestOutputDigest gives the lowestOutput as a crypto.Digest, which allows
// pretty-printing a proposal's lowest output.
// This function is only used for debugging.
func (cred Credential) LowestOutputDigest() crypto.Digest {
	lbytes := cred.lowestOutput().Bytes()
	var out crypto.Digest
	if len(lbytes) > len(out) {
		panic("Cred lowest output too long")
	}
	copy(out[len(out)-len(lbytes):], lbytes)
	return out
}

func (cred hashableCredential) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.Credential, protocol.Encode(&cred)
}
