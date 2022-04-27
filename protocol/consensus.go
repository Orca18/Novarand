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
	"fmt"
)

// ConsensusVersion is a string that identifies a version of the consensus protocol.
// ConsensusVersion은 합의 프로토콜의 버전을 식별하는 문자열입니다.
type ConsensusVersion string

// DEPRECATEDConsensusV0 is a baseline version of the Algorand consensus protocol.
// at the time versioning was introduced.
// It is now deprecated.
const DEPRECATEDConsensusV0 = ConsensusVersion("v0")

// DEPRECATEDConsensusV1 adds support for Genesis ID in transactions, but does not
// require it (transactions missing a GenesisID value are still allowed).
// It is now deprecated.
const DEPRECATEDConsensusV1 = ConsensusVersion("v1")

// DEPRECATEDConsensusV2 fixes a bug in the agreement protocol where proposalValues
// fail to commit to the original period and sender of a block.
const DEPRECATEDConsensusV2 = ConsensusVersion("v2")

// DEPRECATEDConsensusV3 adds support for fine-grained ephemeral keys.
const DEPRECATEDConsensusV3 = ConsensusVersion("v3")

// DEPRECATEDConsensusV4 adds support for a min balance and a transaction that
// closes out an account.
const DEPRECATEDConsensusV4 = ConsensusVersion("v4")

// DEPRECATEDConsensusV5 sets MinTxnFee to 1000 and fixes a blance lookback bug
const DEPRECATEDConsensusV5 = ConsensusVersion("v5")

// DEPRECATEDConsensusV6 adds support for explicit ephemeral-key parameters
const DEPRECATEDConsensusV6 = ConsensusVersion("v6")

// ConsensusV7 increases MaxBalLookback to 320 in preparation for the twin seeds change.
// ConsensusV7은 트윈 시드 변경에 대비하여 MaxBalLookback을 320으로 늘립니다.
const ConsensusV7 = ConsensusVersion("v7")

// ConsensusV8 uses the new parameters and seed derivation policy from the agreement protocol's security analysis.
// ConsensusV8은 계약 프로토콜의 보안 분석에서 새로운 매개변수와 시드 파생 정책을 사용합니다.
const ConsensusV8 = ConsensusVersion("v8")

// ConsensusV9 increases min balance to 100,000 microAlgos.
// Consensus V9는 최소 잔고를 100,000 마이크로 알고리즘으로 늘립니다.
const ConsensusV9 = ConsensusVersion("v9")

// ConsensusV10 introduces fast partition recovery.
// ConsensusV10은 빠른 파티션 복구를 도입합니다.
const ConsensusV10 = ConsensusVersion("v10")

// ConsensusV11 introduces efficient encoding of SignedTxn using SignedTxnInBlock.
// ConsensusV11은 SignedTxnInBlock을 사용하여 SignedTxn의 효율적인 인코딩을 도입합니다.
const ConsensusV11 = ConsensusVersion("v11")

// ConsensusV12 increases the maximum length of a version string.
// ConsensusV12는 버전 문자열의 최대 길이를 늘립니다.
const ConsensusV12 = ConsensusVersion("v12")

// ConsensusV13 makes the consensus version a meaningful string.
// ConsensusV13은 합의 버전을 의미 있는 문자열로 만듭니다.
const ConsensusV13 = ConsensusVersion(
	// Points to version of the Algorand spec as of May 21, 2019.
	// 2019년 5월 21일 현재 알고랜드 사양의 버전을 가리킵니다.
	"https://github.com/algorand/spec/tree/0c8a9dc44d7368cc266d5407b79fb3311f4fc795",
)

// ConsensusV14 adds tracking of closing amounts in ApplyData, and enables genesis hash in transactions.
// ConsensusV14는 ApplyData에서 마감 금액 추적을 추가하고 트랜잭션에서 제네시스 해시를 활성화합니다.
const ConsensusV14 = ConsensusVersion(
	"https://github.com/algorand/spec/tree/2526b6ae062b4fe5e163e06e41e1d9b9219135a9",
)

// ConsensusV15 adds tracking of reward distributions in ApplyData.
// ConsensusV15는 ApplyData에서 보상 분포 추적을 추가합니다.
const ConsensusV15 = ConsensusVersion(
	"https://github.com/algorand/spec/tree/a26ed78ed8f834e2b9ccb6eb7d3ee9f629a6e622",
)

// ConsensusV16 fixes domain separation in Credentials and requires GenesisHash.
// ConsensusV16은 Credentials의 도메인 분리를 수정하고 GenesisHash가 필요합니다.
const ConsensusV16 = ConsensusVersion(
	"https://github.com/algorand/spec/tree/22726c9dcd12d9cddce4a8bd7e8ccaa707f74101",
)

// ConsensusV17 points to 'final' spec commit for 2019 june release
// ConsensusV17은 2019년 6월 릴리스의 '최종' 사양 커밋을 가리킵니다.
const ConsensusV17 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/5615adc36bad610c7f165fa2967f4ecfa75125f0",
)

// ConsensusV18 points to reward calculation spec commit
// ConsensusV18 포인트 보상 계산 사양 커밋
const ConsensusV18 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/6c6bd668be0ab14098e51b37e806c509f7b7e31f",
)

// ConsensusV19 points to 'final' spec commit for 2019 nov release
// ConsensusV19는 2019년 11월 릴리스에 대한 '최종' 사양 커밋을 가리킵니다.
const ConsensusV19 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/0e196e82bfd6e327994bec373c4cc81bc878ef5c",
)

