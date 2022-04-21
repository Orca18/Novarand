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

package ledger

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/algorand/msgp/msgp"

	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/protocol"
)

const (
	// BalancesPerCatchpointFileChunk defines the number of accounts that would be stored in each chunk in the catchpoint file.
	// note that the last chunk would typically be less than this number.
	/*
		BalancesPerCatchpointFileChunk는 캐치포인트 파일의 각 청크(계정의 묶음)에 저장될 계정 수를 정의합니다.
		마지막 청크는 일반적으로 이 숫자보다 작습니다
	*/
	BalancesPerCatchpointFileChunk = 512

	// catchpointFileVersion is the catchpoint file version
	catchpointFileVersion = uint64(0200)
)

// catchpointWriter is the struct managing the persistence of accounts data into the catchpoint file.
// it's designed to work in a step fashion : a caller will call the WriteStep method in a loop until
// the writing is complete. It might take multiple steps until the operation is over, and the caller
// has the option of throttling the CPU utilization in between the calls.
/*
catchpointWriter는 캐치포인트 파일에 대한 계정 데이터의 영속성을 관리하는 구조체입니다 => catchpoint 파일에 계정정보를 작성하는걸까?
단계적으로 작동하도록 설계되었습니다. 호출자는 쓰기가 완료될 때까지 루프에서 WriteStep(파일에 계정정보를 저장) 메서드를 호출합니다.
작업이 끝날 때까지 여러 단계가 필요할 수 있으며 호출자는 호출 사이에 CPU 사용률을 조절할 수 있습니다.
*/
type catchpointWriter struct {
	ctx               context.Context
	hasher            hash.Hash
	innerWriter       io.WriteCloser
	tx                *sql.Tx
	filePath          string
	file              *os.File
	gzip              *gzip.Writer
	tar               *tar.Writer
	headerWritten     bool
	balancesOffset    int
	balancesChunk     catchpointFileBalancesChunk
	fileHeader        *CatchpointFileHeader
	balancesChunkNum  uint64
	writtenBytes      int64
	blocksRound       basics.Round
	blockHeaderDigest crypto.Digest
	label             string
	accountsIterator  encodedAccountsBatchIter
}

type encodedBalanceRecord struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	Address     basics.Address `codec:"pk,allocbound=crypto.DigestSize"`
	AccountData msgp.Raw       `codec:"ad,allocbound=basics.MaxEncodedAccountDataSize"`
}

// CatchpointFileHeader is the content we would have in the "content.msgpack" file in the catchpoint tar archive.
// we need it to be public, as it's being decoded externally by the catchpointdump utility.
/*
CatchpointFileHeader는 catchpoint.tar 아카이브의 "content.msgpack" 파일에 있는 내용입니다.
catchpointdump 유틸리티에 의해 외부적으로 디코딩되기 때문에 공개되어야 합니다
*/
type CatchpointFileHeader struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	Version           uint64                   `codec:"version"`
	BalancesRound     basics.Round             `codec:"balancesRound"`
	BlocksRound       basics.Round             `codec:"blocksRound"`
	Totals            ledgercore.AccountTotals `codec:"accountTotals"`
	TotalAccounts     uint64                   `codec:"accountsCount"`
	TotalChunks       uint64                   `codec:"chunksCount"`
	Catchpoint        string                   `codec:"catchpoint"`
	BlockHeaderDigest crypto.Digest            `codec:"blockHeaderDigest"`
}

type catchpointFileBalancesChunk struct {
	_struct  struct{}               `codec:",omitempty,omitemptyarray"`
	Balances []encodedBalanceRecord `codec:"bl,allocbound=BalancesPerCatchpointFileChunk"`
}

func makeCatchpointWriter(ctx context.Context, filePath string, tx *sql.Tx, blocksRound basics.Round, blockHeaderDigest crypto.Digest, label string) *catchpointWriter {
	return &catchpointWriter{
		ctx:               ctx,
		filePath:          filePath,
		tx:                tx,
		blocksRound:       blocksRound,
		blockHeaderDigest: blockHeaderDigest,
		label:             label,
	}
}

