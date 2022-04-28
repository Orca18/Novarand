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

	Receiver basics.Address    `codec:"rcv"`
	Amount   basics.MicroNovas `codec:"amt"`

	// When CloseRemainderTo is set, it indicates that the
	// transaction is requesting that the account should be
	// closed, and all remaining funds be transferred to this
	// address.
	CloseRemainderTo basics.Address `codec:"close"`
}

func (payment PaymentTxnFields) checkSpender2(header Header, spec SpecialAddresses, proto config.ConsensusParams) error {
	if header.Sender == payment.CloseRemainderTo {
		return fmt.Errorf("transaction cannot close account to its sender %v", header.Sender)
	}

	// the FeeSink account may only spend to the IncentivePool
	if header.Sender == spec.FeeSink {
		if payment.Receiver != spec.RewardsPool {
			return fmt.Errorf("cannot spend from fee sink's address %v to non incentive pool address %v", header.Sender, payment.Receiver)
		}
		if payment.CloseRemainderTo != (basics.Address{}) {
			return fmt.Errorf("cannot close fee sink %v to %v", header.Sender, payment.CloseRemainderTo)
		}
	}
	return nil
}
