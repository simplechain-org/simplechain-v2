package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

type hotstuffForkChoiceTestChain struct {
	config *params.ChainConfig
}

func (c *hotstuffForkChoiceTestChain) Config() *params.ChainConfig { return c.config }
func (c *hotstuffForkChoiceTestChain) Engine() consensus.Engine    { return nil }
func (c *hotstuffForkChoiceTestChain) GetJustifiedNumber(*types.Header) uint64 {
	return 0
}
func (c *hotstuffForkChoiceTestChain) GetTd(common.Hash, uint64) *big.Int { return nil }

func TestHotstuffForkChoiceOnlyExtendsHead(t *testing.T) {
	chain := &hotstuffForkChoiceTestChain{config: &params.ChainConfig{
		Hotstuff:      &params.HotstuffConfig{},
		HotstuffBlock: big.NewInt(10),
	}}
	forkChoice := NewForkChoice(chain)
	current := &types.Header{Number: big.NewInt(10), Extra: []byte("current")}

	tests := []struct {
		name   string
		header *types.Header
		want   bool
	}{
		{
			name: "direct child",
			header: &types.Header{
				Number:     big.NewInt(11),
				ParentHash: current.Hash(),
			},
			want: true,
		},
		{
			name: "sibling",
			header: &types.Header{
				Number:     big.NewInt(10),
				ParentHash: current.ParentHash,
				Extra:      []byte("sibling"),
			},
		},
		{
			name: "side chain child",
			header: &types.Header{
				Number:     big.NewInt(11),
				ParentHash: common.HexToHash("0xdeadbeef"),
			},
		},
		{
			name: "height gap",
			header: &types.Header{
				Number:     big.NewInt(12),
				ParentHash: current.Hash(),
			},
		},
		{
			name: "pre-fork ancestor",
			header: &types.Header{
				Number: big.NewInt(9),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := forkChoice.ReorgNeeded(current, test.header)
			if err != nil {
				t.Fatalf("ReorgNeeded returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("ReorgNeeded = %v, want %v", got, test.want)
			}
			got, err = forkChoice.ReorgNeededWithFastFinality(current, test.header)
			if err != nil {
				t.Fatalf("ReorgNeededWithFastFinality returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("ReorgNeededWithFastFinality = %v, want %v", got, test.want)
			}
		})
	}
}
