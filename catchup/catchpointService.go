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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/data/basics"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/ledger"
	"github.com/Orca18/novarand/ledger/ledgercore"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/network"
)

// CatchpointCatchupNodeServices defines the extenal node support needed for the catchpoint service to switch the node between "regular" operational mode and catchup mode.
// CatchpointCatchupNodeServices는 캐치포인트 서비스가 "일반" 작동 모드와 캐치업 모드 사이에서 노드를 전환하는 데 필요한 외부 노드 지원을 정의합니다.
type CatchpointCatchupNodeServices interface {
	SetCatchpointCatchupMode(bool) (newContextCh <-chan context.Context)
}

// CatchpointCatchupStats is used for querying and reporting the current state of the catchpoint catchup process
// CatchpointCatchupStats는 catchpoint catchup 프로세스의 현재 상태를 쿼리하고 보고하는 데 사용됩니다.
type CatchpointCatchupStats struct {
	CatchpointLabel   string
	TotalAccounts     uint64
	ProcessedAccounts uint64
	VerifiedAccounts  uint64
	TotalBlocks       uint64
	AcquiredBlocks    uint64
	VerifiedBlocks    uint64
	ProcessedBytes    uint64
	StartTime         time.Time
}

// CatchpointCatchupService represents the catchpoint catchup service.
// CatchpointCatchupService는 캐치포인트 캐치업 서비스를 나타냅니다.
type CatchpointCatchupService struct {
	// stats is the statistics object, updated async while downloading the ledger
	// stats는 통계 객체이며 원장을 다운로드하는 동안 비동기식으로 업데이트됩니다.
	stats CatchpointCatchupStats
	// statsMu syncronizes access to stats, as we could attempt to update it while querying for it's current state
	// statsMu는 현재 상태를 쿼리하는 동안 업데이트를 시도할 수 있으므로 통계에 대한 액세스를 동기화합니다.
	statsMu deadlock.Mutex
	node    CatchpointCatchupNodeServices
	// ctx is the node cancelation context, used when the node is being stopped.
	// ctx는 노드가 중지될 때 사용되는 노드 취소 컨텍스트입니다.
	ctx           context.Context
	cancelCtxFunc context.CancelFunc
	// running is a waitgroup counting the running goroutine(1), and allow us to exit cleanly.
	// running은 실행 중인 goroutine(1)을 세는 대기 그룹이며, 우리가 깔끔하게 종료할 수 있도록 합니다.
	running sync.WaitGroup
	// ledgerAccessor is the ledger accessor used to perform ledger-level operation on the database
	// ledgerAccessor는 데이터베이스에서 원장 수준 작업을 수행하는 데 사용되는 원장 접근자입니다.
	ledgerAccessor ledger.CatchpointCatchupAccessor
	// stage is the current stage of the catchpoint catchup process
	// stage는 catchpoint catchup 프로세스의 현재 단계입니다.
	stage ledger.CatchpointCatchupState
	// log is the logger object
	// log는 로거 객체입니다.
	log logging.Logger
	// newService indicates whether this service was created after the node was running ( i.e. true ) or the node just started to find that it was previously perfoming catchup
	// newService는 이 서비스가 노드가 실행된 후에 생성되었는지(즉, true ) 또는 노드가 이전에 캐치업을 수행하고 있었다는 것을 발견하기 시작했는지 여부를 나타냅니다.
	newService bool
	// net is the underlaying network module
	// net은 기본 네트워크 모듈입니다.
	net network.GossipNode
	// ledger points to the ledger object
	// 원장은 원장 객체를 가리킵니다.
	ledger *ledger.Ledger
	// lastBlockHeader is the latest block we have before going into catchpoint catchup mode. We use it to serve the node status requests instead of going to the ledger.
	// lastBlockHeader는 캐치포인트 캐치업 모드로 들어가기 전의 최신 블록입니다. 원장으로 이동하는 대신 노드 상태 요청을 처리하는 데 사용합니다.
	lastBlockHeader bookkeeping.BlockHeader
	// config is a copy of the node configuration
	// config는 노드 구성의 복사본입니다.
	config config.Local
	// abortCtx used as a syncronized flag to let us know when the user asked us to abort the catchpoint catchup process.
	// note that it's not being used when we decided to abort the catchup due to an internal issue ( such as exceeding number of retries )
	// abortCtx는 사용자가 캐치포인트 캐치업 프로세스를 중단하도록 요청할 때 알려주는 동기화 플래그로 사용됩니다.
	// 내부 문제(예: 재시도 횟수 초과)로 인해 캐치업을 중단하기로 결정한 경우에는 사용되지 않습니다.
	abortCtx     context.Context
	abortCtxFunc context.CancelFunc
	// blocksDownloadPeerSelector is the peer selector used for downloading blocks.
	// blocksDownloadPeerSelector는 블록을 다운로드하는 데 사용되는 피어 선택기입니다.
	blocksDownloadPeerSelector *peerSelector
}

