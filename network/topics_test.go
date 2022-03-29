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

package network

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/Orca18/novarand/test/partitiontest"
	"github.com/stretchr/testify/require"
)

// Test the marshall/unmarshall of Topics
// 토픽의 정렬/정렬 해제 테스트
func TestTopics(t *testing.T) {
	partitiontest.PartitionTest(t)

	topics := Topics{
		Topic{
			key:  "key1",
			data: []byte("value 1"),
		},
		Topic{
			key:  "Key2",
			data: []byte("value of key2"),
		},
	}

	// Check if the topics were initialized correctly
	// 토픽이 올바르게 초기화되었는지 확인
	require.Equal(t, 2, len(topics))

	require.Equal(t, "key1", topics[0].key)
	require.Equal(t, "value 1", string(topics[0].data))

	require.Equal(t, "Key2", topics[1].key)
	val, found := topics.GetValue("Key2")
	require.Equal(t, true, found)
	require.Equal(t, "value of key2", string(val))

	// Check if can be marshalled without errors
	// 오류 없이 마샬링할 수 있는지 확인
	buffer := topics.MarshallTopics()

	// Check if can be unmarshalled without errors
	// 오류 없이 비정렬화될 수 있는지 확인
	unMarshalled, e := UnmarshallTopics(buffer)
	require.Empty(t, e)

	// Check if the unmarshalled is equal to the original
	// unmarshaled가 원본과 같은지 확인
	require.Equal(t, len(topics), len(unMarshalled))

	require.Equal(t, topics[0].key, unMarshalled[0].key)
	require.Equal(t, topics[0].data, unMarshalled[0].data)

	require.Equal(t, topics[1].key, unMarshalled[1].key)
	require.Equal(t, topics[1].data, unMarshalled[1].data)
}

// TestCurruptedTopics checks the errors
// Makes sure UnmarshallTopics will not attempt to read beyond the buffer limits
// TestCurruptedTopics는 오류를 확인합니다.
// UnmarshallTopics가 버퍼 제한을 초과하여 읽지 않도록 합니다.
func TestCurruptedTopics(t *testing.T) {
	partitiontest.PartitionTest(t)

	var buffer []byte

	// empty buffer
	buffer = make([]byte, 0)
	_, err := UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: could not read the number of topics"))

	// more than 32 topics
	buffer = make([]byte, binary.MaxVarintLen32)
	binary.PutUvarint(buffer, 33)
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: number of topics %d is greater than 32", 33))

	// no room for the key length
	// 키 길이를 위한 공간 없음
	buffer = make([]byte, 1)
	binary.PutUvarint(buffer, 1)
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: could not read the key length"))

	// key length > buffer size
	buffer = make([]byte, 2)
	binary.PutUvarint(buffer, 1)
	binary.PutUvarint(buffer[1:], 5)
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: could not read the key"))

	// key length > buffer size 64
	buffer = make([]byte, 100)
	binary.PutUvarint(buffer, 1)
	binary.PutUvarint(buffer[1:], 65)
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: could not read the key"))

	// no room for the data length
	// 키 길이를 위한 공간 없음
	buffer = make([]byte, 3)
	binary.PutUvarint(buffer, 1)     // 1 topic
	binary.PutUvarint(buffer[1:], 1) // 1 char key
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: could not read the data length"))

	// datalen > buffer size
	buffer = make([]byte, 5)
	binary.PutUvarint(buffer, 1)     // 1 topic
	binary.PutUvarint(buffer[1:], 1) // 1 char key
	// buffer size is 5. Room for 1 byte data.
	// [/*topics:*/1, /*key len:*/ 1, /*key:*/ 0, /*data len:*/ 2, /*1 byte space for data*/ 0]
	// 2 byte data size should error
	// 버퍼 크기는 5입니다. 1바이트 데이터를 위한 공간입니다.
	// [/*topics:*/1, /*key len:*/ 1, /*key:*/ 0, /*data len:*/ 2, /*데이터를 위한 1바이트 공간*/ 0]
	// 2바이트 데이터 크기는 오류여야 합니다.
	binary.PutUvarint(buffer[3:], 2)
	_, err = UnmarshallTopics(buffer)
	require.Equal(t, err, fmt.Errorf("UnmarshallTopics: data larger than buffer size"))
}
