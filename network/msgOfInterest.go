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
	"strings"

	"github.com/Orca18/novarand/protocol"
)

var errUnableUnmarshallMessage = errors.New("unmarshalMessageOfInterest: could not unmarshall message")
var errInvalidMessageOfInterest = errors.New("unmarshalMessageOfInterest: message missing the tags key")
var errInvalidMessageOfInterestLength = errors.New("unmarshalMessageOfInterest: message length is too long")

const maxMessageOfInterestTags = 1024
const topicsEncodingSeparator = ","

func unmarshallMessageOfInterest(data []byte) (map[protocol.Tag]bool, error) {
	// decode the message, and ensure it's a valid message.
	// 메시지를 디코딩하고 유효한 메시지인지 확인합니다.
	topics, err := UnmarshallTopics(data)
	if err != nil {
		return nil, errUnableUnmarshallMessage
	}
	tags, found := topics.GetValue("tags")
	if !found {
		return nil, errInvalidMessageOfInterest
	}
	if len(tags) > maxMessageOfInterestTags {
		return nil, errInvalidMessageOfInterestLength
	}
	// convert the tags into a tags map.
	// 태그를 태그 맵으로 변환합니다.
	msgTagsMap := make(map[protocol.Tag]bool, len(tags))
	for _, tag := range strings.Split(string(tags), topicsEncodingSeparator) {
		msgTagsMap[protocol.Tag(tag)] = true
	}
	return msgTagsMap, nil
}

// MarshallMessageOfInterest generate a message of interest message body for a given set of message tags.
// MarshallMessageOfInterest는 주어진 메시지 태그 세트에 대한 관심 메시지 본문을 생성합니다.
func MarshallMessageOfInterest(messageTags []protocol.Tag) []byte {
	// create a long string with all these messages.
	// 이 모든 메시지가 포함된 긴 문자열을 만듭니다.
	tags := ""
	for _, tag := range messageTags {
		tags += topicsEncodingSeparator + string(tag)
	}
	if len(tags) > 0 {
		tags = tags[len(topicsEncodingSeparator):]
	}
	topics := Topics{Topic{key: "tags", data: []byte(tags)}}
	return topics.MarshallTopics()
}

// MarshallMessageOfInterestMap generates a message of interest message body for the message tags that map to "true" in the map argument.
// MarshallMessageOfInterestMap은 map 인수에서 "true"로 매핑되는 메시지 태그에 대한 관심 메시지 메시지 본문을 생성합니다.
func MarshallMessageOfInterestMap(tagmap map[protocol.Tag]bool) []byte {
	tags := ""
	for tag, flag := range tagmap {
		if flag {
			tags += topicsEncodingSeparator + string(tag)
		}
	}
	if len(tags) > 0 {
		tags = tags[len(topicsEncodingSeparator):]
	}
	topics := Topics{Topic{key: "tags", data: []byte(tags)}}
	return topics.MarshallTopics()
}
