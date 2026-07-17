package eth

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/hotstuff"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core/types"
)

type backendTestVotePool struct{}

func (*backendTestVotePool) FetchVoteByBlockHash(common.Hash) []*types.VoteEnvelope { return nil }

func TestSetConsensusVotePoolTransition(t *testing.T) {
	transition := hotstuff.NewTransition(&parlia.Parlia{}, &hotstuff.Hotstuff{})
	pool := &backendTestVotePool{}
	if !setConsensusVotePool(transition, pool) {
		t.Fatal("transition engine rejected vote pool injection")
	}
	if transition.Parlia().VotePool != pool {
		t.Fatal("Parlia child did not receive vote pool")
	}
	if transition.Hotstuff().VotePool != pool {
		t.Fatal("HotStuff child did not receive vote pool")
	}
}
