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

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/algorand/go-codec/codec"
	"github.com/algorand/msgp/msgp"
)

// ErrInvalidObject is used to state that an object decoding has failed because it's invalid.
// ErrInvalidObject는 유효하지 않기 때문에 객체 디코딩이 실패했음을 나타내는 데 사용됩니다.
var ErrInvalidObject = errors.New("unmarshalled object is invalid")

// CodecHandle is used to instantiate msgpack encoders and decoders with our settings (canonical, paranoid about decoding errors)
// CodecHandle은 우리의 설정으로 msgpack 인코더 및 디코더를 인스턴스화하는 데 사용됩니다(표준, 디코딩 오류에 대한 편집증).
var CodecHandle *codec.MsgpackHandle

// JSONHandle is used to instantiate JSON encoders and decoders with our settings (canonical, paranoid about decoding errors)
// JSONHandle은 설정을 사용하여 JSON 인코더 및 디코더를 인스턴스화하는 데 사용됩니다(표준, 디코딩 오류에 대한 편집증).
var JSONHandle *codec.JsonHandle

// JSONStrictHandle is the same as JSONHandle but with MapKeyAsString=true for correct maps[int]interface{} encoding
// JSONStrictHandle은 JSONHandle과 동일하지만 올바른 maps[int]interface{} 인코딩을 위해 MapKeyAsString=true를 사용합니다.
var JSONStrictHandle *codec.JsonHandle

// Decoder is our interface for a thing that can decode objects.
// 디코더는 객체를 디코딩할 수 있는 인터페이스입니다.
type Decoder interface {
	Decode(objptr interface{}) error
}

func init() {
	CodecHandle = new(codec.MsgpackHandle)
	CodecHandle.ErrorIfNoField = true
	CodecHandle.ErrorIfNoArrayExpand = true
	CodecHandle.Canonical = true
	CodecHandle.RecursiveEmptyCheck = true
	CodecHandle.WriteExt = true
	CodecHandle.PositiveIntUnsigned = true
	CodecHandle.Raw = true

	JSONHandle = new(codec.JsonHandle)
	JSONHandle.ErrorIfNoField = true
	JSONHandle.ErrorIfNoArrayExpand = true
	JSONHandle.Canonical = true
	JSONHandle.RecursiveEmptyCheck = true
	JSONHandle.Indent = 2
	JSONHandle.HTMLCharsAsIs = true

	JSONStrictHandle = new(codec.JsonHandle)
	JSONStrictHandle.ErrorIfNoField = JSONHandle.ErrorIfNoField
	JSONStrictHandle.ErrorIfNoArrayExpand = JSONHandle.ErrorIfNoArrayExpand
	JSONStrictHandle.Canonical = JSONHandle.Canonical
	JSONStrictHandle.RecursiveEmptyCheck = JSONHandle.RecursiveEmptyCheck
	JSONStrictHandle.Indent = JSONHandle.Indent
	JSONStrictHandle.HTMLCharsAsIs = JSONHandle.HTMLCharsAsIs
	JSONStrictHandle.MapKeyAsString = true
}

type codecBytes struct {
	enc *codec.Encoder

	// Reuse this slice variable so that we don't have to allocate a fresh slice object (runtime.newobject), separate from allocating the slice payload (runtime.makeslice).
	// 슬라이스 페이로드(runtime.makeslice) 할당과 별도로 새로운 슬라이스 객체(runtime.newobject)를 할당할 필요가 없도록 이 슬라이스 변수를 재사용합니다.
	buf []byte
}

var codecBytesPool = sync.Pool{
	New: func() interface{} {
		return &codecBytes{
			enc: codec.NewEncoderBytes(nil, CodecHandle),
		}
	},
}

var codecStreamPool = sync.Pool{
	New: func() interface{} {
		return codec.NewEncoder(nil, CodecHandle)
	},
}

const initEncodeBufSize = 256

// EncodeReflect returns a msgpack-encoded byte buffer for a given object, using reflection.
// EncodeReflect는 리플렉션을 사용하여 주어진 객체에 대해 msgpack으로 인코딩된 바이트 버퍼를 반환합니다.
func EncodeReflect(obj interface{}) []byte {
	codecBytes := codecBytesPool.Get().(*codecBytes)
	codecBytes.buf = make([]byte, initEncodeBufSize)
	codecBytes.enc.ResetBytes(&codecBytes.buf)
	codecBytes.enc.MustEncode(obj)
	res := codecBytes.buf
	// Don't use defer because it incurs a non-trivial overhead
	// for encoding small objects.  If MustEncode panics, we will
	// let the GC deal with the codecBytes object.
	codecBytesPool.Put(codecBytes)
	return res
}

