package hotstuff

import (
	"crypto/ecdsa"
	"encoding/binary"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

type transitionTestChain struct {
	config  *params.ChainConfig
	genesis *types.Header
	current *types.Header
	final   *types.Header
	headers map[common.Hash]*types.Header
}

func (c *transitionTestChain) Config() *params.ChainConfig { return c.config }
func (c *transitionTestChain) GenesisHeader() *types.Header {
	return c.genesis
}
func (c *transitionTestChain) CurrentHeader() *types.Header {
	if c.current != nil {
		return c.current
	}
	return c.genesis
}
func (c *transitionTestChain) CurrentFinalBlock() *types.Header { return c.final }
func (c *transitionTestChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	header := c.headers[hash]
	if header != nil && header.Number.Uint64() == number {
		return header
	}
	return nil
}
func (c *transitionTestChain) GetHeaderByNumber(number uint64) *types.Header {
	for _, header := range c.headers {
		if header.Number.Uint64() == number {
			return header
		}
	}
	return nil
}
func (c *transitionTestChain) GetHeaderByHash(hash common.Hash) *types.Header {
	return c.headers[hash]
}
func (c *transitionTestChain) GetTd(common.Hash, uint64) *big.Int { return nil }
func (c *transitionTestChain) GetHighestVerifiedHeader() *types.Header {
	return c.genesis
}
func (c *transitionTestChain) GetVerifiedBlockByHash(hash common.Hash) *types.Header {
	return c.headers[hash]
}
func (c *transitionTestChain) ChasingHead() *types.Header { return nil }

func TestTransitionAPIsUseSingleForkRouter(t *testing.T) {
	config := &params.ChainConfig{
		Parlia:        &params.ParliaConfig{},
		Hotstuff:      &params.HotstuffConfig{},
		HotstuffBlock: big.NewInt(2),
	}
	genesis := &types.Header{Number: big.NewInt(0), Extra: make([]byte, extraVanity+extraSeal)}
	preFork := &types.Header{
		ParentHash: genesis.Hash(), Number: big.NewInt(1), Extra: make([]byte, extraVanity+extraSeal),
	}
	postFork := &types.Header{
		ParentHash: preFork.Hash(), Number: big.NewInt(2), Extra: make([]byte, extraVanity+extraSeal),
	}
	chain := &transitionTestChain{
		config: config, genesis: genesis, current: postFork, final: preFork,
		headers: map[common.Hash]*types.Header{
			genesis.Hash():  genesis,
			preFork.Hash():  preFork,
			postFork.Hash(): postFork,
		},
	}
	transition := &Transition{
		parlia:   &parlia.Parlia{},
		hotstuff: &Hotstuff{chainConfig: config},
	}
	if _, ok := any(transition.parlia).(consensus.HotStuff); ok {
		t.Fatal("Parlia unexpectedly satisfies the HotStuff marker interface")
	}
	apis := transition.APIs(chain)
	if len(apis) != 1 {
		t.Fatalf("Transition exposed %d APIs, want one fork-aware service", len(apis))
	}
	if apis[0].Namespace != "parlia" || apis[0].Version != "1.0" {
		t.Fatalf("unexpected Transition API registration: %#v", apis[0])
	}
	server := rpc.NewServer()
	defer server.Stop()
	if err := server.RegisterName(apis[0].Namespace, apis[0].Service); err != nil {
		t.Fatalf("register Transition API: %v", err)
	}

	service, ok := apis[0].Service.(*transitionAPI)
	if !ok {
		t.Fatalf("Transition API has type %T, want *transitionAPI", apis[0].Service)
	}
	preForkNumber := rpc.BlockNumber(1)
	postForkNumber := rpc.BlockNumber(2)
	earliest := rpc.EarliestBlockNumber
	safe := rpc.SafeBlockNumber
	finalized := rpc.FinalizedBlockNumber
	for _, test := range []struct {
		name     string
		number   *rpc.BlockNumber
		want     *types.Header
		hotstuff bool
	}{
		{name: "latest", number: nil, want: postFork, hotstuff: true},
		{name: "earliest", number: &earliest, want: genesis},
		{name: "safe-crosses-fork", number: &safe, want: preFork},
		{name: "finalized-crosses-fork", number: &finalized, want: preFork},
		{name: "pre-fork", number: &preForkNumber, want: preFork},
		{name: "post-fork", number: &postForkNumber, want: postFork, hotstuff: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := service.getHeader(test.number)
			if header == nil || header.Hash() != test.want.Hash() {
				t.Fatalf("resolved header %v, want %v", header, test.want.Hash())
			}
			if got := service.isHotstuff(header); got != test.hotstuff {
				t.Fatalf("HotStuff route = %v, want %v", got, test.hotstuff)
			}
		})
	}
}

