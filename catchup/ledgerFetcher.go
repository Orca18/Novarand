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

package catchup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/ledger"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/network"
	"github.com/Orca18/novarand/rpcs"
	"github.com/Orca18/novarand/util"
)

var errNoLedgerForRound = errors.New("No ledger available for given round")

const (
	// maxCatchpointFileChunkSize is a rough estimate for the worst-case scenario we're going to have of all the accounts data per a single catchpoint file chunk.
	// maxCatchpointFileChunkSize는 단일 catchpoint 파일 청크당 모든 계정 데이터를 갖게 될 최악의 시나리오에 대한 대략적인 추정치입니다.
	maxCatchpointFileChunkSize = ledger.BalancesPerCatchpointFileChunk * basics.MaxEncodedAccountDataSize
	// defaultMinCatchpointFileDownloadBytesPerSecond defines the worst-case scenario download speed we expect to get while downloading a catchpoint file
	// defaultMinCatchpointFileDownloadBytesPerSecond는 캐치포인트 파일을 다운로드하는 동안 얻을 것으로 예상되는 최악의 시나리오 다운로드 속도를 정의합니다.
	defaultMinCatchpointFileDownloadBytesPerSecond = 20 * 1024
	// catchpointFileStreamReadSize defines the number of bytes we would attempt to read at each itration from the incoming http data stream
	// catchpointFileStreamReadSize는 들어오는 http 데이터 스트림에서 각 반복에서 읽으려고 시도하는 바이트 수를 정의합니다.
	catchpointFileStreamReadSize = 4096
)

var errNonHTTPPeer = fmt.Errorf("downloadLedger : non-HTTPPeer encountered")

type ledgerFetcherReporter interface {
	updateLedgerFetcherProgress(*ledger.CatchpointCatchupAccessorProgress)
}

type ledgerFetcher struct {
	net      network.GossipNode
	accessor ledger.CatchpointCatchupAccessor
	log      logging.Logger

	reporter ledgerFetcherReporter
	config   config.Local
}

func makeLedgerFetcher(net network.GossipNode, accessor ledger.CatchpointCatchupAccessor, log logging.Logger, reporter ledgerFetcherReporter, cfg config.Local) *ledgerFetcher {
	return &ledgerFetcher{
		net:      net,
		accessor: accessor,
		log:      log,
		reporter: reporter,
		config:   cfg,
	}
}

func (lf *ledgerFetcher) downloadLedger(ctx context.Context, peer network.Peer, round basics.Round) error {
	httpPeer, ok := peer.(network.HTTPPeer)
	if !ok {
		return errNonHTTPPeer
	}
	return lf.getPeerLedger(ctx, httpPeer, round)
}

func (lf *ledgerFetcher) getPeerLedger(ctx context.Context, peer network.HTTPPeer, round basics.Round) error {
	parsedURL, err := network.ParseHostOrURL(peer.GetAddress())
	if err != nil {
		return err
	}

	parsedURL.Path = lf.net.SubstituteGenesisID(path.Join(parsedURL.Path, "/v1/{genesisID}/ledger/"+strconv.FormatUint(uint64(round), 36)))
	ledgerURL := parsedURL.String()
	lf.log.Debugf("ledger GET %#v peer %#v %T", ledgerURL, peer, peer)
	request, err := http.NewRequest(http.MethodGet, ledgerURL, nil)
	if err != nil {
		return err
	}

	timeoutContext, timeoutContextCancel := context.WithTimeout(ctx, lf.config.MaxCatchpointDownloadDuration)
	defer timeoutContextCancel()
	request = request.WithContext(timeoutContext)
	network.SetUserAgentHeader(request.Header)
	response, err := peer.GetHTTPClient().Do(request)
	if err != nil {
		lf.log.Debugf("getPeerLedger GET %v : %s", ledgerURL, err)
		return err
	}
	defer response.Body.Close()

	// check to see that we had no errors.
	// 오류가 없는지 확인합니다.
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound: // server could not find a block with that round numbers.
		return errNoLedgerForRound
	default:
		return fmt.Errorf("getPeerLedger error response status code %d", response.StatusCode)
	}

	// at this point, we've already receieved the response headers.
	// ensure that the response content type is what we'd like it to be.
	// 이 시점에서 우리는 이미 응답 헤더를 수신했습니다.
	// 응답 콘텐츠 유형이 우리가 원하는 것과 같은지 확인합니다.
	contentTypes := response.Header["Content-Type"]
	if len(contentTypes) != 1 {
		err = fmt.Errorf("getPeerLedger : http ledger fetcher invalid content type count %d", len(contentTypes))
		return err
	}

	if contentTypes[0] != rpcs.LedgerResponseContentType {
		err = fmt.Errorf("getPeerLedger : http ledger fetcher response has an invalid content type : %s", contentTypes[0])
		return err
	}

	// maxCatchpointFileChunkDownloadDuration is the maximum amount of time we would wait to download a single chunk off a catchpoint file
	// maxCatchpointFileChunkDownloadDuration은 캐치포인트 파일에서 단일 청크를 다운로드하기 위해 대기하는 최대 시간입니다.
	maxCatchpointFileChunkDownloadDuration := 2 * time.Minute
	if lf.config.MinCatchpointFileDownloadBytesPerSecond > 0 {
		maxCatchpointFileChunkDownloadDuration += maxCatchpointFileChunkSize * time.Second / time.Duration(lf.config.MinCatchpointFileDownloadBytesPerSecond)
	} else {
		maxCatchpointFileChunkDownloadDuration += maxCatchpointFileChunkSize * time.Second / defaultMinCatchpointFileDownloadBytesPerSecond
	}

	watchdogReader := util.MakeWatchdogStreamReader(response.Body, catchpointFileStreamReadSize, 2*maxCatchpointFileChunkSize, maxCatchpointFileChunkDownloadDuration)
	defer watchdogReader.Close()
	tarReader := tar.NewReader(watchdogReader)
	var downloadProgress ledger.CatchpointCatchupAccessorProgress
	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if header.Size > maxCatchpointFileChunkSize || header.Size < 1 {
			return fmt.Errorf("getPeerLedger received a tar header with data size of %d", header.Size)
		}
		balancesBlockBytes := make([]byte, header.Size)
		readComplete := int64(0)

		for readComplete < header.Size {
			bytesRead, err := tarReader.Read(balancesBlockBytes[readComplete:])
			readComplete += int64(bytesRead)
			if err != nil {
				if err == io.EOF {
					if readComplete == header.Size {
						break
					}
					err = fmt.Errorf("getPeerLedger received io.EOF while reading from tar file stream prior of reaching chunk size %d / %d", readComplete, header.Size)
				}
				return err
			}
		}
		err = lf.processBalancesBlock(ctx, header.Name, balancesBlockBytes, &downloadProgress)
		if err != nil {
			return err
		}
		if lf.reporter != nil {
			lf.reporter.updateLedgerFetcherProgress(&downloadProgress)
		}
		if err = watchdogReader.Reset(); err != nil {
			if err == io.EOF {
				return nil
			}
			err = fmt.Errorf("getPeerLedger received the following error while reading the catchpoint file : %v", err)
			return err
		}
	}
}

func (lf *ledgerFetcher) processBalancesBlock(ctx context.Context, sectionName string, bytes []byte, downloadProgress *ledger.CatchpointCatchupAccessorProgress) error {
	return lf.accessor.ProgressStagingBalances(ctx, sectionName, bytes, downloadProgress)
}
