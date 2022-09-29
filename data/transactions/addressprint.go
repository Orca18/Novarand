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
	"fmt"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
)

// AddressPrintTxnFields
type AddressPrintTxnFields struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	// 아 이 Receiver는 이미 PaymentTxnFields에서 구현되어 있기 때문에 중복으로 사용하면 안되는구나!!
	// 마샬링, 언마샬링 코드도 추가해줘야 할 듯하다!!
	Receiver2 basics.Address `codec:"rcv2"`
}

func (addressprint AddressPrintTxnFields) checkSpender2(header Header, spec SpecialAddresses, proto config.ConsensusParams) error {
	// the FeeSink account may only spend to the IncentivePool
	if header.Sender == spec.FeeSink {
		if addressprint.Receiver2 != spec.RewardsPool {
			return fmt.Errorf("cannot spend from fee sink's address %v to non incentive pool address %v", header.Sender, addressprint.Receiver2)
		}
	}
	return nil
}
