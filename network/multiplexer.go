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
	"sync/atomic"

	"github.com/Orca18/novarand/logging"
)

// Multiplexer is a message handler that sorts incoming messages by Tag and passes them along to the relevant message handler for that type of message.
// Multiplexer는 들어오는 메시지를 태그별로 분류하고 해당 메시지 유형에 대한 관련 메시지 처리기로 전달하는 메시지 처리기입니다.
// Multiplexer is a message handler that sorts incoming messages by Tag and passes
// them along to the relevant message handler for that type of message.
/*
	Tag에 따라 인커밍 메시지를 저장하는 핸들러이다. 메시지들을 타입에 따라 관련된 메시지 핸들러에게 전달한다.
*/
type Multiplexer struct {
	// stores map[Tag]MessageHandler, an immutable map.
	/*
		map[Tag]MessageHandler형태의 자료구조를 저장한다.
	*/
	msgHandlers atomic.Value

	log logging.Logger
}

// MakeMultiplexer creates an empty Multiplexer
// MakeMultiplexer는 빈 멀티플렉서를 생성합니다.
func MakeMultiplexer(log logging.Logger) *Multiplexer {
	m := &Multiplexer{
		log: log,
	}
	m.ClearHandlers([]Tag{}) // allocate the map
	return m
}

// getHandlersMap retrieves the handlers map.
// getHandlersMap은 핸들러 맵을 검색합니다.
func (m *Multiplexer) getHandlersMap() map[Tag]MessageHandler {
	handlersVal := m.msgHandlers.Load()
	if handlers, valid := handlersVal.(map[Tag]MessageHandler); valid {
		return handlers
	}
	return nil
}

// Retrives the handler for the given message Tag from the handlers array while taking a read lock.
// 읽기 잠금을 취하는 동안 핸들러 배열에서 주어진 메시지 태그에 대한 핸들러를 검색합니다.
func (m *Multiplexer) getHandler(tag Tag) (MessageHandler, bool) {
	if handlers := m.getHandlersMap(); handlers != nil {
		handler, ok := handlers[tag]
		return handler, ok
	}
	return nil, false
}

// Handle is the "input" side of the multiplexer. It dispatches the message to the previously defined handler.
// 핸들은 멀티플렉서의 "입력" 측입니다. 이전에 정의된 핸들러에 메시지를 전달합니다.
func (m *Multiplexer) Handle(msg IncomingMessage) OutgoingMessage {
	handler, ok := m.getHandler(msg.Tag)

	if ok {
		outmsg := handler.Handle(msg)
		return outmsg
	}
	return OutgoingMessage{}
}

// RegisterHandlers registers the set of given message handlers.
// RegisterHandlers는 주어진 메시지 핸들러 세트를 등록합니다.
func (m *Multiplexer) RegisterHandlers(dispatch []TaggedMessageHandler) {
	mp := make(map[Tag]MessageHandler)
	if existingMap := m.getHandlersMap(); existingMap != nil {
		for k, v := range existingMap {
			mp[k] = v
		}
	}
	for _, v := range dispatch {
		if _, has := mp[v.Tag]; has {
			m.log.Panicf("Already registered a handler for tag %v", v.Tag)
		}
		mp[v.Tag] = v.MessageHandler
	}
	m.msgHandlers.Store(mp)
}

// ClearHandlers deregisters all the existing message handlers other than the one provided in the excludeTags list
// ClearHandlers는 excludeTags 목록에 제공된 것 이외의 모든 기존 메시지 핸들러를 등록 취소합니다.
func (m *Multiplexer) ClearHandlers(excludeTags []Tag) {
	if len(excludeTags) == 0 {
		m.msgHandlers.Store(make(map[Tag]MessageHandler))
		return
	}

	// convert into map, so that we can exclude duplicates.
	// 중복을 제외할 수 있도록 맵으로 변환합니다.
	excludeTagsMap := make(map[Tag]bool)
	for _, tag := range excludeTags {
		excludeTagsMap[tag] = true
	}

	currentHandlersMap := m.getHandlersMap()
	newMap := make(map[Tag]MessageHandler, len(excludeTagsMap))
	for tag, handler := range currentHandlersMap {
		if excludeTagsMap[tag] {
			newMap[tag] = handler
		}
	}

	m.msgHandlers.Store(newMap)
}
