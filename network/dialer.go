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
	"context"
	"net"
	"time"

	"github.com/algorand/go-algorand/tools/network/dnssec"
)

type netDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Dialer establish tcp-level connection with the destination
// 다이얼러는 대상과 tcp 수준 연결을 설정합니다.
type Dialer struct {
	phonebook   Phonebook
	innerDialer netDialer
	resolver    *net.Resolver
}

// makeRateLimitingDialer creates a rate limiting dialer that would limit the connections according to the entries in the phonebook.
// makeRateLimitingDialer는 전화번호부의 항목에 따라 연결을 제한하는 속도 제한 다이얼러를 생성합니다.
func makeRateLimitingDialer(phonebook Phonebook, resolver dnssec.ResolverIf) Dialer {
	var innerDialer netDialer = &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}

	// if a DNSSEC-aware resolver provided, use a wrapping dnssec.Dialer to parse addr, resolve it securely and call a regular net.Dialer
	// DNSSEC 인식 해석기가 제공된 경우 래핑 dnssec.Dialer를 사용하여 addr을 구문 분석하고 안전하게 해석하고 일반 net.Dialer를 호출합니다.
	if resolver != nil {
		innerDialer = &dnssec.Dialer{
			InnerDialer: innerDialer.(*net.Dialer),
			Resolver:    resolver,
		}
	}

	return Dialer{
		phonebook:   phonebook,
		innerDialer: innerDialer,
	}
}

// Dial connects to the address on the named network.
// 다이얼은 명명된 네트워크의 주소에 연결합니다.
// It waits if needed not to exceed connectionsRateLimitingCount.
// 필요하면 connectionsRateLimitingCount를 초과하지 않도록 기다립니다.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext connects to the address on the named network using the provided context.
// DialContext는 제공된 컨텍스트를 사용하여 명명된 네트워크의 주소에 연결합니다.
// It waits if needed not to exceed connectionsRateLimitingCount.
// 필요하면 connectionsRateLimitingCount를 초과하지 않도록 기다립니다.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var waitTime time.Duration
	var provisionalTime time.Time

	for {
		_, waitTime, provisionalTime = d.phonebook.GetConnectionWaitTime(address)
		if waitTime == 0 {
			break // break out of the loop and proceed to the connection
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitTime):
		}
	}
	conn, err := d.innerDialContext(ctx, network, address)
	d.phonebook.UpdateConnectionTime(address, provisionalTime)

	return conn, err
}

func (d *Dialer) innerDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// this would be a good place to have the dnssec evaluated.
	return d.innerDialer.DialContext(ctx, network, address)
}