// MakeResumedCatchpointCatchupService creates a catchpoint catchup service for a node that is already in catchpoint catchup mode
// MakeResumedCatchpointCatchupService는 이미 캐치포인트 캐치업 모드에 있는 노드에 대한 캐치포인트 캐치업 서비스를 생성합니다.
func MakeResumedCatchpointCatchupService(ctx context.Context, node CatchpointCatchupNodeServices, log logging.Logger, net network.GossipNode, l *ledger.Ledger, cfg config.Local) (service *CatchpointCatchupService, err error) {
	service = &CatchpointCatchupService{
		stats: CatchpointCatchupStats{
			StartTime: time.Now(),
		},
		node:           node,
		ledgerAccessor: ledger.MakeCatchpointCatchupAccessor(l, log),
		log:            log,
		newService:     false,
		net:            net,
		ledger:         l,
		config:         cfg,
	}
	service.lastBlockHeader, err = l.BlockHdr(l.Latest())
	if err != nil {
		return nil, err
	}
	err = service.loadStateVariables(ctx)
	if err != nil {
		return nil, err
	}
	service.initDownloadPeerSelector()
	return service, nil
}

// MakeNewCatchpointCatchupService creates a new catchpoint catchup service for a node that is not in catchpoint catchup mode
// MakeNewCatchpointCatchupService는 캐치포인트 캐치업 모드가 아닌 노드에 대해 새로운 캐치포인트 캐치업 서비스를 생성합니다.
func MakeNewCatchpointCatchupService(catchpoint string, node CatchpointCatchupNodeServices, log logging.Logger, net network.GossipNode, l *ledger.Ledger, cfg config.Local) (service *CatchpointCatchupService, err error) {
	if catchpoint == "" {
		return nil, fmt.Errorf("MakeNewCatchpointCatchupService: catchpoint is invalid")
	}
	service = &CatchpointCatchupService{
		stats: CatchpointCatchupStats{
			CatchpointLabel: catchpoint,
			StartTime:       time.Now(),
		},
		node:           node,
		ledgerAccessor: ledger.MakeCatchpointCatchupAccessor(l, log),
		stage:          ledger.CatchpointCatchupStateInactive,
		log:            log,
		newService:     true,
		net:            net,
		ledger:         l,
		config:         cfg,
	}
	service.lastBlockHeader, err = l.BlockHdr(l.Latest())
	if err != nil {
		return nil, err
	}
	service.initDownloadPeerSelector()
	return service, nil
}

// Start starts the catchpoint catchup service ( continue in the process )
// 시작은 캐치포인트 캐치업 서비스를 시작합니다(프로세스 계속).
func (cs *CatchpointCatchupService) Start(ctx context.Context) {
	cs.ctx, cs.cancelCtxFunc = context.WithCancel(ctx)
	cs.abortCtx, cs.abortCtxFunc = context.WithCancel(context.Background())
	cs.running.Add(1)
	go cs.run()
}

// Abort aborts the catchpoint catchup process
// 중단은 캐치포인트 캐치업 프로세스를 중단합니다.
func (cs *CatchpointCatchupService) Abort() {
	// In order to abort the catchpoint catchup process, we need to first set the flag of abortCtxFunc, and follow that by canceling the main context.
	// The order of these calls is crucial : The various stages are blocked on the main context. When that one expires, it uses the abort context to determine
	// if the cancelation meaning that we want to shut down the process, or aborting the catchpoint catchup completly.
	// 캐치포인트 캐치업 프로세스를 중단하려면 먼저 abortCtxFunc 플래그를 설정하고 메인 컨텍스트를 취소해야 합니다.
	// 이러한 호출의 순서가 중요합니다. 다양한 단계가 기본 컨텍스트에서 차단됩니다. 그것이 만료되면 중단 컨텍스트를 사용하여
	// 취소가 프로세스를 종료하거나 캐치포인트 캐치업을 완전히 중단하려는 경우.
	cs.abortCtxFunc()
	cs.cancelCtxFunc()
}

// Stop stops the catchpoint catchup service - unlike Abort, this is not intended to abort the process but rather to allow cleanup of in-memory resources for the purpose of clean shutdown.
// Stop은 캐치포인트 캐치업 서비스를 중지합니다. - Abort와 달리 이는 프로세스를 중단하기 위한 것이 아니라 완전한 종료를 위해 메모리 내 리소스를 정리할 수 있도록 하기 위한 것입니다.
func (cs *CatchpointCatchupService) Stop() {
	// signal the running goroutine that we want to stop
	// 실행 중인 고루틴에 중지하려는 신호를 보냅니다.
	cs.cancelCtxFunc()
	// wait for the running goroutine to terminate.
	// 실행 중인 고루틴이 종료될 때까지 기다립니다.
	cs.running.Wait()
	// call the abort context canceling, just to release it's goroutine.
	// 중단 컨텍스트 취소를 호출하여 고루틴을 해제합니다.
	cs.abortCtxFunc()
}

