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
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Orca18/novarand/test/partitiontest"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/msgp/msgp"
	"github.com/stretchr/testify/require"
)

const debugCodecTester = false

type msgpMarshalUnmarshal interface {
	msgp.Marshaler
	msgp.Unmarshaler
}

var rawMsgpType = reflect.TypeOf(msgp.Raw{})
var errSkipRawMsgpTesting = fmt.Errorf("skipping msgp.Raw serializing, since it won't be the same across go-codec and msgp")

func oneOf(n int) bool {
	return (rand.Int() % n) == 0
}

// RandomizeObject returns a random object of the same type as template
// RandomizeObject는 템플릿과 동일한 유형의 임의의 개체를 반환합니다.
func RandomizeObject(template interface{}) (interface{}, error) {
	tt := reflect.TypeOf(template)
	if tt.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("RandomizeObject: must be ptr")
	}
	v := reflect.New(tt.Elem())
	err := randomizeValue(v.Elem(), tt.String(), "")
	return v.Interface(), err
}

func parseStructTags(structTag string) map[string]string {
	tagsMap := map[string]string{}

	for _, tag := range strings.Split(reflect.StructTag(structTag).Get("codec"), ",") {
		elements := strings.Split(tag, "=")
		if len(elements) != 2 {
			continue
		}
		tagsMap[elements[0]] = elements[1]
	}
	return tagsMap
}

var printWarningOnce deadlock.Mutex
var warningMessages map[string]bool

func printWarning(warnMsg string) {
	printWarningOnce.Lock()
	defer printWarningOnce.Unlock()
	if warningMessages == nil {
		warningMessages = make(map[string]bool)
	}
	if !warningMessages[warnMsg] {
		warningMessages[warnMsg] = true
		fmt.Printf("%s\n", warnMsg)
	}
}

var testedDatatypesForAllocBound = map[string]bool{}
var testedDatatypesForAllocBoundMu = deadlock.Mutex{}

func checkMsgpAllocBoundDirective(dataType reflect.Type) bool {
	// does any of the go files in the package directory has the msgp:allocbound defined for that datatype ?
	// 패키지 디렉토리의 go 파일에 해당 데이터 유형에 대해 정의된 msgp:allocbound가 있습니까?
	gopath := os.Getenv("GOPATH")
	const repositoryRoot = "go-algorand/"
	const thisFile = "protocol/codec_tester.go"
	packageFilesPath := path.Join(gopath, "src", dataType.PkgPath())

	if _, err := os.Stat(packageFilesPath); os.IsNotExist(err) {
		// no such directory. Try to assemble the path based on the current working directory.
		// 그러한 디렉토리가 없습니다. 현재 작업 디렉토리를 기반으로 경로를 조합해 보십시오.
		cwd, err := os.Getwd()
		if err != nil {
			return false
		}
		if cwdPaths := strings.SplitAfter(cwd, repositoryRoot); len(cwdPaths) == 2 {
			cwd = cwdPaths[0]
		} else {
			// try to assemble the project directory based on the current stack frame
			// 현재 스택 프레임을 기반으로 프로젝트 디렉토리를 어셈블하려고 시도합니다.
			_, file, _, ok := runtime.Caller(0)
			if !ok {
				return false
			}
			cwd = strings.TrimSuffix(file, thisFile)
		}

		relPkdPath := strings.SplitAfter(dataType.PkgPath(), repositoryRoot)
		if len(relPkdPath) != 2 {
			return false
		}
		packageFilesPath = path.Join(cwd, relPkdPath[1])
		if _, err := os.Stat(packageFilesPath); os.IsNotExist(err) {
			return false
		}
	}
	packageFiles := []string{}
	filepath.Walk(packageFilesPath, func(path string, info os.FileInfo, err error) error {
		if filepath.Ext(path) == ".go" {
			packageFiles = append(packageFiles, path)
		}
		return nil
	})
	for _, packageFile := range packageFiles {
		fileBytes, err := ioutil.ReadFile(packageFile)
		if err != nil {
			continue
		}
		if strings.Index(string(fileBytes), fmt.Sprintf("msgp:allocbound %s", dataType.Name())) != -1 {
			// message pack alloc bound definition was found.
			return true
		}
	}
	return false
}