func (cw *catchpointWriter) Abort() error {
	cw.accountsIterator.Close()
	if cw.tar != nil {
		cw.tar.Close()
	}
	if cw.gzip != nil {
		cw.gzip.Close()
	}
	if cw.file != nil {
		cw.gzip.Close()
	}
	err := os.Remove(cw.filePath)
	return err
}

func (cw *catchpointWriter) WriteStep(stepCtx context.Context) (more bool, err error) {
	if cw.file == nil {
		err = os.MkdirAll(filepath.Dir(cw.filePath), 0700)
		if err != nil {
			return
		}
		cw.file, err = os.OpenFile(cw.filePath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return
		}
		cw.gzip = gzip.NewWriter(cw.file)
		cw.tar = tar.NewWriter(cw.gzip)
	}

	// have we timed-out / canceled by that point ?
	if more, err = hasContextDeadlineExceeded(stepCtx); more == true || err != nil {
		return
	}

	if cw.fileHeader == nil {
		err = cw.readHeaderFromDatabase(cw.ctx, cw.tx)
		if err != nil {
			return
		}
	}

	// have we timed-out / canceled by that point ?
	if more, err = hasContextDeadlineExceeded(stepCtx); more == true || err != nil {
		return
	}

	if !cw.headerWritten {
		encodedHeader := protocol.Encode(cw.fileHeader)
		err = cw.tar.WriteHeader(&tar.Header{
			Name: "content.msgpack",
			Mode: 0600,
			Size: int64(len(encodedHeader)),
		})
		if err != nil {
			return
		}
		_, err = cw.tar.Write(encodedHeader)
		if err != nil {
			return
		}
		cw.headerWritten = true
	}

	writerRequest := make(chan catchpointFileBalancesChunk, 1)
	writerResponse := make(chan error, 2)
	go cw.asyncWriter(writerRequest, writerResponse, cw.balancesChunkNum)
	defer func() {
		close(writerRequest)
		// wait for the writerResponse to close.
		for {
			select {
			case writerError, open := <-writerResponse:
				if open {
					err = writerError
				} else {
					return
				}
			}
		}
	}()

	for {
		// have we timed-out / canceled by that point ?
		if more, err = hasContextDeadlineExceeded(stepCtx); more == true || err != nil {
			return
		}

		if len(cw.balancesChunk.Balances) == 0 {
			err = cw.readDatabaseStep(cw.ctx, cw.tx)
			if err != nil {
				return
			}
		}

		// have we timed-out / canceled by that point ?
		if more, err = hasContextDeadlineExceeded(stepCtx); more == true || err != nil {
			return
		}

		// check if we had any error on the writer from previous iterations.
		select {
		case err := <-writerResponse:
			// we ran into an error. wait for the channel to close before returning with the error.
			select {
			case <-writerResponse:
			}
			return false, err
		default:
		}

		// write to disk.
		if len(cw.balancesChunk.Balances) > 0 {
			cw.balancesChunkNum++
			writerRequest <- cw.balancesChunk
			if len(cw.balancesChunk.Balances) < BalancesPerCatchpointFileChunk || cw.balancesChunkNum == cw.fileHeader.TotalChunks {
				cw.accountsIterator.Close()
				// if we're done, wait for the writer to complete it's writing.
				select {
				case err, opened := <-writerResponse:
					if opened {
						// we ran into an error. wait for the channel to close before returning with the error.
						select {
						case <-writerResponse:
						}
						return false, err
					}
					// channel is closed. we're done writing and no issues detected.
					return false, nil
				}
			}
			cw.balancesChunk.Balances = nil
		}
	}
}

