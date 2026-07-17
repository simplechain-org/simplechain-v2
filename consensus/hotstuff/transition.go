package hotstuff

import (
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
)

// Transition routes consensus calls to Parlia before HotStuffBlock and to
// HotStuff from HotStuffBlock onward.
type Transition struct {
	parlia   *parlia.Parlia
	hotstuff *Hotstuff
}

var (
	_ consensus.Engine   = (*Transition)(nil)
	_ consensus.PoSA     = (*Transition)(nil)
	_ consensus.HotStuff = (*Transition)(nil)
	_ consensus.HotStuff = (*Hotstuff)(nil)
)

func NewTransition(parliaEngine *parlia.Parlia, hotstuffEngine *Hotstuff) *Transition {
	return &Transition{parlia: parliaEngine, hotstuff: hotstuffEngine}
}

func (t *Transition) Hotstuff() *Hotstuff { return t.hotstuff }
func (t *Transition) Parlia() *parlia.Parlia {
	return t.parlia
}

// SetVotePool injects the shared vote pool into both sides of the transition.
func (t *Transition) SetVotePool(pool consensus.VotePool) {
	t.parlia.VotePool = pool
	t.hotstuff.VotePool = pool
}

// ConsensusAddress returns the validator address shared by both child engines.
// Authorize always configures both engines with the same address.
func (t *Transition) ConsensusAddress() common.Address {
	return t.hotstuff.ConsensusAddress()
}

// EstimateGasReservedForSystemTxs reserves gas according to the engine active
// at the block being assembled.
func (t *Transition) EstimateGasReservedForSystemTxs(chain consensus.ChainHeaderReader, header *types.Header) uint64 {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.EstimateGasReservedForSystemTxs(chain, header)
	}
	return t.parlia.EstimateGasReservedForSystemTxs(chain, header)
}

// BlockInterval returns the interval configured by the active child engine.
func (t *Transition) BlockInterval(chain consensus.ChainHeaderReader, header *types.Header) (uint64, error) {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.BlockInterval(chain, header)
	}
	return t.parlia.BlockInterval(chain, header)
}

// SignRecently preserves Parlia's anti-double-sign scheduling before the
// transition. HotStuff leader/view rules replace it for the next HotStuff block.
func (t *Transition) SignRecently(chain consensus.ChainReader, parent *types.Header) (bool, error) {
	next := new(big.Int).Add(parent.Number, common.Big1)
	if chain.Config().IsHotstuff(next) {
		return false, nil
	}
	return t.parlia.SignRecently(chain, parent)
}

func (t *Transition) IsSlash() bool { return t.parlia.IsSlash() }

// GetProposalState exposes speculative HotStuff state to the miner. The miner
// copies and validates the state before using it for a new proposal.
func (t *Transition) GetProposalState(hash common.Hash) *state.StateDB {
	return t.hotstuff.GetProposalState(hash)
}

func (t *Transition) SetHsNetwork(n interface{}) { t.hotstuff.SetHsNetwork(n) }
func (t *Transition) SetChainReader(chain consensus.ChainHeaderReader) {
	t.hotstuff.SetChainReader(chain)
}
func (t *Transition) SetBLSVoteSigner(vs interface{}) { t.hotstuff.SetBLSVoteSigner(vs) }
func (t *Transition) GetNotifyMinerCh() <-chan struct{} {
	return t.hotstuff.GetNotifyMinerCh()
}
func (t *Transition) IsHotstuffMining(number *big.Int) bool {
	return t.hotstuff.IsHotstuffMining(number)
}
func (t *Transition) HasPendingProposal() bool { return t.hotstuff.HasPendingProposal() }
func (t *Transition) GetCurrentView() uint64   { return t.hotstuff.GetCurrentView() }
func (t *Transition) HasProposalForView(view uint64) bool {
	return t.hotstuff.HasProposalForView(view)
}
func (t *Transition) GetProposalParent(chain consensus.ChainHeaderReader) common.Hash {
	return t.hotstuff.GetProposalParent(chain)
}
func (t *Transition) GetBlockFromState(hash common.Hash) *types.Block {
	return t.hotstuff.GetBlockFromState(hash)
}
func (t *Transition) GetNodeIDsMap() (map[common.Address][]enode.ID, error) {
	return t.hotstuff.GetNodeIDsMap()
}
func (t *Transition) GetNodeIDs() ([]enode.ID, error) { return t.hotstuff.GetNodeIDs() }
func (t *Transition) AddNodeIDs(nodeIDs []enode.ID, nonce uint64) (*types.Transaction, error) {
	return t.hotstuff.AddNodeIDs(nodeIDs, nonce)
}
func (t *Transition) RemoveNodeIDs(nodeIDs []enode.ID, nonce uint64) (*types.Transaction, error) {
	return t.hotstuff.RemoveNodeIDs(nodeIDs, nonce)
}
func (t *Transition) Authorize(val common.Address, signFn func(accounts.Account, string, []byte) ([]byte, error), signTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)) {
	t.parlia.Authorize(val, signFn, signTxFn)
	t.hotstuff.Authorize(val, signFn, signTxFn)
}
func (t *Transition) OnHsProposal(peerID string, pkt *hs.ProposalPacket) error {
	return t.hotstuff.OnHsProposal(peerID, pkt)
}
func (t *Transition) OnHsVote(peerID string, pkt *hs.VotePacket) error {
	return t.hotstuff.OnHsVote(peerID, pkt)
}
func (t *Transition) OnHsNewView(peerID string, pkt *hs.NewViewPacket) error {
	return t.hotstuff.OnHsNewView(peerID, pkt)
}
func (t *Transition) OnHsTimeout(peerID string, pkt *hs.TimeoutPacket) error {
	return t.hotstuff.OnHsTimeout(peerID, pkt)
}
func (t *Transition) OnHsQuorumCert(peerID string, pkt *hs.QuorumCertPacket) error {
	return t.hotstuff.OnHsQuorumCert(peerID, pkt)
}

