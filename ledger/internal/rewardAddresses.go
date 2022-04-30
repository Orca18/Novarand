package internal

import (
	"github.com/Orca18/novarand/data/basics"
)

/*
Rewards할 Cert Vote 주소를 나타내는 배열
*/
type rewardAddresses struct {
	CertVoteAddresses []basics.Address
}

func (ra *rewardAddresses) SetCertVoteAddresses(rewardAddresses []basics.Address) {
	ra.CertVoteAddresses = rewardAddresses
}

func (ra *rewardAddresses) GetCertVoteAddresses() []basics.Address {
	return ra.CertVoteAddresses
}