// EncodeMsgp returns a msgpack-encoded byte buffer, requiring that we pre-generated the code for doing so using msgp.
// EncodeMsgp는 msgpack으로 인코딩된 바이트 버퍼를 반환하므로 msgp를 사용하여 코드를 미리 생성해야 합니다.
func EncodeMsgp(obj msgp.Marshaler) []byte {
	return obj.MarshalMsg(nil)
}

// Encode returns a msgpack-encoded byte buffer for a given object.
// Encode는 주어진 객체에 대해 msgpack으로 인코딩된 바이트 버퍼를 반환합니다.
func Encode(obj msgp.Marshaler) []byte {
	if obj.CanMarshalMsg(obj) {
		return EncodeMsgp(obj)
	}

	// Use fmt instead of logging to avoid import loops; the expectation is that this should never happen.
	// 가져오기 루프를 피하기 위해 로깅 대신 fmt를 사용합니다. 이런 일이 절대 일어나서는 안 될 것으로 예상됩니다.
	fmt.Fprintf(os.Stderr, "Encoding %T using go-codec; stray embedded field?\n", obj)
	return EncodeReflect(obj)
}

// EncodeStream is like Encode but writes to an io.Writer instead.
// EncodeStream은 Encode와 비슷하지만 대신 io.Writer에 씁니다.
func EncodeStream(w io.Writer, obj interface{}) {
	enc := codecStreamPool.Get().(*codec.Encoder)
	enc.Reset(w)
	enc.MustEncode(obj)
	// Don't use defer because it incurs a non-trivial overhead for encoding small objects
	// If MustEncode panics, we will let the GC deal with the enc object.
	// defer를 사용하면 작은 개체를 인코딩하는 데 적지 않은 오버헤드가 발생하므로 사용하지 마십시오.
	// MustEncode가 패닉하면 GC가 enc 객체를 처리하도록 합니다.
	codecStreamPool.Put(enc)
}

// EncodeJSON returns a JSON-encoded byte buffer for a given object
// EncodeJSON은 주어진 객체에 대해 JSON으로 인코딩된 바이트 버퍼를 반환합니다.
func EncodeJSON(obj interface{}) []byte {
	var b []byte
	enc := codec.NewEncoderBytes(&b, JSONHandle)
	enc.MustEncode(obj)
	return b
}

// EncodeJSONStrict returns a JSON-encoded byte buffer for a given object
// It is the same EncodeJSON but encodes map's int keys as strings
// EncodeJSONStrict는 주어진 객체에 대해 JSON으로 인코딩된 바이트 버퍼를 반환합니다.
// 동일한 EncodeJSON이지만 맵의 int 키를 문자열로 인코딩합니다.
func EncodeJSONStrict(obj interface{}) []byte {
	var b []byte
	enc := codec.NewEncoderBytes(&b, JSONStrictHandle)
	enc.MustEncode(obj)
	return b
}

// DecodeReflect attempts to decode a msgpack-encoded byte buffer into an object instance pointed to by objptr, using reflection.
// DecodeReflect는 리플렉션을 사용하여 msgpack으로 인코딩된 바이트 버퍼를 objptr이 가리키는 개체 인스턴스로 디코딩하려고 시도합니다.
func DecodeReflect(b []byte, objptr interface{}) error {
	dec := codec.NewDecoderBytes(b, CodecHandle)
	return dec.Decode(objptr)
}

// DecodeMsgp attempts to decode a msgpack-encoded byte buffer into an object instance pointed to by objptr, requiring that we pre- generated the code for doing so using msgp.
// DecodeMsgp는 msgpack으로 인코딩된 바이트 버퍼를 objptr이 가리키는 개체 인스턴스로 디코딩하려고 시도하므로 msgp를 사용하여 코드를 미리 생성해야 합니다.
func DecodeMsgp(b []byte, objptr msgp.Unmarshaler) (err error) {
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("DecodeMsgp: %v", x)
		}
	}()

	var rem []byte
	rem, err = objptr.UnmarshalMsg(b)
	if err != nil {
		return err
	}

	// go-codec compat: allow remaining bytes, because go-codec allows it too
	// go-codec compat: go-codec도 허용하므로 나머지 바이트 허용
	if false {
		if len(rem) != 0 {
			return fmt.Errorf("decoding left %d remaining bytes", len(rem))
		}
	}

	return nil
}