// GetLatestBlockHeader returns the last block header that was available at the time the catchpoint catchup service started
// GetLatestBlockHeader는 catchpoint catchup 서비스가 시작될 때 사용 가능한 마지막 블록 헤더를 반환합니다.
func (cs *CatchpointCatchupService) GetLatestBlockHeader() bookkeeping.BlockHeader {
	return cs.lastBlockHeader
}

// run is the main stage-switching background service function. It switches the current stage into the correct stage handler.
// run은 메인 스테이지 전환 백그라운드 서비스 기능입니다. 현재 스테이지를 올바른 스테이지 핸들러로 전환합니다.
func (cs *CatchpointCatchupService) run() {
	defer cs.running.Done()
	var err error
	for {
		// check if we need to abort.
		select {
		case <-cs.ctx.Done():
			return
		default:
		}

		switch cs.stage {
		case ledger.CatchpointCatchupStateInactive:
			err = cs.processStageInactive()
		case ledger.CatchpointCatchupStateLedgerDownload:
			err = cs.processStageLedgerDownload()
		case ledger.CatchpointCatchupStateLastestBlockDownload:
			err = cs.processStageLastestBlockDownload()
		case ledger.CatchpointCatchupStateBlocksDownload:
			err = cs.processStageBlocksDownload()
		case ledger.CatchpointCatchupStateSwitch:
			err = cs.processStageSwitch()
		default:
			err = cs.abort(fmt.Errorf("unexpected catchpoint catchup stage encountered : %v", cs.stage))
		}

		if cs.ctx.Err() != nil {
			if err != nil {
				cs.log.Warnf("catchpoint catchup stage error : %v", err)
			}
			continue
		}

		if err != nil {
			cs.log.Warnf("catchpoint catchup stage error : %v", err)
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// loadStateVariables loads the current stage and catchpoint label from disk. It's used only in the case of catchpoint catchup recovery.
// ( i.e. the node never completed the catchup, and the node was shutdown )
// loadStateVariables는 디스크에서 현재 단계 및 캐치포인트 레이블을 로드합니다. 캐치포인트 캐치업 복구의 경우에만 사용됩니다.
// (즉, 노드가 캐치업을 완료하지 않았고 노드가 종료됨)
func (cs *CatchpointCatchupService) loadStateVariables(ctx context.Context) (err error) {
	var label string
	label, err = cs.ledgerAccessor.GetLabel(ctx)
	if err != nil {
		return err
	}
	cs.statsMu.Lock()
	cs.stats.CatchpointLabel = label
	cs.statsMu.Unlock()

	cs.stage, err = cs.ledgerAccessor.GetState(ctx)
	if err != nil {
		return err
	}
	return nil
}

// processStageInactive is the first catchpoint stage.
// It stores the desired label for catching up, so that if the catchpoint catchup is interrupted it could be resumed from that point.
// processStageInactive는 첫 번째 캐치포인트 단계입니다.
// 따라잡기 위해 원하는 레이블을 저장하므로 catchpoint catchup이 중단되면 해당 지점에서 다시 시작할 수 있습니다.
func (cs *CatchpointCatchupService) processStageInactive() (err error) {
	cs.statsMu.Lock()
	label := cs.stats.CatchpointLabel
	cs.statsMu.Unlock()
	err = cs.ledgerAccessor.SetLabel(cs.ctx, label)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageInactive failed to set a catchpoint label : %v", err))
	}
	err = cs.updateStage(ledger.CatchpointCatchupStateLedgerDownload)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageInactive failed to update stage : %v", err))
	}
	if cs.newService {
		// we need to let the node know that it should shut down all the unneed services to avoid clashes.
		cs.updateNodeCatchupMode(true)
	}
	return nil
}

// processStageLedgerDownload is the second catchpoint catchup stage. It downloads the ledger.
// processStageLedgerDownload는 두 번째 캐치포인트 캐치업 단계입니다. 원장을 다운로드합니다.
func (cs *CatchpointCatchupService) processStageLedgerDownload() (err error) {
	cs.statsMu.Lock()
	label := cs.stats.CatchpointLabel
	cs.statsMu.Unlock()
	round, _, err0 := ledgercore.ParseCatchpointLabel(label)

	if err0 != nil {
		return cs.abort(fmt.Errorf("processStageLedgerDownload failed to patse label : %v", err0))
	}

	// download balances file.
	peerSelector := makePeerSelector(cs.net, []peerClass{{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersPhonebookRelays}})
	ledgerFetcher := makeLedgerFetcher(cs.net, cs.ledgerAccessor, cs.log, cs, cs.config)
	attemptsCount := 0

	for {
		attemptsCount++

		err = cs.ledgerAccessor.ResetStagingBalances(cs.ctx, true)
		if err != nil {
			if cs.ctx.Err() != nil {
				return cs.stopOrAbort()
			}
			return cs.abort(fmt.Errorf("processStageLedgerDownload failed to reset staging balances : %v", err))
		}
		psp, err := peerSelector.getNextPeer()
		if err != nil {
			err = fmt.Errorf("processStageLedgerDownload: catchpoint catchup was unable to obtain a list of peers to retrieve the catchpoint file from")
			return cs.abort(err)
		}
		peer := psp.Peer
		err = ledgerFetcher.downloadLedger(cs.ctx, peer, round)
		if err == nil {
			err = cs.ledgerAccessor.BuildMerkleTrie(cs.ctx, cs.updateVerifiedAccounts)
			if err == nil {
				break
			}
			// failed to build the merkle trie for the above catchpoint file.
			peerSelector.rankPeer(psp, peerRankInvalidDownload)
		} else {
			peerSelector.rankPeer(psp, peerRankDownloadFailed)
		}

		// instead of testing for err == cs.ctx.Err() , we'll check on the context itself.
		// this is more robust, as the http client library sometimes wrap the context canceled error with other errors.
		// err == cs.ctx.Err() 을 테스트하는 대신 컨텍스트 자체를 확인합니다.
		// http 클라이언트 라이브러리가 때때로 컨텍스트 취소 오류를 다른 오류로 래핑하기 때문에 이것은 더 강력합니다.
		if cs.ctx.Err() != nil {
			return cs.stopOrAbort()
		}

		if attemptsCount >= cs.config.CatchupLedgerDownloadRetryAttempts {
			err = fmt.Errorf("processStageLedgerDownload: catchpoint catchup exceeded number of attempts to retrieve ledger")
			return cs.abort(err)
		}
		cs.log.Warnf("unable to download ledger : %v", err)
	}

	err = cs.updateStage(ledger.CatchpointCatchupStateLastestBlockDownload)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageLedgerDownload failed to update stage to CatchpointCatchupStateLastestBlockDownload : %v", err))
	}
	return nil
}

