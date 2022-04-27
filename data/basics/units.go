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
	"github.com/algorand/go-codec/codec"
	"github.com/algorand/msgp/msgp"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
)

// RoundInterval is a number of rounds
/*
라운드의 수
*/
type RoundInterval uint64

// MicroAlgos is our unit of currency.  It is wrapped in a struct to nudge
// developers to use an overflow-checking library for any arithmetic.
/*
개발자들이 overflow체킹 라이브러리를 사용할 수 있도록 구조체로 감싼 알고랜드의 최소 통화단위
*/
type MicroAlgos struct {
	Raw uint64
}

// LessThan implements arithmetic comparison for MicroAlgos
func (a MicroAlgos) LessThan(b MicroAlgos) bool {
	return a.Raw < b.Raw
}

// GreaterThan implements arithmetic comparison for MicroAlgos
func (a MicroAlgos) GreaterThan(b MicroAlgos) bool {
	return a.Raw > b.Raw
}

// IsZero implements arithmetic comparison for MicroAlgos
func (a MicroAlgos) IsZero() bool {
	return a.Raw == 0
}

// ToUint64 converts the amount of algos to uint64
func (a MicroAlgos) ToUint64() uint64 {
	return a.Raw
}

// RewardUnits returns the number of reward units in some number of algos
func (a MicroAlgos) RewardUnits(proto config.ConsensusParams) uint64 {
	return a.Raw / proto.RewardUnit
}

// We generate our own encoders and decoders for MicroAlgos
// because we want it to appear as an integer, even though
// we represent it as a single-element struct.
//msgp:ignore MicroAlgos

// CodecEncodeSelf implements codec.Selfer to encode MicroAlgos as a simple int
func (a MicroAlgos) CodecEncodeSelf(enc *codec.Encoder) {
	enc.MustEncode(a.Raw)
}

// CodecDecodeSelf implements codec.Selfer to decode MicroAlgos as a simple int
func (a *MicroAlgos) CodecDecodeSelf(dec *codec.Decoder) {
	dec.MustDecode(&a.Raw)
}

// CanMarshalMsg implements msgp.Marshaler
func (MicroAlgos) CanMarshalMsg(z interface{}) bool {
	_, ok := (z).(MicroAlgos)
	return ok
}

// MarshalMsg implements msgp.Marshaler
func (a MicroAlgos) MarshalMsg(b []byte) (o []byte) {
	o = msgp.Require(b, msgp.Uint64Size)
	o = msgp.AppendUint64(o, a.Raw)
	return
}

// CanUnmarshalMsg implements msgp.Unmarshaler
func (*MicroAlgos) CanUnmarshalMsg(z interface{}) bool {
	_, ok := (z).(*MicroAlgos)
	return ok
}

// UnmarshalMsg implements msgp.Unmarshaler
func (a *MicroAlgos) UnmarshalMsg(bts []byte) (o []byte, err error) {
	a.Raw, o, err = msgp.ReadUint64Bytes(bts)
	return
}

// Msgsize returns an upper bound estimate of the number of bytes occupied by the serialized message
func (a MicroAlgos) Msgsize() (s int) {
	return msgp.Uint64Size
}

// MsgIsZero returns whether this is a zero value
func (a MicroAlgos) MsgIsZero() bool {
	return a.Raw == 0
}

// Round represents a protocol round index
/*
프로토콜 라운드 인덱스
*/
type Round uint64

// OneTimeIDForRound maps a round to the identifier for which ephemeral key
// should be used for that round.  keyDilution specifies the number of keys
// in the bottom-level of the two-level key structure.
/*
	OneTimeIDForRound는 라운드를 해당 라운드에 사용되어야 하는 임시 키의 식별자에 매핑합니다.
	keyDilution은 2레벨 키 구조의 최하위 레벨에 있는 키의 수를 지정합니다.
*/
func OneTimeIDForRound(round Round, keyDilution uint64) crypto.OneTimeSignatureIdentifier {
	return crypto.OneTimeSignatureIdentifier{
		Batch:  uint64(round) / keyDilution,
		Offset: uint64(round) % keyDilution,
	}
}

// SubSaturate subtracts x rounds with saturation arithmetic that does not
// wrap around past zero, and instead returns 0 on underflow.
func (round Round) SubSaturate(x Round) Round {
	if round < x {
		return 0
	}

	return round - x
}

// RoundUpToMultipleOf rounds up round to the next multiple of n.
func (round Round) RoundUpToMultipleOf(n Round) Round {
	return (round + n - 1) / n * n
}