func (cw *catchpointWriter) asyncWriter(balances chan catchpointFileBalancesChunk, response chan error, initialBalancesChunkNum uint64) {
	defer close(response)
	balancesChunkNum := initialBalancesChunkNum
	for bc := range balances {
		balancesChunkNum++
		if len(bc.Balances) == 0 {
			break
		}

		encodedChunk := protocol.Encode(&bc)
		err := cw.tar.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("balances.%d.%d.msgpack", balancesChunkNum, cw.fileHeader.TotalChunks),
			Mode: 0600,
			Size: int64(len(encodedChunk)),
		})
		if err != nil {
			response <- err
			break
		}
		_, err = cw.tar.Write(encodedChunk)
		if err != nil {
			response <- err
			break
		}

		if len(bc.Balances) < BalancesPerCatchpointFileChunk || balancesChunkNum == cw.fileHeader.TotalChunks {
			cw.tar.Close()
			cw.gzip.Close()
			cw.file.Close()
			cw.file = nil
			var fileInfo os.FileInfo
			fileInfo, err = os.Stat(cw.filePath)
			if err != nil {
				response <- err
				break
			}
			cw.writtenBytes = fileInfo.Size()
			break
		}
	}
}

func (cw *catchpointWriter) readDatabaseStep(ctx context.Context, tx *sql.Tx) (err error) {
	cw.balancesChunk.Balances, err = cw.accountsIterator.Next(ctx, tx, BalancesPerCatchpointFileChunk)
	if err == nil {
		cw.balancesOffset += BalancesPerCatchpointFileChunk
	}
	return
}

func (cw *catchpointWriter) readHeaderFromDatabase(ctx context.Context, tx *sql.Tx) (err error) {
	var header CatchpointFileHeader
	header.BalancesRound, err = accountsRound(tx)
	if err != nil {
		return
	}
	header.Totals, err = accountsTotals(tx, false)
	if err != nil {
		return
	}
	header.TotalAccounts, err = totalAccounts(context.Background(), tx)
	if err != nil {
		return
	}
	header.TotalChunks = (header.TotalAccounts + BalancesPerCatchpointFileChunk - 1) / BalancesPerCatchpointFileChunk
	header.BlocksRound = cw.blocksRound
	header.Catchpoint = cw.label
	header.Version = catchpointFileVersion
	header.BlockHeaderDigest = cw.blockHeaderDigest
	cw.fileHeader = &header
	return
}

// GetSize returns the number of bytes that have been written to the file.
func (cw *catchpointWriter) GetSize() int64 {
	return cw.writtenBytes
}

// GetBalancesRound returns the round number of the balances to which this catchpoint is generated for.
func (cw *catchpointWriter) GetBalancesRound() basics.Round {
	if cw.fileHeader != nil {
		return cw.fileHeader.BalancesRound
	}
	return basics.Round(0)
}

// GetBalancesCount returns the number of balances written to this catchpoint file.
func (cw *catchpointWriter) GetTotalAccounts() uint64 {
	if cw.fileHeader != nil {
		return cw.fileHeader.TotalAccounts
	}
	return 0
}

// GetCatchpoint returns the catchpoint string to which this catchpoint file was generated for.
func (cw *catchpointWriter) GetCatchpoint() string {
	if cw.fileHeader != nil {
		return cw.fileHeader.Catchpoint
	}
	return ""
}

// hasContextDeadlineExceeded examine the given context and see if it was canceled or timed-out.
// if it has timed out, the function returns contextExceeded=true and contextError = nil.
// if it's a non-timeout error, the functions returns contextExceeded=false and contextError = error.
// otherwise, the function returns the contextExceeded=false and contextError = nil.
func hasContextDeadlineExceeded(ctx context.Context) (contextExceeded bool, contextError error) {
	// have we timed-out / canceled by that point ?
	select {
	case <-ctx.Done():
		contextError = ctx.Err()
		if contextError == context.DeadlineExceeded {
			contextExceeded = true
			contextError = nil
			return
		}
	default:
	}
	return
}
