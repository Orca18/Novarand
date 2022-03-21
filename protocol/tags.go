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

// Tag represents a message type identifier.  Messages have a Tag field. Handlers can register to a given Tag.
// e.g., the agreement service can register to handle agreements with the Agreement tag.
type Tag string

// Tags, in lexicographic sort order of tag values to avoid duplicates.
// These tags must not contain a comma character because lists of tags
// are encoded using a comma separator (see network/msgOfInterest.go).
// The tags must be 2 bytes long.
// ==========================================
// 태그, 중복을 피하기 위해 태그 값의 사전식 정렬 순서.
// 태그 목록은 쉼표 구분 기호를 사용하여 인코딩되기 때문에
// 이러한 태그에는 쉼표 문자가 포함되지 않아야 합니다(network/msgOfInterest.go 참조).
// 태그의 길이는 2바이트여야 합니다.
const (
	UnknownMsgTag      Tag = "??"
	AgreementVoteTag   Tag = "AV"
	CompactCertSigTag  Tag = "CS"
	MsgOfInterestTag   Tag = "MI"
	MsgDigestSkipTag   Tag = "MS"
	NetPrioResponseTag Tag = "NP"
	PingTag            Tag = "pi"
	PingReplyTag       Tag = "pj"
	ProposalPayloadTag Tag = "PP"
	TopicMsgRespTag    Tag = "TS"
	TxnTag             Tag = "TX"
	UniCatchupReqTag   Tag = "UC" //Replaced by UniEnsBlockReqTag. Only for backward compatibility.
	UniEnsBlockReqTag  Tag = "UE"
	//UniEnsBlockResTag  Tag = "US" was used for wsfetcherservice
	//UniCatchupResTag   Tag = "UT" was used for wsfetcherservice
	VoteBundleTag Tag = "VB"
)