func (t *Transition) engine(chain consensus.ChainHeaderReader, header *types.Header) consensus.Engine {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff
	}
	return t.parlia
}

func (t *Transition) Author(header *types.Header) (common.Address, error) {
	if t.hotstuff.chainConfig.IsHotstuff(header.Number) {
		return t.hotstuff.Author(header)
	}
	return t.parlia.Author(header)
}

func (t *Transition) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return t.engine(chain, header).VerifyHeader(chain, header)
}

// batchHeaderReader makes already-seen batch headers available to the child
// engine selected for the current header, including across the fork boundary.
type batchHeaderReader struct {
	consensus.ChainHeaderReader
	parents []*types.Header
}

func (r *batchHeaderReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	for i := len(r.parents) - 1; i >= 0; i-- {
		header := r.parents[i]
		if header != nil && header.Number != nil && header.Number.Uint64() == number && header.Hash() == hash {
			return header
		}
	}
	return r.ChainHeaderReader.GetHeader(hash, number)
}

func (r *batchHeaderReader) GetHeaderByNumber(number uint64) *types.Header {
	for i := len(r.parents) - 1; i >= 0; i-- {
		header := r.parents[i]
		if header != nil && header.Number != nil && header.Number.Uint64() == number {
			return header
		}
	}
	return r.ChainHeaderReader.GetHeaderByNumber(number)
}

func (r *batchHeaderReader) GetHeaderByHash(hash common.Hash) *types.Header {
	for i := len(r.parents) - 1; i >= 0; i-- {
		header := r.parents[i]
		if header != nil && header.Hash() == hash {
			return header
		}
	}
	return r.ChainHeaderReader.GetHeaderByHash(hash)
}

func (t *Transition) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	go func() {
		for i, header := range headers {
			reader := consensus.ChainHeaderReader(chain)
			if i > 0 {
				reader = &batchHeaderReader{ChainHeaderReader: chain, parents: headers[:i]}
			}
			select {
			case <-abort:
				return
			case results <- t.VerifyHeader(reader, header):
			}
		}
	}()
	return abort, results
}

func (t *Transition) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	return t.engine(chain, block.Header()).VerifyUncles(chain, block)
}

func (t *Transition) VerifyRequests(header *types.Header, requests [][]byte) error {
	if t.hotstuff.chainConfig.IsHotstuff(header.Number) {
		return t.hotstuff.VerifyRequests(header, requests)
	}
	return t.parlia.VerifyRequests(header, requests)
}

func (t *Transition) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.NextInTurnValidator(chain, header)
	}
	return t.parlia.NextInTurnValidator(chain, header)
}

func (t *Transition) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	return t.engine(chain, header).Prepare(chain, header)
}

func (t *Transition) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB, txs *[]*types.Transaction,
	uncles []*types.Header, withdrawals []*types.Withdrawal, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	return t.engine(chain, header).Finalize(chain, header, state, txs, uncles, withdrawals, receipts, systemTxs, usedGas, tracer)
}

