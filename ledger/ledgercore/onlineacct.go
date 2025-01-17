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

package ledgercore

import (
	"github.com/Orca18/novarand/crypto/merklesignature"
	"github.com/Orca18/novarand/data/basics"
)

// An OnlineAccount corresponds to an account whose AccountData.Status
// is Online.  This is used for a Merkle tree commitment of online
// accounts, which is subsequently used to validate participants for
// a compact certificate.
/*
OnlineAccount는 AccountData.Status가 온라인인 계정에 해당합니다.
이것은 온라인 계정의 Merkle 트리 확정에 사용되며,
이후에 참가자가 컴팩트 인증서에 대해 유효성을 검사하는 데 사용됩니다.
*/
type OnlineAccount struct {
	// These are a subset of the fields from the corresponding AccountData.
	Address                 basics.Address
	MicroNovas              basics.MicroNovas
	RewardsBase             uint64
	NormalizedOnlineBalance uint64
	VoteFirstValid          basics.Round
	VoteLastValid           basics.Round
	StateProofID            merklesignature.Verifier
}
