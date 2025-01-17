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

package transactions

import (
	"bytes"

	"github.com/Orca18/novarand/crypto"
)

// EvalMaxArgs is the maximum number of arguments to an LSig
const EvalMaxArgs = 255

// LogicSig contains logic for validating a transaction.
// LogicSig is signed by an account, allowing delegation of operations.
// OR
// LogicSig defines a contract account.
/*
LogicSig에는 트랜잭션을 검증하기 위한 논리가 포함되어 있습니다.
서명 권한 위임을 허용합니다.
또는 LogicSig는 계약 계정을 정의합니다.
*/
type LogicSig struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	// Logic signed by Sig or Msig, OR hashed to be the Address of an account.
	/*
		Logig은 Sig 또는 Msig에 의해 서명되거나 계정의 주소로 해시됩니다.
	*/
	Logic []byte `codec:"l,allocbound=config.MaxLogicSigMaxSize"`

	Sig  crypto.Signature   `codec:"sig"`
	Msig crypto.MultisigSig `codec:"msig"`

	// Args are not signed, but checked by Logic
	Args [][]byte `codec:"arg,allocbound=EvalMaxArgs,allocbound=config.MaxLogicSigMaxSize"`
}

// Blank returns true if there is no content in this LogicSig
func (lsig *LogicSig) Blank() bool {
	return len(lsig.Logic) == 0
}

// Len returns the length of Logic plus the length of the Args
// This is limited by config.ConsensusParams.LogicSigMaxSize
func (lsig *LogicSig) Len() int {
	lsiglen := len(lsig.Logic)
	for _, arg := range lsig.Args {
		lsiglen += len(arg)
	}
	return lsiglen
}

// Equal returns true if both LogicSig are equivalent.
//
// Out of paranoia, Equal distinguishes zero-length byte slices
// from byte slice-typed nil values as they may have subtly
// different behaviors within the evaluation of a LogicSig,
// due to differences in msgpack encoding behavior.
func (lsig *LogicSig) Equal(b *LogicSig) bool {
	sigs := lsig.Sig == b.Sig && lsig.Msig.Equal(b.Msig)
	if !sigs {
		return false
	}
	if !safeSliceCheck(lsig.Logic, b.Logic) {
		return false
	}

	if len(lsig.Args) != len(b.Args) {
		return false
	}
	for i := range lsig.Args {
		if !safeSliceCheck(lsig.Args[i], b.Args[i]) {
			return false
		}
	}
	return true
}

func safeSliceCheck(a, b []byte) bool {
	if a != nil && b != nil {
		return bytes.Equal(a, b)
	}
	return a == nil && b == nil
}
