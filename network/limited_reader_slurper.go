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
	"errors"
	"io"
)

// ErrIncomingMsgTooLarge is returned when an incoming message is too large
// 수신 메시지가 너무 크면 ErrIncomingMsgTooLarge가 반환됩니다.
var ErrIncomingMsgTooLarge = errors.New("read limit exceeded")

// allocationStep is the amount of memory allocated at any single time we don't have enough memory allocated.
// 할당 단계는 할당된 메모리가 충분하지 않을 때 할당된 메모리의 양입니다.
const allocationStep = uint64(64 * 1024)

// LimitedReaderSlurper collects bytes from an io.Reader, but stops if a limit is reached.
// LimitedReaderSlurper는 io.Reader에서 바이트를 수집하지만 제한에 도달하면 중지합니다.
type LimitedReaderSlurper struct {
	// remainedUnallocatedSpace is how much more memory we are allowed to allocate for this reader beyond the base allocation.
	// 남아있는UnallocatedSpace는 기본 할당 이상으로 이 리더에 할당할 수 있는 메모리의 양입니다.
	remainedUnallocatedSpace uint64

	// the buffers array contain the memory buffers used to store the data. The first level array is preallocated dependening on the desired base allocation. The rest of the levels are dynamically allocated on demand.
	// 버퍼 배열은 데이터를 저장하는 데 사용되는 메모리 버퍼를 포함합니다. 첫 번째 레벨 배열은 원하는 기본 할당에 따라 미리 할당됩니다. 나머지 레벨은 요청 시 동적으로 할당됩니다.
	buffers [][]byte

	// lastBuffer is the index of the last filled buffer, or the first one if no buffer was ever filled.
	// lastBuffer는 마지막으로 채워진 버퍼의 인덱스이거나 버퍼가 채워지지 않은 경우 첫 번째 버퍼입니다.
	lastBuffer int
}

// MakeLimitedReaderSlurper creates a LimitedReaderSlurper instance with the provided base and max memory allocations.
// MakeLimitedReaderSlurper는 제공된 기본 및 최대 메모리 할당으로 LimitedReaderSlurper 인스턴스를 생성합니다.
func MakeLimitedReaderSlurper(baseAllocation, maxAllocation uint64) *LimitedReaderSlurper {
	if baseAllocation > maxAllocation {
		baseAllocation = maxAllocation
	}
	lrs := &LimitedReaderSlurper{
		remainedUnallocatedSpace: maxAllocation - baseAllocation,
		lastBuffer:               0,
		buffers:                  make([][]byte, 1+(maxAllocation-baseAllocation+allocationStep-1)/allocationStep),
	}
	lrs.buffers[0] = make([]byte, 0, baseAllocation)
	return lrs
}

// Read does repeated Read()s on the io.Reader until it gets io.EOF.
// Returns underlying error or ErrIncomingMsgTooLarge if limit reached.
// Returns a nil error if the underlying io.Reader returned io.EOF.
// Read는 io.EOF가 될 때까지 io.Reader에서 Read()를 반복합니다.
// 한계에 도달하면 기본 오류 또는 ErrIncomingMsgTooLarge를 반환합니다.
// 기본 io.Reader가 io.EOF를 반환하면 nil 오류를 반환합니다.
func (s *LimitedReaderSlurper) Read(reader io.Reader) error {
	var readBuffer []byte
	for {
		// do we have more room in the current buffer ?
		// 현재 버퍼에 더 많은 공간이 있습니까?
		if len(s.buffers[s.lastBuffer]) == cap(s.buffers[s.lastBuffer]) {
			// current buffer is full, try to expand buffers
			// 현재 버퍼가 가득 찼습니다. 버퍼 확장을 시도합니다.
			if s.remainedUnallocatedSpace == 0 {
				// we ran out of memory, but is there any more data ?
				// 메모리가 부족하지만 더 이상 데이터가 있습니까?
				n, err := reader.Read(make([]byte, 1))
				switch {
				case n > 0:
					// yes, there was at least one extra byte - return ErrIncomingMsgTooLarge
					// 예, 최소한 하나의 추가 바이트가 있었습니다. 반환 ErrIncomingMsgTooLarge
					return ErrIncomingMsgTooLarge
				case err == io.EOF:
					// no, no more data. just return nil
					// 아니요, 더 이상 데이터가 없습니다. 그냥 반환 없음
					return nil
				case err == nil:
					// if we received err == nil and n == 0, we should retry calling the Read function.
					// err == nil 및 n == 0을 수신했다면 Read 함수를 다시 호출해야 합니다.
					continue
				default:
					// if we received a non-io.EOF error, return it.
					// io.EOF가 아닌 오류가 수신되면 반환합니다.
					return err
				}
			}

			// make another buffer
			// 버퍼를 하나 더 만든다
			s.allocateNextBuffer()
		}

		readBuffer = s.buffers[s.lastBuffer]
		// the entireBuffer is the same underlying buffer as readBuffer, but the length was moved to the maximum buffer capacity.
		// wholeBuffer는 readBuffer와 동일한 기본 버퍼이지만 길이가 최대 버퍼 용량으로 이동되었습니다.
		entireBuffer := readBuffer[:cap(readBuffer)]
		// read the data into the unused area of the read buffer.
		// 읽기 버퍼의 사용되지 않은 영역으로 데이터를 읽습니다.
		n, err := reader.Read(entireBuffer[len(readBuffer):])
		if err != nil {
			if err == io.EOF {
				s.buffers[s.lastBuffer] = readBuffer[:len(readBuffer)+n]
				return nil
			}
			return err
		}
		s.buffers[s.lastBuffer] = readBuffer[:len(readBuffer)+n]
	}
}

// Size returs the current total size of contained chunks read from io.Reader
// Size는 io.Reader에서 읽은 포함된 청크의 현재 총 크기를 반환합니다.
func (s *LimitedReaderSlurper) Size() (size uint64) {
	for i := 0; i <= s.lastBuffer; i++ {
		size += uint64(len(s.buffers[i]))
	}
	return
}

// Reset clears the buffered data
// 재설정은 버퍼링된 데이터를 지웁니다.
func (s *LimitedReaderSlurper) Reset() {
	for i := 1; i <= s.lastBuffer; i++ {
		s.remainedUnallocatedSpace += uint64(cap(s.buffers[i]))
		s.buffers[i] = nil
	}
	s.buffers[0] = s.buffers[0][:0]
	s.lastBuffer = 0
}

// Bytes returns a copy of all the collected data
// Bytes는 수집된 모든 데이터의 복사본을 반환합니다.
func (s *LimitedReaderSlurper) Bytes() []byte {
	out := make([]byte, s.Size())
	offset := 0
	for i := 0; i <= s.lastBuffer; i++ {
		copy(out[offset:], s.buffers[i])
		offset += len(s.buffers[i])
	}
	return out
}

// allocateNextBuffer allocates the next buffer and places it in the buffers array.
// assignNextBuffer는 다음 버퍼를 할당하고 버퍼 배열에 배치합니다.
func (s *LimitedReaderSlurper) allocateNextBuffer() {
	s.lastBuffer++
	allocationSize := allocationStep
	if allocationSize > s.remainedUnallocatedSpace {
		allocationSize = s.remainedUnallocatedSpace
	}
	s.buffers[s.lastBuffer] = make([]byte, 0, allocationSize)
	s.remainedUnallocatedSpace -= allocationSize
}