// updateVerifiedAccounts update the user's statistics for the given verified accounts
// updateVerifiedAccounts는 주어진 확인된 계정에 대한 사용자 통계를 업데이트합니다.
func (cs *CatchpointCatchupService) updateVerifiedAccounts(verifiedAccounts uint64) {
	cs.statsMu.Lock()
	defer cs.statsMu.Unlock()
	cs.stats.VerifiedAccounts = verifiedAccounts
}

// processStageLastestBlockDownload is the third catchpoint catchup stage. It downloads the latest block and verify that against the previously downloaded ledger.
// processStageLastestBlockDownload는 세 번째 캐치포인트 캐치업 단계입니다. 최신 블록을 다운로드하고 이전에 다운로드한 원장에 대해 확인합니다.
func (cs *CatchpointCatchupService) processStageLastestBlockDownload() (err error) {
	blockRound, err := cs.ledgerAccessor.GetCatchupBlockRound(cs.ctx)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageLastestBlockDownload failed to retrieve catchup block round : %v", err))
	}

	attemptsCount := 0
	var blk *bookkeeping.Block
	// check to see if the current ledger might have this block. If so, we should try this first instead of downloading anything.
	// 현재 원장에 이 블록이 있는지 확인합니다. 그렇다면 아무것도 다운로드하지 말고 먼저 시도해야 합니다.
	if ledgerBlock, err := cs.ledger.Block(blockRound); err == nil {
		blk = &ledgerBlock
	}
	var protoParams config.ConsensusParams
	var ok bool

	for {
		attemptsCount++

		var psp *peerSelectorPeer
		blockDownloadDuration := time.Duration(0)
		if blk == nil {
			var stop bool
			blk, blockDownloadDuration, psp, stop, err = cs.fetchBlock(blockRound, uint64(attemptsCount))
			if stop {
				return err
			} else if blk == nil {
				continue
			}
		}

		// check block protocol version support.
		// 블록 프로토콜 버전 지원을 확인합니다.
		if protoParams, ok = config.Consensus[blk.BlockHeader.CurrentProtocol]; !ok {
			cs.log.Warnf("processStageLastestBlockDownload: unsupported protocol version detected: '%v'", blk.BlockHeader.CurrentProtocol)

			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload: unsupported protocol version detected: '%v'", blk.BlockHeader.CurrentProtocol))
		}

		// We need to compare explicitly the genesis hash since we're not doing any block validation.
		// This would ensure the genesis.json file matches the block that we've receieved.
		// 블록 유효성 검사를 수행하지 않기 때문에 명시적으로 제네시스 해시를 비교해야 합니다.
		// 이렇게 하면 genesis.json 파일이 우리가 받은 블록과 일치하는지 확인할 수 있습니다.
		if protoParams.SupportGenesisHash && blk.GenesisHash() != cs.ledger.GenesisHash() {
			cs.log.Warnf("processStageLastestBlockDownload: genesis hash mismatches : genesis hash on genesis.json file is %v while genesis hash of downloaded block is %v", cs.ledger.GenesisHash(), blk.GenesisHash())
			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload: genesis hash mismatches : genesis hash on genesis.json file is %v while genesis hash of downloaded block is %v", cs.ledger.GenesisHash(), blk.GenesisHash()))
		}

		// check to see that the block header and the block payset aligns
		// 블록 헤더와 블록 페이셋이 정렬되었는지 확인합니다.
		if !blk.ContentsMatchHeader() {
			cs.log.Warnf("processStageLastestBlockDownload: downloaded block content does not match downloaded block header")

			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload: downloaded block content does not match downloaded block header"))
		}

		// verify that the catchpoint is valid.
		// 캐치포인트가 유효한지 확인합니다.
		err = cs.ledgerAccessor.VerifyCatchpoint(cs.ctx, blk)
		if err != nil {
			if cs.ctx.Err() != nil {
				return cs.stopOrAbort()
			}
			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				cs.log.Infof("processStageLastestBlockDownload: block %d verification against catchpoint failed, another attempt will be made; err = %v", blockRound, err)
				cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload failed when calling VerifyCatchpoint : %v", err))
		}
		// give a rank to the download, as the download was successful.
		// 다운로드가 성공적이었으므로 다운로드에 순위를 지정합니다.
		peerRank := cs.blocksDownloadPeerSelector.peerDownloadDurationToRank(psp, blockDownloadDuration)
		cs.blocksDownloadPeerSelector.rankPeer(psp, peerRank)

		err = cs.ledgerAccessor.StoreBalancesRound(cs.ctx, blk)
		if err != nil {
			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload failed when calling StoreBalancesRound : %v", err))
		}

		err = cs.ledgerAccessor.StoreFirstBlock(cs.ctx, blk)
		if err != nil {
			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload failed when calling StoreFirstBlock : %v", err))
		}

		err = cs.updateStage(ledger.CatchpointCatchupStateBlocksDownload)
		if err != nil {
			if attemptsCount <= cs.config.CatchupBlockDownloadRetryAttempts {
				// try again.
				blk = nil
				continue
			}
			return cs.abort(fmt.Errorf("processStageLastestBlockDownload failed to update stage : %v", err))
		}

		// great ! everything is ready for next stage.
		break
	}
	return nil
}

