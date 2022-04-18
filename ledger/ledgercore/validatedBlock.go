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
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/data/committee"
)

// ValidatedBlock represents the result of a block validation.  It can
// be used to efficiently add the block to the ledger, without repeating
// the work of applying the block's changes to the ledger state.
/*
ValidatedBlock은 블록 유효성 검사의 결과를 나타냅니다.
원장 상태에 블록의 변경 사항을 적용하는 작업을 반복하지 않고 블록을
원장에 효율적으로 추가할 수 있다..
(StateDelta 덕분인것 같다. StateDelta가 이전 상태와의 차이를 나타내므로 이전에서 변경된 부분만 저장하면
되는 듯하다!!)
*/
type ValidatedBlock struct {
	blk bookkeeping.Block
	// 이게 유효성검사 결과인듯?
	delta StateDelta
}

// Block returns the underlying Block for a ValidatedBlock.
func (vb ValidatedBlock) Block() bookkeeping.Block {
	return vb.blk
}

// Delta returns the underlying Delta for a ValidatedBlock.
func (vb ValidatedBlock) Delta() StateDelta {
	return vb.delta
}

// WithSeed returns a copy of the ValidatedBlock with a modified seed.
func (vb ValidatedBlock) WithSeed(s committee.Seed) ValidatedBlock {
	newblock := vb.blk
	newblock.BlockHeader.Seed = s

	return ValidatedBlock{
		blk:   newblock,
		delta: vb.delta,
	}
}

// MakeValidatedBlock creates a validated block.
func MakeValidatedBlock(blk bookkeeping.Block, delta StateDelta) ValidatedBlock {
	return ValidatedBlock{
		blk:   blk,
		delta: delta,
	}
}
