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

	"github.com/Orca18/novarand/crypto"
)

// Constant strings used as keys for topics
// 토픽의 키로 사용되는 상수 문자열
const (
	requestHashKey = "RequestHash"
	ErrorKey       = "Error" // used for passing an error message
)

// Topic is a key-value pair
// 토픽은 키-값 쌍입니다.
type Topic struct {
	key  string
	data []byte
}

// MakeTopic Creates a Topic
// MakeTopic은 토픽을 생성합니다.
func MakeTopic(key string, data []byte) Topic {
	return Topic{key: key, data: data}
}

// Topics is an array of type Topic
// The maximum number of topics allowed is 32
// Each topic key can be 64 characters long and cannot be size 0
// 토픽은 토픽 유형의 배열입니다.
// 허용되는 최대 주제 수는 32개입니다.
// 각 토픽 키는 64자일 수 있으며 크기가 0일 수 없습니다.
type Topics []Topic

// MarshallTopics serializes the topics into a byte array
// MarshallTopics는 주제를 바이트 배열로 직렬화합니다.
func (ts Topics) MarshallTopics() (b []byte) {

	// Calculate the total buffer size required to store the topics
	// 토픽을 저장하는 데 필요한 총 버퍼 크기 계산
	bufferSize := binary.MaxVarintLen32 // store topic array size // 토픽 배열 크기 저장

	for _, val := range ts {
		bufferSize += 2 * binary.MaxVarintLen32 // store key size and the data size // 키 크기와 데이터 크기 저장
		bufferSize += len(val.key)
		bufferSize += len(val.data)
	}

	buffer := make([]byte, bufferSize)
	bidx := binary.PutUvarint(buffer, uint64(len(ts)))
	for _, val := range ts {
		// write the key size
		n := binary.PutUvarint(buffer[bidx:], uint64(len(val.key)))
		bidx += n
		// write the key
		n = copy(buffer[bidx:], []byte(val.key))
		bidx += n

		// write the data size
		n = binary.PutUvarint(buffer[bidx:], uint64(len(val.data)))
		bidx += n
		// write the data
		n = copy(buffer[bidx:], val.data)
		bidx += n
	}
	return buffer[:bidx]
}

// UnmarshallTopics unmarshalls the topics from the byte array
// UnmarshallTopics는 바이트 배열에서 토픽을 비정렬화합니다.
func UnmarshallTopics(buffer []byte) (ts Topics, err error) {
	// Get the number of topics
	// 주제의 수를 가져옵니다.
	var idx int
	numTopics, nr := binary.Uvarint(buffer[idx:])
	if nr <= 0 {
		return nil, fmt.Errorf("UnmarshallTopics: could not read the number of topics")
	}
	if numTopics > 32 { // numTopics is uint64
		return nil, fmt.Errorf("UnmarshallTopics: number of topics %d is greater than 32", numTopics)
	}
	idx += nr
	topics := make([]Topic, numTopics)

	for x := 0; x < int(numTopics); x++ {
		// read the key length
		strlen, nr := binary.Uvarint(buffer[idx:])
		if nr <= 0 {
			return nil, fmt.Errorf("UnmarshallTopics: could not read the key length")
		}
		idx += nr

		// read the key
		if len(buffer) < idx+int(strlen) || strlen > 64 || strlen == 0 {
			return nil, fmt.Errorf("UnmarshallTopics: could not read the key")
		}
		topics[x].key = string(buffer[idx : idx+int(strlen)])
		idx += int(strlen)

		// read the data length
		dataLen, nr := binary.Uvarint(buffer[idx:])
		if nr <= 0 {
			return nil, fmt.Errorf("UnmarshallTopics: could not read the data length")
		}
		idx += nr

		// read the data
		if len(buffer) < idx+int(dataLen) {
			return nil, fmt.Errorf("UnmarshallTopics: data larger than buffer size")
		}
		topics[x].data = make([]byte, dataLen)
		copy(topics[x].data, buffer[idx:idx+int(dataLen)])
		idx += int(dataLen)
	}
	return topics, nil
}

// hashTopics returns the hash of serialized topics.
// hashTopics는 직렬화된 주제의 해시를 반환합니다.
// Expects the nonce to be already added as a topic
// 논스는 이미 주제로 추가될 것으로 예상합니다.
func hashTopics(topics []byte) (partialHash uint64) {
	digest := crypto.Hash(topics)
	partialHash = digest.TrimUint64()
	return partialHash
}

// GetValue returns the value of the key if the key is found in the topics
// GetValue는 토픽에서 키가 발견되면 키의 값을 반환합니다.
func (ts *Topics) GetValue(key string) (val []byte, found bool) {
	for _, t := range *ts {
		if t.key == key {
			return t.data, true
		}
	}
	return
}
