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

package basics

import (
	"encoding/binary"
	"fmt"

	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/crypto/merklesignature"
	"github.com/Orca18/novarand/protocol"
)

const (
	// ErrIndexOutOfBound returned when an index is out of the array's bound
	ErrIndexOutOfBound = "pos %d past end %d"
)

// A Participant corresponds to an account whose AccountData.Status
// is Online, and for which the expected sigRound satisfies
// AccountData.VoteFirstValid <= sigRound <= AccountData.VoteLastValid.
//
// In the Algorand ledger, it is possible for multiple accounts to have
// the same PK.  Thus, the PK is not necessarily unique among Participants.
// However, each account will produce a unique Participant struct, to avoid
// potential DoS attacks where one account claims to have the same VoteID PK
// as another account.
/*
Participant는 AccountData.Status가 온라인 이고(즉, 합의에 참여가능)
sigRound값이 합의에 참여 가능한 라운드 안에 있는 계정과 동일하다.
알고랜드 원장에서는 여러 계정이 동일한 pk를 갖는것이 가능하다(왜지??). 따라서 Participants간에
PK는 반드시 유니크지는 않다. 하지만 각 계정은 유니크한 Participant 구조체를 생성한다.
왜냐하면 하나의 계정이 다른 계정과 동일한 VoteID PK를 갖겠다고 요구하는 DoS공격을 방지하기 위해서다.
*/
type Participant struct {
	// 이게 뭘까...
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	// PK is the identifier used to verify the signature for a specific participant
	// PK는 특정 참여자의 서명을 증명하기 위한 식별자이다.
	PK merklesignature.Verifier `codec:"p"`

	// Weight is AccountData.MicroNovas.
	// Weight는 보유한 알고양이다.
	Weight uint64 `codec:"w"`
}

// ToBeHashed implements the crypto.Hashable interface.
// In order to create a more SNARK-friendly commitments on the signature we must avoid using the msgpack infrastructure.
// msgpack creates a compressed representation of the struct which might be varied in length, which will
// be bad for creating SNARK
/*
서명 생성 시 더욱 SNARK friendly한 약속을 생성하기 위해선 msgpack의 사용을 피해야 한다.
msgpack은 압축된 값을 생성하기 때문에 SNARK를 생성하기 위한 길이와 다를 수 있다.

해당 Participant의 압축 서명을 생성한다.
*/
func (p Participant) ToBeHashed() (protocol.HashID, []byte) {

	weightAsBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(weightAsBytes, p.Weight)

	publicKeyBytes := p.PK

	partCommitment := make([]byte, 0, len(weightAsBytes)+len(publicKeyBytes))
	partCommitment = append(partCommitment, weightAsBytes...)
	partCommitment = append(partCommitment, publicKeyBytes[:]...)

	return protocol.CompactCertPart, partCommitment
}

// ParticipantsArray implements merklearray.Array and is used to commit to a Merkle tree of online accounts.
//msgp:ignore ParticipantsArray
/* ParticipantsArray는 머클배열을 구현한다. 배열은 온라인 계정의 머클트리를 커밋하기 위해 사용된다.*/
type ParticipantsArray []Participant

// Length returns the ledger of the array.
func (p ParticipantsArray) Length() uint64 {
	return uint64(len(p))
}

// Marshal Returns the hash for the given position.
func (p ParticipantsArray) Marshal(pos uint64) (crypto.Hashable, error) {
	if pos >= uint64(len(p)) {
		return nil, fmt.Errorf(ErrIndexOutOfBound, pos, len(p))
	}

	return p[pos], nil
}