func checkBoundsLimitingTag(val reflect.Value, datapath string, structTag string) (hasAllocBound bool) {
	var objType string
	if val.Kind() == reflect.Slice {
		objType = "slice"
	} else if val.Kind() == reflect.Map {
		objType = "map"
	}

	if structTag != "" {
		tagsMap := parseStructTags(structTag)

		if tagsMap["allocbound"] == "-" {
			printWarning(fmt.Sprintf("%s %s have an unbounded allocbound defined", objType, datapath))
			return
		}

		if _, have := tagsMap["allocbound"]; have {
			hasAllocBound = true
			testedDatatypesForAllocBoundMu.Lock()
			defer testedDatatypesForAllocBoundMu.Unlock()
			if val.Type().Name() == "" {
				testedDatatypesForAllocBound[datapath] = true
			} else {
				testedDatatypesForAllocBound[val.Type().Name()] = true
			}
			return
		}
	}
	// no struct tag, or have a struct tag with no allocbound.
	// 구조체 태그가 없거나 allocbound가 없는 구조체 태그가 있습니다.
	if val.Type().Name() != "" {
		testedDatatypesForAllocBoundMu.Lock()
		var exists bool
		hasAllocBound, exists = testedDatatypesForAllocBound[val.Type().Name()]
		testedDatatypesForAllocBoundMu.Unlock()
		if !exists {
			// does any of the go files in the package directory has the msgp:allocbound defined for that datatype ?
			// 패키지 디렉토리의 go 파일에 해당 데이터 유형에 대해 정의된 msgp:allocbound가 있습니까?
			hasAllocBound = checkMsgpAllocBoundDirective(val.Type())
			testedDatatypesForAllocBoundMu.Lock()
			testedDatatypesForAllocBound[val.Type().Name()] = hasAllocBound
			testedDatatypesForAllocBoundMu.Unlock()
			return
		} else if hasAllocBound {
			return
		}
	}

	if val.Type().Kind() == reflect.Slice || val.Type().Kind() == reflect.Map || val.Type().Kind() == reflect.Array {
		printWarning(fmt.Sprintf("%s %s does not have an allocbound defined for %s %s", objType, datapath, val.Type().String(), val.Type().PkgPath()))
	}
	return
}

func randomizeValue(v reflect.Value, datapath string, tag string) error {
	if oneOf(5) {
		// Leave zero value
		return nil
	}

	/* Consider cutting off recursive structures by stopping at some datapath depth.
	일부 데이터 경로 깊이에서 중지하여 재귀 구조를 차단하는 것을 고려하십시오.

	    if len(datapath) > 200 {
			// Cut off recursive structures
			return nil
		}
	*/

	switch v.Kind() {
	case reflect.Uint, reflect.Uintptr, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(rand.Uint64())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(rand.Uint64()))
	case reflect.String:
		var buf []byte
		len := rand.Int() % 64
		for i := 0; i < len; i++ {
			buf = append(buf, byte(rand.Uint32()))
		}
		v.SetString(string(buf))
	case reflect.Struct:
		st := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := st.Field(i)
			tag := f.Tag

			if f.PkgPath != "" && !f.Anonymous {
				// unexported
				continue
			}
			if rawMsgpType == f.Type {
				return errSkipRawMsgpTesting
			}
			err := randomizeValue(v.Field(i), datapath+"/"+f.Name, string(tag))
			if err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			err := randomizeValue(v.Index(i), fmt.Sprintf("%s/%d", datapath, i), "")
			if err != nil {
				return err
			}
		}
	case reflect.Slice:
		// we don't want to allocate a slice with size of 0. This is because decoding and encoding this slice will result in nil and not slice of size 0
		// 우리는 크기가 0인 슬라이스를 할당하고 싶지 않습니다. 이것은 이 슬라이스를 디코딩하고 인코딩하면 크기가 0이 아닌 nil이 되기 때문입니다.
		l := rand.Int()%31 + 1

		hasAllocBound := checkBoundsLimitingTag(v, datapath, tag)
		if hasAllocBound {
			l = 1
		}
		s := reflect.MakeSlice(v.Type(), l, l)
		for i := 0; i < l; i++ {
			err := randomizeValue(s.Index(i), fmt.Sprintf("%s/%d", datapath, i), "")
			if err != nil {
				return err
			}
		}
		v.Set(s)
	case reflect.Bool:
		v.SetBool(rand.Uint32()%2 == 0)
	case reflect.Map:
		hasAllocBound := checkBoundsLimitingTag(v, datapath, tag)
		mt := v.Type()
		v.Set(reflect.MakeMap(mt))
		l := rand.Int() % 32
		if hasAllocBound {
			l = 1
		}
		for i := 0; i < l; i++ {
			mk := reflect.New(mt.Key())
			err := randomizeValue(mk.Elem(), fmt.Sprintf("%s/%d", datapath, i), "")
			if err != nil {
				return err
			}

			mv := reflect.New(mt.Elem())
			err = randomizeValue(mv.Elem(), fmt.Sprintf("%s/%d", datapath, i), "")
			if err != nil {
				return err
			}

			v.SetMapIndex(mk.Elem(), mv.Elem())
		}
	default:
		return fmt.Errorf("unsupported object kind %v", v.Kind())
	}
	return nil
}

