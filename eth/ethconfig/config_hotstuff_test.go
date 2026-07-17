package ethconfig

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/hotstuff"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/params"
)

func TestCreatePureHotstuffEngineWithoutTTD(t *testing.T) {
	config := &params.ChainConfig{
		ChainID:       big.NewInt(1),
		Parlia:        &params.ParliaConfig{},
		Hotstuff:      &params.HotstuffConfig{},
		HotstuffBlock: big.NewInt(0),
	}
	engine, err := CreateConsensusEngine(config, rawdb.NewMemoryDatabase(), nil, common.Hash{}, nil, nil)
	if err != nil {
		t.Fatalf("CreateConsensusEngine returned error: %v", err)
	}
	defer engine.Close()
	hs, ok := engine.(*hotstuff.Hotstuff)
	if !ok {
		t.Fatalf("engine type = %T, want *hotstuff.Hotstuff", engine)
	}
	if hs == nil {
		t.Fatal("created nil HotStuff engine")
	}
}

func TestCreateHotstuffEngineRejectsIncompleteConfig(t *testing.T) {
	tests := []struct {
		name   string
		config *params.ChainConfig
	}{
		{
			name: "missing parlia parameters",
			config: &params.ChainConfig{
				ChainID: big.NewInt(1), Hotstuff: &params.HotstuffConfig{}, HotstuffBlock: big.NewInt(0),
			},
		},
		{
			name: "missing activation block",
			config: &params.ChainConfig{
				ChainID: big.NewInt(1), Parlia: &params.ParliaConfig{}, Hotstuff: &params.HotstuffConfig{},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if engine, err := CreateConsensusEngine(test.config, rawdb.NewMemoryDatabase(), nil, common.Hash{}, nil, nil); err == nil {
				if engine != nil {
					engine.Close()
				}
				t.Fatal("incomplete HotStuff config was accepted")
			}
		})
	}
}