// processStageBlocksDownload is the fourth catchpoint catchup stage.
// It downloads all the reminder of the blocks, verifying each one of them against it's predecessor.\
// processStageBlocksDownload는 네 번째 캐치포인트 캐치업 단계입니다.
// 블록의 모든 알림을 다운로드하여 각각의 블록을 이전 블록과 비교하여 확인합니다.
func (cs *CatchpointCatchupService) processStageBlocksDownload() (err error) {
	topBlock, err := cs.ledgerAccessor.EnsureFirstBlock(cs.ctx)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageBlocksDownload failed, unable to ensure first block : %v", err))
	}

	// pick the lookback with the greater of either MaxTxnLife or MaxBalLookback
	// MaxTxnLife 또는 MaxBalLookback 중 더 큰 값으로 룩백을 선택합니다.
	lookback := config.Consensus[topBlock.CurrentProtocol].MaxTxnLife
	if lookback < config.Consensus[topBlock.CurrentProtocol].MaxBalLookback {
		lookback = config.Consensus[topBlock.CurrentProtocol].MaxBalLookback
	}
	// in case the effective lookback is going before our rounds count, trim it there.
	// ( a catchpoint is generated starting round MaxBalLookback, and this is a possible in any round in the range of MaxBalLookback..MaxTxnLife)
	// 유효 룩백이 라운드 카운트 전에 진행되는 경우 거기에서 트리밍합니다.
	// (캐치포인트는 MaxBalLookback 라운드부터 생성되며, 이는 MaxBalLookback..MaxTxnLife 범위의 모든 라운드에서 가능합니다.)
	if lookback >= uint64(topBlock.Round()) {
		lookback = uint64(topBlock.Round() - 1)
	}

	cs.statsMu.Lock()
	cs.stats.TotalBlocks = uint64(lookback)
	cs.stats.AcquiredBlocks = 0
	cs.stats.VerifiedBlocks = 0
	cs.statsMu.Unlock()

	prevBlock := &topBlock
	blocksFetched := uint64(1) // we already got the first block in the previous step.
	// 우리는 이미 이전 단계에서 첫 번째 블록을 얻었습니다.
	var blk *bookkeeping.Block
	for retryCount := uint64(1); blocksFetched <= lookback; {
		if err := cs.ctx.Err(); err != nil {
			return cs.stopOrAbort()
		}

		blk = nil
		// check to see if the current ledger might have this block.
		// If so, we should try this first instead of downloading anything.
		// 현재 원장에 이 블록이 있는지 확인합니다.
		// 그렇다면 다운로드하는 대신 먼저 시도해야 합니다.
		if ledgerBlock, err := cs.ledger.Block(topBlock.Round() - basics.Round(blocksFetched)); err == nil {
			blk = &ledgerBlock
		} else {
			switch err.(type) {
			case ledgercore.ErrNoEntry:
				// this is expected, ignore this one.
				// 이것은 예상된 것이므로 무시하십시오.
			default:
				cs.log.Warnf("processStageBlocksDownload encountered the following error when attempting to retrieve the block for round %d : %v", topBlock.Round()-basics.Round(blocksFetched), err)
			}
		}

		var psp *peerSelectorPeer
		blockDownloadDuration := time.Duration(0)
		if blk == nil {
			var stop bool
			blk, blockDownloadDuration, psp, stop, err = cs.fetchBlock(topBlock.Round()-basics.Round(blocksFetched), retryCount)
			if stop {
				return err
			} else if blk == nil {
				retryCount++
				continue
			}
		}

		cs.updateBlockRetrievalStatistics(1, 0)

		// validate :
		if prevBlock.BlockHeader.Branch != blk.Hash() {
			// not identical, retry download.
			// 동일하지 않습니다. 다운로드를 다시 시도하십시오.
			cs.log.Warnf("processStageBlocksDownload downloaded block(%d) did not match it's successor(%d) block hash %v != %v", blk.Round(), prevBlock.Round(), blk.Hash(), prevBlock.BlockHeader.Branch)
			cs.updateBlockRetrievalStatistics(-1, 0)
			cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
			if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
				// try again.
				retryCount++
				continue
			}
			return cs.abort(fmt.Errorf("processStageBlocksDownload downloaded block(%d) did not match it's successor(%d) block hash %v != %v", blk.Round(), prevBlock.Round(), blk.Hash(), prevBlock.BlockHeader.Branch))
		}

		// check block protocol version support.
		// 블록 프로토콜 버전 지원을 확인합니다.
		if _, ok := config.Consensus[blk.BlockHeader.CurrentProtocol]; !ok {
			cs.log.Warnf("processStageBlocksDownload: unsupported protocol version detected: '%v'", blk.BlockHeader.CurrentProtocol)
			cs.updateBlockRetrievalStatistics(-1, 0)
			cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
			if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
				// try again.
				retryCount++
				continue
			}
			return cs.abort(fmt.Errorf("processStageBlocksDownload: unsupported protocol version detected: '%v'", blk.BlockHeader.CurrentProtocol))
		}

		// check to see that the block header and the block payset aligns
		// 블록 헤더와 블록 페이셋이 정렬되었는지 확인합니다.
		if !blk.ContentsMatchHeader() {
			cs.log.Warnf("processStageBlocksDownload: downloaded block content does not match downloaded block header")
			// try again.
			cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
			cs.updateBlockRetrievalStatistics(-1, 0)
			if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
				// try again.
				retryCount++
				continue
			}
			return cs.abort(fmt.Errorf("processStageBlocksDownload: downloaded block content does not match downloaded block header"))
		}

		cs.updateBlockRetrievalStatistics(0, 1)
		peerRank := cs.blocksDownloadPeerSelector.peerDownloadDurationToRank(psp, blockDownloadDuration)
		cs.blocksDownloadPeerSelector.rankPeer(psp, peerRank)

		// all good, persist and move on.
		// 모든 것이 좋습니다. 지속하고 계속 진행합니다.
		err = cs.ledgerAccessor.StoreBlock(cs.ctx, blk)
		if err != nil {
			cs.log.Warnf("processStageBlocksDownload failed to store downloaded staging block for round %d", blk.Round())
			cs.updateBlockRetrievalStatistics(-1, -1)
			if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
				// try again.
				retryCount++
				continue
			}
			return cs.abort(fmt.Errorf("processStageBlocksDownload failed to store downloaded staging block for round %d", blk.Round()))
		}
		prevBlock = blk
		blocksFetched++
	}

	err = cs.updateStage(ledger.CatchpointCatchupStateSwitch)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageBlocksDownload failed to update stage : %v", err))
	}
	return nil
}