// Decode attempts to decode a msgpack-encoded byte buffer into an object instance pointed to by objptr.
// Decode는 msgpack으로 인코딩된 바이트 버퍼를 objptr이 가리키는 개체 인스턴스로 디코딩하려고 시도합니다.
func Decode(b []byte, objptr msgp.Unmarshaler) error {
	if objptr.CanUnmarshalMsg(objptr) {
		return DecodeMsgp(b, objptr)
	}

	// Use fmt instead of logging to avoid import loops;
	// the expectation is that this should never happen.
	fmt.Fprintf(os.Stderr, "Decoding %T using go-codec; stray embedded field?\n", objptr)

	return DecodeReflect(b, objptr)
}

// DecodeStream is like Decode but reads from an io.Reader instead.
// DecodeStream은 Decode와 비슷하지만 대신 io.Reader에서 읽습니다.
func DecodeStream(r io.Reader, objptr interface{}) error {
	dec := codec.NewDecoder(r, CodecHandle)
	return dec.Decode(objptr)
}

// DecodeJSON attempts to decode a JSON-encoded byte buffer into an object instance pointed to by objptr
// DecodeJSON은 JSON으로 인코딩된 바이트 버퍼를 objptr이 가리키는 객체 인스턴스로 디코딩하려고 시도합니다.
func DecodeJSON(b []byte, objptr interface{}) error {
	dec := codec.NewDecoderBytes(b, JSONHandle)
	return dec.Decode(objptr)
}

// NewEncoder returns an encoder object writing bytes into [w].
// NewEncoder는 [w]에 바이트를 쓰는 인코더 객체를 반환합니다.
func NewEncoder(w io.Writer) *codec.Encoder {
	return codec.NewEncoder(w, CodecHandle)
}

// NewJSONEncoder returns an encoder object writing bytes into [w].
func NewJSONEncoder(w io.Writer) *codec.Encoder {
	return codec.NewEncoder(w, JSONHandle)
}

// NewDecoder returns a decoder object reading bytes from [r].
func NewDecoder(r io.Reader) Decoder {
	return codec.NewDecoder(r, CodecHandle)
}

// NewJSONDecoder returns a json decoder object reading bytes from [r].
func NewJSONDecoder(r io.Reader) Decoder {
	return codec.NewDecoder(r, JSONHandle)
}

// NewDecoderBytes returns a decoder object reading bytes from [b].
func NewDecoderBytes(b []byte) Decoder {
	return codec.NewDecoderBytes(b, CodecHandle)
}

// encodingPool holds temporary byte slice buffers used for encoding messages.
// encodingPool은 메시지 인코딩에 사용되는 임시 바이트 슬라이스 버퍼를 보유합니다.
var encodingPool = sync.Pool{
	New: func() interface{} {
		return []byte{}
	},
}

// GetEncodingBuf returns a byte slice that can be used for encoding a temporary message.
// The byte slice has zero length but potentially non-zero capacity.
// The caller gets full ownership of the byte slice, but is encouraged to return it using PutEncodingBuf().
// GetEncodingBuf는 임시 메시지를 인코딩하는 데 사용할 수 있는 바이트 슬라이스를 반환합니다.
// 바이트 슬라이스의 길이는 0이지만 잠재적으로 0이 아닌 용량을 갖습니다.
// 호출자는 바이트 슬라이스의 전체 소유권을 갖지만 PutEncodingBuf()를 사용하여 반환하는 것이 좋습니다.
func GetEncodingBuf() []byte {
	return encodingPool.Get().([]byte)[:0]
}

// PutEncodingBuf places a byte slice into the pool of temporary buffers for encoding.
// The caller gives up ownership of the byte slice when passing it to PutEncodingBuf().
// PutEncodingBuf는 인코딩을 위해 임시 버퍼 풀에 바이트 슬라이스를 배치합니다.
// 호출자는 바이트 슬라이스를 PutEncodingBuf()에 전달할 때 바이트 슬라이스의 소유권을 포기합니다.
func PutEncodingBuf(s []byte) {
	encodingPool.Put(s)
}
