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
	"net/http"
	"time"
)

// rateLimitingTransport is the transport for execute a single HTTP transaction, obtaining the Response for a given Request.
// rateLimitingTransport는 단일 HTTP 트랜잭션을 실행하여 주어진 요청에 대한 응답을 얻기 위한 전송입니다.
type rateLimitingTransport struct {
	phonebook       Phonebook
	innerTransport  *http.Transport
	queueingTimeout time.Duration
}

// ErrConnectionQueueingTimeout indicates that we've exceeded the time allocated for queueing the current request before the request attempt could be made.
// ErrConnectionQueueingTimeout은 요청 시도가 이루어지기 전에 현재 요청을 대기열에 넣는 데 할당된 시간을 초과했음을 나타냅니다.
var ErrConnectionQueueingTimeout = errors.New("rateLimitingTransport: queueing timeout")

// makeRateLimitingTransport creates a rate limiting http transport that would limit the requests rate according to the entries in the phonebook.
// makeRateLimitingTransport는 폰북의 항목에 따라 요청 속도를 제한하는 속도 제한 http 전송을 생성합니다.
func makeRateLimitingTransport(phonebook Phonebook, queueingTimeout time.Duration, dialer *Dialer, maxIdleConnsPerHost int) rateLimitingTransport {
	defaultTransport := http.DefaultTransport.(*http.Transport)
	return rateLimitingTransport{
		phonebook: phonebook,
		innerTransport: &http.Transport{
			Proxy:                 defaultTransport.Proxy,
			DialContext:           dialer.innerDialContext,
			MaxIdleConns:          defaultTransport.MaxIdleConns,
			IdleConnTimeout:       defaultTransport.IdleConnTimeout,
			TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout,
			ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		},
		queueingTimeout: queueingTimeout,
	}
}

// RoundTrip connects to the address on the named network using the provided context.
// RoundTrip은 제공된 컨텍스트를 사용하여 명명된 네트워크의 주소에 연결합니다.
// It waits if needed not to exceed connectionsRateLimitingCount.
// 필요하면 connectionsRateLimitingCount를 초과하지 않도록 기다립니다.
func (r *rateLimitingTransport) RoundTrip(req *http.Request) (res *http.Response, err error) {
	var waitTime time.Duration
	var provisionalTime time.Time
	queueingTimedOut := time.After(r.queueingTimeout)
	for {
		_, waitTime, provisionalTime = r.phonebook.GetConnectionWaitTime(req.Host)
		if waitTime == 0 {
			break // break out of the loop and proceed to the connection
		}
		select {
		case <-time.After(waitTime):
		case <-queueingTimedOut:
			return nil, ErrConnectionQueueingTimeout
		}
	}
	res, err = r.innerTransport.RoundTrip(req)
	r.phonebook.UpdateConnectionTime(req.Host, provisionalTime)
	return
}