// fetchBlock uses the internal peer selector blocksDownloadPeerSelector to pick a peer and then attempt to fetch the block requested from that peer.
// The method return stop=true if the caller should exit the current operation
// If the method return a nil block, the caller is expected to retry the operation, increasing the retry counter as needed.
// fetchBlock은 내부 피어 선택기 blocksDownloadPeerSelector를 사용하여 피어를 선택한 다음 해당 피어에서 요청한 블록 가져오기를 시도합니다.
// 호출자가 현재 작업을 종료해야 하는 경우 메서드 반환 stop=true
// 메서드가 nil 블록을 반환하면 호출자는 작업을 다시 시도하여 필요에 따라 재시도 카운터를 늘립니다.
func (cs *CatchpointCatchupService) fetchBlock(round basics.Round, retryCount uint64) (blk *bookkeeping.Block, downloadDuration time.Duration, psp *peerSelectorPeer, stop bool, err error) {
	psp, err = cs.blocksDownloadPeerSelector.getNextPeer()
	if err != nil {
		err = fmt.Errorf("fetchBlock: unable to obtain a list of peers to retrieve the latest block from")
		return nil, time.Duration(0), psp, true, cs.abort(err)
	}
	peer := psp.Peer

	httpPeer, validPeer := peer.(network.HTTPPeer)
	if !validPeer {
		cs.log.Warnf("fetchBlock: non-HTTP peer was provided by the peer selector")
		cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankInvalidDownload)
		if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
			// try again.
			return nil, time.Duration(0), psp, false, nil
		}
		return nil, time.Duration(0), psp, true, cs.abort(fmt.Errorf("fetchBlock: recurring non-HTTP peer was provided by the peer selector"))
	}
	fetcher := makeUniversalBlockFetcher(cs.log, cs.net, cs.config)
	blk, _, downloadDuration, err = fetcher.fetchBlock(cs.ctx, round, httpPeer)
	if err != nil {
		if cs.ctx.Err() != nil {
			return nil, time.Duration(0), psp, true, cs.stopOrAbort()
		}
		if retryCount <= uint64(cs.config.CatchupBlockDownloadRetryAttempts) {
			// try again.
			cs.log.Infof("Failed to download block %d on attempt %d out of %d. %v", round, retryCount, cs.config.CatchupBlockDownloadRetryAttempts, err)
			cs.blocksDownloadPeerSelector.rankPeer(psp, peerRankDownloadFailed)
			return nil, time.Duration(0), psp, false, nil
		}
		return nil, time.Duration(0), psp, true, cs.abort(fmt.Errorf("fetchBlock failed after multiple blocks download attempts"))
	}
	// success
	return blk, downloadDuration, psp, false, nil
}

