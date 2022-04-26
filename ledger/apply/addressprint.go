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

package apply

import (
	"fmt"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/transactions"
)

func checkSpender2(payment transactions.PaymentTxnFields, header transactions.Header, spec transactions.SpecialAddresses, proto config.ConsensusParams) error {
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

// Payment changes the balances according to this transaction.
// The ApplyData argument should reflect the changes made by
// apply().  It may already include changes made by the caller
// (i.e., Transaction.Apply), so apply() must update it rather
// than overwriting it.  For example, Transaction.Apply() may
// have updated ad.SenderRewards, and this function should only
// add to ad.SenderRewards (if needed), but not overwrite it.
/*
Payment은 이 transaction에 따라 잔액을 변경합니다.
ApplyData 인수는 apply()에 의해 변경된 사항을 반영해야 합니다.
호출자(즉, Transaction.Apply)가 변경한 사항이 이미 포함되어 있을 수 있으므로
apply()는 덮어쓰지 않고 업데이트해야 합니다.
예를 들어 Transaction.Apply()는 ad.SenderRewards를 업데이트했을 수 있으며
이 함수는 ad.SenderRewards(필요한 경우)에만 추가해야 하지만 덮어쓰면 안 됩니다.
*/
func AddressPrint(addressprint transactions.AddressPrintTxnFields, header transactions.Header, balances Balances, spec transactions.SpecialAddresses, ad *transactions.ApplyData) error {
	/*
		// move tx money0
		if !payment.Amount.IsZero() || payment.Receiver != (basics.Address{}) {
			err := balances.Move(header.Sender, payment.Receiver, payment.Amount, &ad.SenderRewards, &ad.ReceiverRewards)
			if err != nil {
				return err
			}
		}

		if payment.CloseRemainderTo != (basics.Address{}) {
			rec, err := balances.Get(header.Sender, true)
			if err != nil {
				return err
			}

			closeAmount := rec.MicroAlgos
			ad.ClosingAmount = closeAmount
			err = balances.Move(header.Sender, payment.CloseRemainderTo, closeAmount, &ad.SenderRewards, &ad.CloseRewards)
			if err != nil {
				return err
			}

			// Confirm that we have no balance left
			rec, err = balances.Get(header.Sender, true)
			if err != nil {
				return err
			}
			if !rec.MicroAlgos.IsZero() {
				return fmt.Errorf("balance %d still not zero after CloseRemainderTo", rec.MicroAlgos.Raw)
			}

			// Confirm that there is no asset-related state in the account
			if len(rec.Assets) > 0 {
				return fmt.Errorf("cannot close: %d outstanding assets", len(rec.Assets))
			}

			if len(rec.AssetParams) > 0 {
				// This should be impossible because every asset created
				// by an account (in AssetParams) must also appear in Assets,
				// which we checked above.
				return fmt.Errorf("cannot close: %d outstanding created assets", len(rec.AssetParams))
			}

			// Confirm that there is no application-related state remaining
			if len(rec.AppLocalStates) > 0 {
				return fmt.Errorf("cannot close: %d outstanding applications opted in. Please opt out or clear them", len(rec.AppLocalStates))
			}

			// Can't have created apps remaining either
			if len(rec.AppParams) > 0 {
				return fmt.Errorf("cannot close: %d outstanding created applications", len(rec.AppParams))
			}

			// Clear out entire account record, to allow the DB to GC it
			rec = basics.AccountData{}
			err = balances.Put(header.Sender, rec)
			if err != nil {
				return err
			}
		}
	*/

	return nil
}