func TestTransitionVerifyHeadersAcrossFork(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	validator := crypto.PubkeyToAddress(key.PublicKey)
	maxwellTime := uint64(0)
	config := &params.ChainConfig{
		ChainID:       big.NewInt(1913),
		LondonBlock:   big.NewInt(0),
		MaxwellTime:   &maxwellTime,
		Parlia:        &params.ParliaConfig{},
		Hotstuff:      &params.HotstuffConfig{},
		HotstuffBlock: big.NewInt(2),
	}
	genesis := &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(time.Now().Add(-3 * time.Second).Unix()),
		UncleHash:  types.EmptyUncleHash,
		Difficulty: big.NewInt(2),
		GasLimit:   params.GenesisGasLimit,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Extra:      append(make([]byte, extraVanity), validator.Bytes()...),
	}
	genesis.Extra = append(genesis.Extra, make([]byte, extraSeal)...)
	chain := &transitionTestChain{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{genesis.Hash(): genesis},
	}
	db := rawdb.NewMemoryDatabase()
	parliaEngine := parlia.New(config, db, nil, genesis.Hash())
	hotstuffEngine := New(config, db, nil, genesis.Hash(), nil, nil)
	if hotstuffEngine.hsTimer != nil {
		defer hotstuffEngine.hsTimer.Stop()
	}
	transition := NewTransition(parliaEngine, hotstuffEngine)

	parliaHeader := transitionTestSignedHeader(t, config, genesis, 1, validator, key)
	hotstuffHeader := transitionTestSignedHeader(t, config, parliaHeader, 2, validator, key)
	transitionTestAddSyncInfo(hotstuffHeader, parliaHeader.Hash(), 1)
	transitionTestSignHeader(t, config, hotstuffHeader, key)

	_, results := transition.VerifyHeaders(chain, []*types.Header{parliaHeader, hotstuffHeader})
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("header %d failed verification: %v", i+1, err)
		}
	}

	chain.headers[parliaHeader.Hash()] = parliaHeader
	chain.headers[hotstuffHeader.Hash()] = hotstuffHeader
	preForkInterval, err := transition.BlockInterval(chain, parliaHeader)
	if err != nil {
		t.Fatalf("Parlia BlockInterval failed: %v", err)
	}
	postForkInterval, err := transition.BlockInterval(chain, hotstuffHeader)
	if err != nil {
		t.Fatalf("HotStuff BlockInterval failed: %v", err)
	}
	if preForkInterval == 0 || postForkInterval == 0 {
		t.Fatalf("invalid routed block intervals: pre=%d post=%d", preForkInterval, postForkInterval)
	}

	preForkVote := &types.VoteEnvelope{Data: &types.VoteData{
		SourceNumber: 99,
		TargetNumber: parliaHeader.Number.Uint64(),
		TargetHash:   parliaHeader.Hash(),
	}}
	if err := transition.VerifyVote(chain, preForkVote); err == nil || !strings.Contains(err.Error(), "source block mismatch") {
		t.Fatalf("pre-fork vote used wrong verifier: %v", err)
	}
	postForkVote := &types.VoteEnvelope{Data: &types.VoteData{
		SourceNumber: 99,
		TargetNumber: hotstuffHeader.Number.Uint64(),
		TargetHash:   hotstuffHeader.Hash(),
	}}
	if err := transition.VerifyVote(chain, postForkVote); err != nil {
		t.Fatalf("post-fork vote did not use HotStuff verifier: %v", err)
	}
	if err := transition.VerifyVote(chain, nil); err == nil {
		t.Fatal("nil vote was accepted")
	}
	if err := transition.VerifyVote(chain, &types.VoteEnvelope{}); err == nil {
		t.Fatal("vote with nil data was accepted")
	}
}

func TestHotstuffBootstrapInitializesAtRuntimeForkBoundary(t *testing.T) {
	config := &params.ChainConfig{
		Hotstuff: &params.HotstuffConfig{}, HotstuffBlock: big.NewInt(2),
	}
	parent := &types.Header{Number: big.NewInt(1), Extra: make([]byte, extraVanity+extraSeal)}
	chain := &transitionTestChain{
		config:  config,
		genesis: parent,
		headers: map[common.Hash]*types.Header{parent.Hash(): parent},
	}
	h := &Hotstuff{chainConfig: config, chain: chain}
	h.initHsState()
	h.ensureBootstrapAtHead()
	if h._hs.highQC == nil || h._hs.highQC.BlockHash != parent.Hash() || h._hs.highQC.View != 1 {
		t.Fatalf("runtime bootstrap QC not initialized: %#v", h._hs.highQC)
	}
	if h._hs.currentView != 2 {
		t.Fatalf("runtime bootstrap view = %d, want 2", h._hs.currentView)
	}
}

func transitionTestSignedHeader(t *testing.T, config *params.ChainConfig, parent *types.Header, number uint64, validator common.Address, key *ecdsa.PrivateKey) *types.Header {
	t.Helper()
	header := &types.Header{
		ParentHash: parent.Hash(),
		UncleHash:  types.EmptyUncleHash,
		Coinbase:   validator,
		Difficulty: big.NewInt(2),
		Number:     new(big.Int).SetUint64(number),
		GasLimit:   parent.GasLimit,
		Time:       parent.Time + 1,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	if parent.BaseFee != nil {
		header.BaseFee = eip1559.CalcBaseFee(config, parent)
	}
	transitionTestSignHeader(t, config, header, key)
	return header
}

func transitionTestSignHeader(t *testing.T, config *params.ChainConfig, header *types.Header, key *ecdsa.PrivateKey) {
	t.Helper()
	sig, err := crypto.Sign(types.SealHash(header, config.ChainID).Bytes(), key)
	if err != nil {
		t.Fatalf("sign header failed: %v", err)
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
}

func transitionTestAddSyncInfo(header *types.Header, parentHash common.Hash, parentView uint64) {
	seal := append([]byte(nil), header.Extra[len(header.Extra)-extraSeal:]...)
	header.Extra = header.Extra[:len(header.Extra)-extraSeal]
	syncInfo := make([]byte, 1+syncInfoTotalSize)
	syncInfo[0] = hsFlag
	binary.LittleEndian.PutUint64(syncInfo[1:1+viewSize], parentView)
	copy(syncInfo[1+viewSize:1+viewSize+hashSize], parentHash[:])
	header.Extra = append(header.Extra, syncInfo...)
	header.Extra = append(header.Extra, seal...)
}