func (t *Transition) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error) {
	return t.engine(chain, header).FinalizeAndAssemble(chain, header, state, body, receipts, tracer)
}

func (t *Transition) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	return t.engine(chain, block.Header()).Seal(chain, block, results, stop)
}

func (t *Transition) SealHash(header *types.Header) common.Hash {
	if t.hotstuff.chainConfig.IsHotstuff(header.Number) {
		return t.hotstuff.SealHash(header)
	}
	return t.parlia.SealHash(header)
}

func (t *Transition) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	next := new(big.Int).Add(parent.Number, common.Big1)
	if chain.Config().IsHotstuff(next) {
		return t.hotstuff.CalcDifficulty(chain, time, parent)
	}
	return t.parlia.CalcDifficulty(chain, time, parent)
}

func (t *Transition) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.Delay(chain, header, leftOver)
	}
	return t.parlia.Delay(chain, header, leftOver)
}

func (t *Transition) Close() error {
	return errors.Join(t.parlia.Close(), t.hotstuff.Close())
}

func (t *Transition) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{{
		Namespace: "parlia",
		Version:   "1.0",
		Service: &transitionAPI{
			chain:       chain,
			transition:  t,
			parliaAPI:   parlia.NewAPI(chain, t.parlia),
			hotstuffAPI: &API{chain: chain, parlia: t.hotstuff},
		},
		Public: false,
	}}
}

func (t *Transition) IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error) {
	if t.hotstuff.chainConfig.IsHotstuff(header.Number) {
		return t.hotstuff.IsSystemTransaction(tx, header)
	}
	return t.parlia.IsSystemTransaction(tx, header)
}

func (t *Transition) IsSystemContract(to *common.Address) bool {
	return t.parlia.IsSystemContract(to) || t.hotstuff.IsSystemContract(to)
}

func (t *Transition) EnoughDistance(chain consensus.ChainReader, header *types.Header) bool {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.EnoughDistance(chain, header)
	}
	return t.parlia.EnoughDistance(chain, header)
}

func (t *Transition) IsLocalBlock(header *types.Header) bool {
	if t.hotstuff.chainConfig.IsHotstuff(header.Number) {
		return t.hotstuff.IsLocalBlock(header)
	}
	return t.parlia.IsLocalBlock(header)
}

func (t *Transition) GetJustifiedNumberAndHash(chain consensus.ChainHeaderReader, headers []*types.Header) (uint64, common.Hash, error) {
	if len(headers) > 0 && headers[len(headers)-1] != nil && chain.Config().IsHotstuff(headers[len(headers)-1].Number) {
		return t.hotstuff.GetJustifiedNumberAndHash(chain, headers)
	}
	return t.parlia.GetJustifiedNumberAndHash(chain, headers)
}

func (t *Transition) GetFinalizedHeader(chain consensus.ChainHeaderReader, header *types.Header) *types.Header {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.GetFinalizedHeader(chain, header)
	}
	return t.parlia.GetFinalizedHeader(chain, header)
}

func (t *Transition) VerifyVote(chain consensus.ChainHeaderReader, vote *types.VoteEnvelope) error {
	if vote == nil || vote.Data == nil {
		return errors.New("invalid vote: missing vote data")
	}
	targetNumber := new(big.Int).SetUint64(vote.Data.TargetNumber)
	if chain.Config().IsHotstuff(targetNumber) {
		return t.hotstuff.VerifyVote(chain, vote)
	}
	return t.parlia.VerifyVote(chain, vote)
}

func (t *Transition) IsActiveValidatorAt(chain consensus.ChainHeaderReader, header *types.Header, checkVoteKeyFn func(*types.BLSPublicKey) bool) bool {
	if chain.Config().IsHotstuff(header.Number) {
		return t.hotstuff.IsActiveValidatorAt(chain, header, checkVoteKeyFn)
	}
	return t.parlia.IsActiveValidatorAt(chain, header, checkVoteKeyFn)
}

func (t *Transition) NextProposalBlock(chain consensus.ChainHeaderReader, header *types.Header, proposer common.Address) (uint64, uint64, error) {
	next := new(big.Int).Add(header.Number, common.Big1)
	if chain.Config().IsHotstuff(next) {
		return t.hotstuff.NextProposalBlock(chain, header, proposer)
	}
	return t.parlia.NextProposalBlock(chain, header, proposer)
}