// EncodingTest tests that our two msgpack codecs (msgp and go-codec) agree on encodings and decodings of random values of the type of template, returning an error if there is a mismatch.
// EncodingTest는 두 개의 msgpack 코덱(msgp 및 go-codec)이 템플릿 유형의 임의 값의 인코딩 및 디코딩에 동의하는지 테스트하고 불일치가 있으면 오류를 반환합니다.
func EncodingTest(template msgpMarshalUnmarshal) error {
	v0, err := RandomizeObject(template)
	if err != nil {
		return err
	}

	if debugCodecTester {
		ioutil.WriteFile("/tmp/v0", []byte(fmt.Sprintf("%#v", v0)), 0666)
	}

	e1 := EncodeMsgp(v0.(msgp.Marshaler))
	e2 := EncodeReflect(v0)

	// for debug, write out the encodings to a file
	// 디버그의 경우 인코딩을 파일에 기록합니다.
	if debugCodecTester {
		ioutil.WriteFile("/tmp/e1", e1, 0666)
		ioutil.WriteFile("/tmp/e2", e2, 0666)
	}

	if !reflect.DeepEqual(e1, e2) {
		return fmt.Errorf("encoding mismatch for %v: %v != %v", v0, e1, e2)
	}

	v1 := reflect.New(reflect.TypeOf(template).Elem()).Interface().(msgpMarshalUnmarshal)
	v2 := reflect.New(reflect.TypeOf(template).Elem()).Interface().(msgpMarshalUnmarshal)

	err = DecodeMsgp(e1, v1)
	if err != nil {
		return err
	}

	err = DecodeReflect(e1, v2)
	if err != nil {
		return err
	}

	if debugCodecTester {
		ioutil.WriteFile("/tmp/v1", []byte(fmt.Sprintf("%#v", v1)), 0666)
		ioutil.WriteFile("/tmp/v2", []byte(fmt.Sprintf("%#v", v2)), 0666)
	}

	// At this point, it might be that v differs from v1 and v2, because there are multiple representations
	// 이 시점에서 v는 여러 표현이 있기 때문에 v1 및 v2와 다를 수 있습니다.
	// (e.g., an empty byte slice could be either nil or a zero-length slice).
	// (예를 들어, 빈 바이트 슬라이스는 nil이거나 길이가 0인 슬라이스일 수 있음).
	// But we require that the msgp codec match the behavior of go-codec.
	// 하지만 msgp 코덱이 go-codec의 동작과 일치해야 합니다.

	if !reflect.DeepEqual(v1, v2) {
		return fmt.Errorf("decoding mismatch")
	}

	// Finally, check that the value encodes back to the same encoding.
	// 마지막으로 값이 동일한 인코딩으로 다시 인코딩되는지 확인합니다.

	ee1 := EncodeMsgp(v1)
	ee2 := EncodeReflect(v1)

	if debugCodecTester {
		ioutil.WriteFile("/tmp/ee1", ee1, 0666)
		ioutil.WriteFile("/tmp/ee2", ee2, 0666)
	}

	if !reflect.DeepEqual(e1, ee1) {
		return fmt.Errorf("re-encoding mismatch: e1 != ee1")
	}
	if !reflect.DeepEqual(e1, ee2) {
		return fmt.Errorf("re-encoding mismatch: e1 != ee2")
	}

	return nil
}

// RunEncodingTest runs several iterations of encoding/decoding consistency testing of object type specified by template.
// RunEncodingTest는 템플릿에 의해 지정된 객체 유형의 인코딩/디코딩 일관성 테스트의 여러 반복을 실행합니다.
func RunEncodingTest(t *testing.T, template msgpMarshalUnmarshal) {
	partitiontest.PartitionTest(t)
	for i := 0; i < 1000; i++ {
		err := EncodingTest(template)
		if err == errSkipRawMsgpTesting {
			// we want to skip the serilization test in this case.
			// 이 경우 직렬화 테스트를 건너뛰고 싶습니다.
			t.Skip()
			return
		}
		if err == nil {
			continue
		}

		// some objects might appen to the original error additional info.
		// 일부 객체는 원래 오류 추가 정보에 추가될 수 있습니다.
		// we ensure that invalidObject error is not failing the test.
		// invalidObject 오류가 테스트에 실패하지 않았는지 확인합니다.
		if errors.As(err, &ErrInvalidObject) {
			continue
		}
		require.NoError(t, err)
	}
}