// processStageLedgerDownload is the fifth catchpoint catchup stage.
// It completes the catchup process, swap the new tables and restart the node functionality.
// processStageLedgerDownload는 다섯 번째 캐치포인트 캐치업 단계입니다.
// 캐치업 프로세스를 완료하고 새 테이블을 교체하고 노드 기능을 다시 시작합니다.
func (cs *CatchpointCatchupService) processStageSwitch() (err error) {
	err = cs.ledgerAccessor.CompleteCatchup(cs.ctx)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageSwitch failed to complete catchup : %v", err))
	}

	err = cs.updateStage(ledger.CatchpointCatchupStateInactive)
	if err != nil {
		return cs.abort(fmt.Errorf("processStageSwitch failed to update stage : %v", err))
	}
	cs.updateNodeCatchupMode(false)
	// we've completed the catchup, so we want to cancel the context so that the
	// run function would exit.
	cs.cancelCtxFunc()
	return nil
}

// stopOrAbort is called when any of the stage processing function sees that cs.ctx has been canceled.
// It can be due to the end user attempting to abort the current catchpoint catchup operation or due to a node shutdown.
// stopOrAbort는 cs.ctx가 취소된 것을 스테이지 처리 함수에서 확인할 때 호출됩니다.
// 최종 사용자가 현재 캐치포인트 캐치업 작업을 중단하려고 하거나 노드 종료로 인해 발생할 수 있습니다.
func (cs *CatchpointCatchupService) stopOrAbort() error {
	if cs.abortCtx.Err() == context.Canceled {
		return cs.abort(context.Canceled)
	}
	return nil
}

// abort aborts the current catchpoint catchup process, reverting to node to standard operation.
// abort는 현재 캐치포인트 캐치업 프로세스를 중단하고 노드를 표준 작업으로 되돌립니다.
func (cs *CatchpointCatchupService) abort(originatingErr error) error {
	outError := originatingErr
	err0 := cs.ledgerAccessor.ResetStagingBalances(cs.ctx, false)
	if err0 != nil {
		outError = fmt.Errorf("unable to reset staging balances : %v; %v", err0, outError)
	}
	cs.updateNodeCatchupMode(false)
	// we want to abort the catchpoint catchup process, and the node already reverted to normal operation.
	// as part of the returning to normal operation, we've re-created our context.
	// This context need to be canceled so that when we go back to run(), we would exit from there right away.
	// 캐치포인트 캐치업 프로세스를 중단하고 노드가 이미 정상 작동으로 되돌리고 싶습니다.
	// 정상 작동으로 돌아가기의 일부로 컨텍스트를 다시 만들었습니다.
	// 이 컨텍스트를 취소해야 run()으로 돌아갈 때 바로 종료됩니다.
	cs.cancelCtxFunc()
	return outError
}

