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
	"log"
	"os"
	"path/filepath"

	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/transactions"
)

/*
	AddressPrint트랜잭션을 실행하기 위한 함수
*/
func AddressPrint(addressprint transactions.AddressPrintTxnFields, header transactions.Header, balances Balances, spec transactions.SpecialAddresses, ad *transactions.ApplyData) error {
	// sender와 receiver와 라운드 정보를 로그파일에 저장하는 함수
	// 로그파일에 저장하는 로직 가져오기

	//1. sender, receiver 주소 가져오기
	sender := header.Sender
	receiver := addressprint.Receiver2

	//3. 로그파일에 저장하기
	transactionLog(sender, receiver)

	return nil
}

func transactionLog(sender basics.Address, receiver basics.Address) error {
	dir := os.Getenv("ALGORAND_DATA")
	fmt.Println("폴더 경로: ", dir)

	absolutePath, absPathErr := filepath.Abs(dir)

	// 데이터 경로 가져오는데 에러 발생
	if absPathErr != nil {
		fmt.Println("데이터폴더 경로 가져올 때 에러발생!! ", absPathErr)
		return fmt.Errorf("데이터폴더 경로 가져올 때 에러발생!! %w", absPathErr)
	}

	// 로그파일 생성
	txnLogFilePath := filepath.Join(absolutePath, "addressprint.log")
	fmt.Println("파일 경로: ", txnLogFilePath)

	// 해당 파일의 모드 설정
	txnLogFileMode := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	logFile, err := os.OpenFile(txnLogFilePath, txnLogFileMode, 0666)

	if err != nil {
		//fmt.Println("만약 에러가 없지 않으면=에러가 있으면")
		log.Fatalf("error opening file: %v", err)
	}

	defer logFile.Close()
	log.SetOutput(logFile)

	log.Println(" [Sender]: ", sender, "[Receiver]: ", receiver)
	return nil
}