// ConsensusV20 points to adding the decimals field to assets
// ConsensusV20은 자산에 소수 필드를 추가하는 것을 가리킵니다.
const ConsensusV20 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/4a9db6a25595c6fd097cf9cc137cc83027787eaa",
)

// ConsensusV21 fixes a bug in credential.lowestOutput
// ConsensusV21은 credential.lowestOutput의 버그를 수정합니다.
const ConsensusV21 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/8096e2df2da75c3339986317f9abe69d4fa86b4b",
)

// ConsensusV22 allows tuning the upgrade delay.
// ConsensusV22는 업그레이드 지연을 조정할 수 있습니다.
const ConsensusV22 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/57016b942f6d97e6d4c0688b373bb0a2fc85a1a2",
)

// ConsensusV23 fixes lease behavior.
// ConsensusV23은 임대 동작을 수정합니다.
const ConsensusV23 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/e5f565421d720c6f75cdd186f7098495caf9101f",
)

// ConsensusV24 include the applications, rekeying and teal v2
// ConsensusV24에는 응용 프로그램, rekeying 및 청록 v2가 포함됩니다.
const ConsensusV24 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/3a83c4c743f8b17adfd73944b4319c25722a6782",
)

// ConsensusV25 adds support for AssetCloseAmount in the ApplyData
// ConsensusV25는 ApplyData에서 AssetCloseAmount에 대한 지원을 추가합니다.
const ConsensusV25 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/bea19289bf41217d2c0af30522fa222ef1366466",
)

// ConsensusV26 adds support for TEAL 3, initial rewards calculation and merkle tree hash commitments
// ConsensusV26은 TEAL 3, 초기 보상 계산 및 머클 트리 해시 약정에 대한 지원을 추가합니다.
const ConsensusV26 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/ac2255d586c4474d4ebcf3809acccb59b7ef34ff",
)

// ConsensusV27 updates ApplyDelta.EvalDelta.LocalDeltas format
// ConsensusV27 업데이트 ApplyDelta.EvalDelta.LocalDelta 형식
const ConsensusV27 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/d050b3cade6d5c664df8bd729bf219f179812595",
)

// ConsensusV28 introduces new TEAL features, larger program size, fee pooling and longer asset max URL
// ConsensusV28은 새로운 TEAL 기능, 더 큰 프로그램 크기, 수수료 풀링 및 더 긴 자산 최대 URL을 도입합니다.
const ConsensusV28 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/65b4ab3266c52c56a0fa7d591754887d68faad0a",
)

// ConsensusV29 fixes application update by using ExtraProgramPages in size calculations
// ConsensusV29는 크기 계산에서 ExtraProgramPages를 사용하여 애플리케이션 업데이트를 수정합니다.
const ConsensusV29 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/abc54f79f9ad679d2d22f0fb9909fb005c16f8a1",
)

// ConsensusV30 introduces AVM 1.0 and TEAL 5, increases the app opt in limit to 50, and allows costs to be pooled in grouped stateful transactions.
// ConsensusV30은 AVM 1.0 및 TEAL 5를 도입하고 앱 옵트인 제한을 50으로 늘리고 비용을 그룹화된 상태 저장 트랜잭션으로 풀링할 수 있습니다.
const ConsensusV30 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/bc36005dbd776e6d1eaf0c560619bb183215645c",
)

// ConsensusV31 enables the batch verification for ed25519 signatures, Fix reward calculation issue, introduces the ability to force an expired participation offline, enables TEAL 6 ( AVM 1.1 ) and add support for creating state proof keys.
// ConsensusV31은 ed25519 서명에 대한 일괄 검증을 활성화하고, 보상 계산 문제를 수정하고, 만료된 참여를 오프라인으로 강제 적용하는 기능을 도입하고,
// TEAL 6( AVM 1.1 )을 활성화하고, 상태 증명 키 생성에 대한 지원을 추가합니다.
const ConsensusV31 = ConsensusVersion(
	"https://github.com/algorandfoundation/specs/tree/85e6db1fdbdef00aa232c75199e10dc5fe9498f6",
)

// ConsensusFuture is a protocol that should not appear in any production network, but is used to test features before they are released.
// ConsensusFuture는 프로덕션 네트워크에 표시되어서는 안 되는 프로토콜이지만 출시되기 전에 기능을 테스트하는 데 사용됩니다.
const ConsensusFuture = ConsensusVersion(
	"future",
)

// !!! ********************* !!!
// !!! *** Please update ConsensusCurrentVersion when adding new protocol versions *** !!!
// !!! *** 새 프로토콜 버전을 추가할 때 ConsensusCurrentVersion을 업데이트하십시오 *** !!!
// !!! ********************* !!!

// ConsensusCurrentVersion is the latest version and should be used when a specific version is not provided.
// ConsensusCurrentVersion은 최신 버전이며 특정 버전이 제공되지 않을 때 사용해야 합니다.
const ConsensusCurrentVersion = ConsensusV31

// Error is used to indicate that an unsupported protocol has been detected.
// 오류는 지원되지 않는 프로토콜이 감지되었음을 나타내는 데 사용됩니다.
type Error ConsensusVersion

// Error satisfies builtin interface `error`
// Error 는 내장 인터페이스 `error`를 충족합니다.
func (err Error) Error() string {
	proto := ConsensusVersion(err)
	return fmt.Sprintf("protocol not supported: %s", proto)
}
