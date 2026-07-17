package hotstuff

import (
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func testHotstuffBlock(number uint64, parent common.Hash, marker byte) *types.Block {
	return types.NewBlockWithHeader(&types.Header{
		ParentHash: parent,
		Number:     new(big.Int).SetUint64(number),
		Difficulty: new(big.Int),
		GasLimit:   30_000_000,
		Extra:      []byte{marker},
	})
}

func TestCollectUncommittedAncestorsStopsAtCanonicalParent(t *testing.T) {
	canonical := testHotstuffBlock(1_000_000, common.Hash{}, 0)
	first := testHotstuffBlock(1_000_001, canonical.Hash(), 1)
	target := testHotstuffBlock(1_000_002, first.Hash(), 2)
	proposals := map[common.Hash]*types.Block{first.Hash(): first}
	proposalLookups := 0

	blocks, err := collectUncommittedAncestors(
		target,
		func(hash common.Hash, number uint64) bool {
			return number == canonical.NumberU64() && hash == canonical.Hash()
		},
		func(hash common.Hash) *types.Block {
			proposalLookups++
			return proposals[hash]
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Hash() != first.Hash() || blocks[1].Hash() != target.Hash() {
		t.Fatalf("unexpected speculative tail: %v", blocks)
	}
	if proposalLookups != 1 {
		t.Fatalf("walked past canonical boundary: got %d proposal lookups", proposalLookups)
	}
}

func TestCollectUncommittedAncestorsCanonicalTarget(t *testing.T) {
	target := testHotstuffBlock(42, common.Hash{}, 1)
	lookups := 0
	blocks, err := collectUncommittedAncestors(
		target,
		func(hash common.Hash, number uint64) bool { return hash == target.Hash() && number == 42 },
		func(common.Hash) *types.Block {
			lookups++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 || lookups != 0 {
		t.Fatalf("canonical target should not walk history: blocks=%d lookups=%d", len(blocks), lookups)
	}
}

func TestCommitWaitersAreRoutedByBlockHash(t *testing.T) {
	h := new(Hotstuff)
	first := testHotstuffBlock(1, common.Hash{}, 1)
	second := testHotstuffBlock(1, common.Hash{}, 2)
	firstWaiterA := h.registerCommitWaiter(first.Hash())
	firstWaiterB := h.registerCommitWaiter(first.Hash())
	secondWaiter := h.registerCommitWaiter(second.Hash())
	defer h.unregisterCommitWaiter(first.Hash(), firstWaiterA)
	defer h.unregisterCommitWaiter(first.Hash(), firstWaiterB)
	defer h.unregisterCommitWaiter(second.Hash(), secondWaiter)

	h.notifyCommittedBlock(first)
	for i, waiter := range []chan *types.Block{firstWaiterA, firstWaiterB} {
		select {
		case got := <-waiter:
			if got.Hash() != first.Hash() {
				t.Fatalf("waiter %d received %s, want %s", i, got.Hash(), first.Hash())
			}
		default:
			t.Fatalf("waiter %d did not receive matching commit", i)
		}
	}
	select {
	case got := <-secondWaiter:
		t.Fatalf("unrelated waiter received commit %s", got.Hash())
	default:
	}

	h.notifyCommittedBlock(second)
	select {
	case got := <-secondWaiter:
		if got.Hash() != second.Hash() {
			t.Fatalf("second waiter received %s, want %s", got.Hash(), second.Hash())
		}
	default:
		t.Fatal("second waiter did not receive its commit")
	}
}

func testRecoveryHeader(parent *types.Header, config *params.ChainConfig, proof bool) *types.Header {
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		Difficulty: new(big.Int),
		GasLimit:   parent.GasLimit,
	}
	flag := hsFlag
	size := syncInfoTotalSize
	if proof {
		flag = hsProofFlag
		size = syncInfoProofTotalSize
	}
	syncInfo := make([]byte, 1+size)
	syncInfo[0] = flag
	binary.LittleEndian.PutUint64(syncInfo[1:1+viewSize], getViewFromHeader(parent, config))
	copy(syncInfo[1+viewSize:1+viewSize+hashSize], parent.Hash().Bytes())
	if proof {
		offset := 1 + syncInfoTotalSize
		binary.LittleEndian.PutUint64(syncInfo[offset:offset+countSize], 1)
		syncInfo[offset+countSize] = 1
	}
	header.Extra = append(make([]byte, extraVanity), syncInfo...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)
	return header
}

func TestRecoveryQCFromHeaderRequiresContinuousProof(t *testing.T) {
	config := testHotstuffConfig()
	parent := testHotstuffBlock(10, common.Hash{}, 1).Header()
	header := testRecoveryHeader(parent, config, true)
	qc, tcView, err := recoveryQCFromHeader(header, parent, config, false)
	if err != nil {
		t.Fatal(err)
	}
	if qc.BlockHash != parent.Hash() || qc.View != getViewFromHeader(parent, config) || !qc.hasAggregateProof() || tcView != 0 {
		t.Fatalf("unexpected recovered QC: %#v tcView=%d", qc, tcView)
	}

	proofless := testRecoveryHeader(parent, config, false)
	if _, _, err := recoveryQCFromHeader(proofless, parent, config, false); err == nil {
		t.Fatal("proofless non-bootstrap recovery link was accepted")
	}
	if _, _, err := recoveryQCFromHeader(proofless, parent, config, true); err != nil {
		t.Fatalf("bootstrap proofless link was rejected: %v", err)
	}

	wrongParent := types.CopyHeader(parent)
	wrongParent.Extra = append(wrongParent.Extra, 0xff)
	if _, _, err := recoveryQCFromHeader(header, wrongParent, config, false); err == nil {
		t.Fatal("discontinuous recovery link was accepted")
	}
}

func TestRecoverSpeculativeTailFailsVotingClosedWithoutHighQC(t *testing.T) {
	config := &params.ChainConfig{
		HotstuffBlock: big.NewInt(0),
		Hotstuff:      &params.HotstuffConfig{},
	}
	head := testHotstuffBlock(1, common.Hash{}, 1).Header()
	chain := &transitionTestChain{
		config:  config,
		genesis: head,
		headers: map[common.Hash]*types.Header{head.Hash(): head},
	}
	h := &Hotstuff{chainConfig: config, chain: chain, db: rawdb.NewMemoryDatabase(), _hs: &hsState{}}
	if err := h.recoverSpeculativeTail(); err == nil {
		t.Fatal("active recovery without a WAL HighQC succeeded")
	}
	if h.hsWALError == nil {
		t.Fatal("recovery failure did not disable voting")
	}
}

func TestRecoverSpeculativeTailAllowsFreshGenesisBootstrap(t *testing.T) {
	config := &params.ChainConfig{
		HotstuffBlock: big.NewInt(0),
		Hotstuff:      &params.HotstuffConfig{},
	}
	genesis := testHotstuffBlock(0, common.Hash{}, 1).Header()
	chain := &transitionTestChain{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{genesis.Hash(): genesis},
	}
	h := &Hotstuff{chainConfig: config, chain: chain, db: rawdb.NewMemoryDatabase(), _hs: &hsState{}}
	if err := h.recoverSpeculativeTail(); err != nil {
		t.Fatalf("fresh HotStuff genesis bootstrap failed: %v", err)
	}
	if h.hsWALError != nil {
		t.Fatalf("fresh genesis disabled voting: %v", h.hsWALError)
	}
}

func TestProposalStateCopiesAreIsolated(t *testing.T) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	addr := common.Address{1}
	statedb.SetNonce(addr, 7, tracing.NonceChangeUnspecified)
	hash := common.Hash{1}
	h := &Hotstuff{_hs: &hsState{proposalStates: make(map[common.Hash]*state.StateDB)}}
	h.cacheProposalState(hash, statedb)

	statedb.SetNonce(addr, 8, tracing.NonceChangeUnspecified)
	child := h.GetProposalState(hash)
	child.SetNonce(addr, 9, tracing.NonceChangeUnspecified)
	secondChild := h.GetProposalState(hash)
	if nonce := secondChild.GetNonce(addr); nonce != 7 {
		t.Fatalf("cached proposal state was mutated through a copy: have %d want 7", nonce)
	}
}

func TestDiscardProposalStateOnlyRemovesUnrecordedBranch(t *testing.T) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	root := testHotstuffBlock(1, common.Hash{}, 1)
	child := testHotstuffBlock(2, root.Hash(), 2)
	h := &Hotstuff{_hs: &hsState{
		proposalStates:       map[common.Hash]*state.StateDB{root.Hash(): statedb.Copy(), child.Hash(): statedb.Copy()},
		proposalsByHashBlock: map[common.Hash]*types.Block{child.Hash(): child},
	}}
	h.DiscardProposalState(root.Hash())
	if len(h._hs.proposalStates) != 0 {
		t.Fatalf("unrecorded branch was not discarded: %d states remain", len(h._hs.proposalStates))
	}

	h._hs.proposalStates[root.Hash()] = statedb.Copy()
	h._hs.proposalsByHashBlock[root.Hash()] = root
	h.DiscardProposalState(root.Hash())
	if h._hs.proposalStates[root.Hash()] == nil {
		t.Fatal("accepted proposal state was discarded by a retransmission failure")
	}
}

func TestPrewriteBlockPersistsCompleteMetadataAndTD(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	parent := testHotstuffBlock(7, common.Hash{}, 1)
	block := testHotstuffBlock(8, parent.Hash(), 2)
	rawdb.WriteTd(db, parent.Hash(), parent.NumberU64(), big.NewInt(11))
	h := &Hotstuff{
		db: db,
		_hs: &hsState{
			proposalsByHashBlock:    map[common.Hash]*types.Block{block.Hash(): block},
			proposalsByHashReceipts: map[common.Hash]types.Receipts{block.Hash(): {}},
			prewritten:              make(map[common.Hash]struct{}),
		},
	}
	if !h.prewriteBlock(block.Hash()) {
		t.Fatal("prewrite failed")
	}
	if stored := rawdb.ReadBlock(db, block.Hash(), block.NumberU64()); stored == nil {
		t.Fatal("block was not persisted")
	}
	if number := rawdb.ReadHeaderNumber(db, block.Hash()); number == nil || *number != block.NumberU64() {
		t.Fatalf("header number mapping missing: %v", number)
	}
	if td := rawdb.ReadTd(db, block.Hash(), block.NumberU64()); td == nil || td.Cmp(big.NewInt(11)) != 0 {
		t.Fatalf("unexpected total difficulty: %v", td)
	}
	if receipts := rawdb.ReadReceiptsRLP(db, block.Hash(), block.NumberU64()); len(receipts) == 0 {
		t.Fatal("receipts metadata was not persisted")
	}
}

func TestDurableHotstuffFinalizedMarkerSurvivesEngineRestart(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	genesis := testHotstuffBlock(0, common.Hash{}, 1).Header()
	finalized := testHotstuffBlock(1, genesis.Hash(), 2).Header()
	config := &params.ChainConfig{Hotstuff: &params.HotstuffConfig{}, HotstuffBlock: big.NewInt(0)}
	chain := &transitionTestChain{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{
			genesis.Hash():   genesis,
			finalized.Hash(): finalized,
		},
	}
	rawdb.WriteHeader(db, finalized)
	rawdb.WriteFinalizedBlockHash(db, finalized.Hash())

	restarted := &Hotstuff{chainConfig: config, chain: chain, db: db, _hs: &hsState{}}
	got := restarted.GetFinalizedHeader(chain, finalized)
	if got == nil || got.Hash() != finalized.Hash() {
		t.Fatalf("durable finalized marker was not restored: got %#v want %s", got, finalized.Hash())
	}
}

func TestDurableHotstuffFinalizedMarkerAnswersHistoricalQueriesAfterRestart(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	genesis := testHotstuffBlock(0, common.Hash{}, 1).Header()
	first := testHotstuffBlock(1, genesis.Hash(), 2).Header()
	second := testHotstuffBlock(2, first.Hash(), 3).Header()
	finalized := testHotstuffBlock(3, second.Hash(), 4).Header()
	side := testHotstuffBlock(1, genesis.Hash(), 9).Header()
	config := &params.ChainConfig{Hotstuff: &params.HotstuffConfig{}, HotstuffBlock: big.NewInt(0)}
	chain := &transitionTestChain{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{
			genesis.Hash():   genesis,
			first.Hash():     first,
			second.Hash():    second,
			finalized.Hash(): finalized,
			side.Hash():      side,
		},
	}
	rawdb.WriteHeader(db, finalized)
	rawdb.WriteFinalizedBlockHash(db, finalized.Hash())

	restarted := &Hotstuff{chainConfig: config, chain: chain, db: db, _hs: &hsState{}}
	if got := restarted.GetFinalizedHeader(chain, first); got == nil || got.Hash() != first.Hash() {
		t.Fatalf("historical canonical head was not recognized as finalized: got %#v want %s", got, first.Hash())
	}
	number, hash, err := restarted.GetJustifiedNumberAndHash(chain, []*types.Header{first})
	if err != nil {
		t.Fatal(err)
	}
	if number != first.Number.Uint64() || hash != first.Hash() {
		t.Fatalf("historical justified result = (%d, %s), want (%d, %s)", number, hash, first.Number.Uint64(), first.Hash())
	}
	if got := restarted.GetFinalizedHeader(chain, side); got == nil || got.Hash() != genesis.Hash() {
		t.Fatalf("non-canonical historical branch was treated as finalized: got %#v", got)
	}
}