// updateStage updates the current catchpoint catchup stage to the provided new stage.
// updateStage는 현재 catchpoint catchup 단계를 제공된 새 단계로 업데이트합니다.
func (cs *CatchpointCatchupService) updateStage(newStage ledger.CatchpointCatchupState) (err error) {
	err = cs.ledgerAccessor.SetState(cs.ctx, newStage)
	if err != nil {
		return err
	}
	cs.stage = newStage
	return nil
}

// updateNodeCatchupMode requests the node to change it's operational mode from catchup mode to normal mode and vice versa.
// updateNodeCatchupMode는 노드가 캐치업 모드에서 일반 모드로 또는 그 반대로 작동 모드를 변경하도록 요청합니다.
func (cs *CatchpointCatchupService) updateNodeCatchupMode(catchupModeEnabled bool) {
	newCtxCh := cs.node.SetCatchpointCatchupMode(catchupModeEnabled)
	select {
	case newCtx, open := <-newCtxCh:
		if open {
			cs.ctx, cs.cancelCtxFunc = context.WithCancel(newCtx)
		} else {
			// channel is closed, this means that the node is stopping
		}
	case <-cs.ctx.Done():
		// the node context was canceled before the SetCatchpointCatchupMode goroutine had the chance of completing.
		// We At this point, the service is shutting down.
		// However, we don't know how long it would take for the node mutex until it's become available.
		// given that the SetCatchpointCatchupMode gave us a non-buffered channel, it might get blocked if we won't be draining that channel.
		// To resolve that, we will create another goroutine here which would drain that channel.
		// SetCatchpointCatchupMode 고루틴이 완료되기 전에 노드 컨텍스트가 취소되었습니다.
		// We 이 시점에서 서비스가 종료됩니다.
		// 그러나 노드 뮤텍스가 사용 가능해질 때까지 얼마나 걸릴지 모릅니다.
		// SetCatchpointCatchupMode가 버퍼되지 않은 채널을 제공했다면 해당 채널을 비우지 않을 경우 차단될 수 있습니다.
		// 이를 해결하기 위해 해당 채널을 배수하는 또 다른 고루틴을 여기에서 생성합니다.
		go func() {
			// We'll wait here for the above goroutine to complete :
			// 여기에서 위의 고루틴이 완료될 때까지 기다립니다.
			<-newCtxCh
		}()
	}
}

func (cs *CatchpointCatchupService) updateLedgerFetcherProgress(fetcherStats *ledger.CatchpointCatchupAccessorProgress) {
	cs.statsMu.Lock()
	defer cs.statsMu.Unlock()
	cs.stats.TotalAccounts = fetcherStats.TotalAccounts
	cs.stats.ProcessedAccounts = fetcherStats.ProcessedAccounts
	cs.stats.ProcessedBytes = fetcherStats.ProcessedBytes
}

// GetStatistics returns a copy of the current catchpoint catchup statistics
// GetStatistics는 현재 캐치포인트 캐치업 통계의 복사본을 반환합니다.
func (cs *CatchpointCatchupService) GetStatistics() (out CatchpointCatchupStats) {
	cs.statsMu.Lock()
	defer cs.statsMu.Unlock()
	out = cs.stats
	return
}

// updateBlockRetrievalStatistics updates the blocks retrieval statistics by applying the provided deltas
// updateBlockRetrievalStatistics는 제공된 델타를 적용하여 블록 검색 통계를 업데이트합니다.
func (cs *CatchpointCatchupService) updateBlockRetrievalStatistics(aquiredBlocksDelta, verifiedBlocksDelta int64) {
	cs.statsMu.Lock()
	defer cs.statsMu.Unlock()
	cs.stats.AcquiredBlocks = uint64(int64(cs.stats.AcquiredBlocks) + aquiredBlocksDelta)
	cs.stats.VerifiedBlocks = uint64(int64(cs.stats.VerifiedBlocks) + verifiedBlocksDelta)
}

func (cs *CatchpointCatchupService) initDownloadPeerSelector() {
	if cs.config.EnableCatchupFromArchiveServers {
		cs.blocksDownloadPeerSelector = makePeerSelector(
			cs.net,
			[]peerClass{
				{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersPhonebookArchivers},
				{initialRank: peerRankInitialSecondPriority, peerClass: network.PeersPhonebookRelays},
			})
	} else {
		cs.blocksDownloadPeerSelector = makePeerSelector(
			cs.net,
			[]peerClass{
				{initialRank: peerRankInitialFirstPriority, peerClass: network.PeersPhonebookRelays},
			})
	}
}
