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

package protocol

// A single Algorand chain can support multiple types of compact certs, reflecting different hash functions, signature schemes, and frequency parameters.
// 단일 알고랜드 체인은 다양한 해시 함수, 서명 체계 및 빈도 매개변수를 반영하는 여러 유형의 컴팩트 인증서를 지원할 수 있습니다.

// CompactCertType identifies a particular configuration of compact certs.
// CompactCertType은 컴팩트 인증서의 특정 구성을 식별합니다.
type CompactCertType uint64

const (
	// CompactCertBasic is our initial compact cert setup, using Ed25519 ephemeral-key signatures and SHA512/256 hashes.
	// CompactCertBasic은 Ed25519 임시 키 서명 및 SHA512/256 해시를 사용하는 초기 컴팩트 인증서 설정입니다.
	CompactCertBasic CompactCertType = 0

	// NumCompactCertTypes is the max number of types of compact certs that we support.
	// NumCompactCertTypes는 우리가 지원하는 컴팩트 인증서 유형의 최대 수입니다.
	// This is used as an allocation bound for a map containing different compact cert types in msgpack encoding.
	// 이것은 msgpack 인코딩에서 다양한 컴팩트 인증서 유형을 포함하는 맵에 대한 할당 경계로 사용됩니다.
	NumCompactCertTypes int = 1
)

// SortCompactCertType implements sorting by CompactCertType keys for canonical encoding of maps in msgpack format.
// SortCompactCertType은 msgpack 형식의 맵을 표준 인코딩하기 위해 CompactCertType 키를 기준으로 정렬을 구현합니다.
//msgp:ignore SortCompactCertType
//msgp:sort CompactCertType SortCompactCertType
type SortCompactCertType []CompactCertType

func (a SortCompactCertType) Len() int           { return len(a) }
func (a SortCompactCertType) Less(i, j int) bool { return a[i] < a[j] }
func (a SortCompactCertType) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
