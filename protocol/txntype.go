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

// Transaction types indicate different types of transactions that can appear in a block.
// 트랜잭션 유형은 블록에 나타날 수 있는 다양한 유형의 트랜잭션을 나타냅니다.
// They are used in the data/transaction package and the REST API.
// 데이터/트랜잭션 패키지 및 REST API에서 사용됩니다.

// TxType is the type of the transaction written to the ledger
// TxType은 원장에 기록된 트랜잭션의 유형입니다.
type TxType string

const (
	// PaymentTx indicates a payment transaction
	// PaymentTx는 결제 거래를 나타냅니다.
	PaymentTx TxType = "pay"

	// KeyRegistrationTx indicates a transaction that registers participation keys
	// KeyRegistrationTx는 참여 키를 등록하는 트랜잭션을 나타냅니다.
	KeyRegistrationTx TxType = "keyreg"

	// AssetConfigTx creates, re-configures, or destroys an asset
	// AssetConfigTx는 자산을 생성, 재구성 또는 파괴합니다.
	AssetConfigTx TxType = "acfg"

	// AssetTransferTx transfers assets between accounts (optionally closing)
	// AssetTransferTx는 계정 간에 자산을 전송합니다(선택적으로 폐쇄)
	AssetTransferTx TxType = "axfer"

	// AssetFreezeTx changes the freeze status of an asset
	// AssetFreezeTx는 자산의 정지 상태를 변경합니다.
	AssetFreezeTx TxType = "afrz"

	// ApplicationCallTx allows creating, deleting, and interacting with an application
	// ApplicationCallTx는 애플리케이션 생성, 삭제 및 상호 작용을 허용합니다.
	ApplicationCallTx TxType = "appl"

	// CompactCertTx records a compact certificate
	// CompactCertTx는 컴팩트 인증서를 기록합니다.
	CompactCertTx TxType = "cert"

	// (추가) 새로만들 트랜잭션 타입 정의!!
	AddressPrintTx TxType = "addrprint"

	// UnknownTx signals an error
	// UnknownTx는 오류 신호를 보냅니다.
	UnknownTx TxType = "unknown"
)
