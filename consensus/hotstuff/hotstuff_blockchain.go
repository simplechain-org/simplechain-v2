package hotstuff

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

const maxSpeculativeTailDepth = 4096

// storeBlock stores a validated block to the chain
func (h *Hotstuff) storeBlock(header *types.Header, txs []*types.Transaction) (*types.Block, error) {
	// 完整持久化：使用 FinalizeAndAssemble 组装并写入 body/receipts/header
	parent := h.chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return nil, errors.New("parent not found for storeBlock")
	}
	// 基于父状态构造 StateDB
	if h.db == nil {
		return nil, errors.New("db not set")
	}
	sdb := state.NewDatabase(triedb.NewDatabase(h.db, triedb.HashDefaults), nil)
	statedb, err := state.New(parent.Root, sdb)
	if err != nil {
		return nil, fmt.Errorf("open parent state: %w", err)
	}
	body := &types.Body{Transactions: txs}
	// 先用空 receipts，占位由 FinalizeAndAssemble 生成或填充
	receipts := make([]*types.Receipt, 0, len(txs))
	block, receipts, err := h.FinalizeAndAssemble(h.chain, header, statedb, body, receipts, nil)
	if err != nil {
		return nil, fmt.Errorf("finalize and assemble: %w", err)
	}

	// rawdb 完整写入（header/body/receipts），不改 head，仅做持久化
	rawdb.WriteBlock(h.db, block)
	rawdb.WriteReceipts(h.db, block.Hash(), block.NumberU64(), receipts)
	// 维护 number 映射（WriteBlock 已写 Body，HeaderNumber 单独保证）
	rawdb.WriteHeaderNumber(h.db, block.Hash(), block.NumberU64())

	log.Info("Block persisted", "number", block.NumberU64(), "hash", block.Hash(), "txs", len(block.Transactions()))
	return block, nil
}

// tryCommitBlocks attempts to commit blocks that have 3-chain justification
// In Chained HotStuff, when we have 3 consecutive QCs, the grandparent block can be committed
func (h *Hotstuff) tryCommitBlocks(st *hsState, currentHash common.Hash, currentView uint64) error {
	// STRICT CONSECUTIVE VIEW 3-CHAIN RULE (LibraBFT/DiemBFT style):
	//
	// A block B0 can be committed if there exist certified blocks B1 and B2 that satisfy:
	// 1) B0 <- B1 <- B2 (parent-child relationship)
	// 2) round(B0) + 1 = round(B1) (consecutive views)
	// 3) round(B1) + 1 = round(B2) (consecutive views)
	//
	// 好处：
	// - 减少分叉：连续视图强制锁快速前移，唯一化父块选择
	// - 更好的流水线：提交路径时间对齐，未提交尾部更短
	//
	// 注意：
	// - 如果中间视图超时（例如 view 3->4->7），则不满足连续性，需要等待后续连续视图
	// - 一旦满足连续3-chain，会提交B0及其所有未提交祖先（解决"unknown ancestor"问题）

	// 获取当前view的QC和对应区块
	qcCurrent := st.qcsByView[currentView]
	if qcCurrent == nil || !qcCurrent.hasAggregateProof() {
		return nil
	}
	if qcCurrent.BlockHash != currentHash {
		return fmt.Errorf("QC at view %d certifies %s, not requested block %s", currentView, qcCurrent.BlockHash, currentHash)
	}

	blockCurrent := st.proposalsByHashBlock[qcCurrent.BlockHash]
	if blockCurrent == nil {
		return nil
	}

	// In Chained HotStuff, a QC can certify a block from a previous view
	// Example: Block proposed in view 3, but QC formed in view 5 (after timeouts)
	// We care about the QC's view for consecutive view checking
	viewCurrent := currentView

	// 基于区块的父子关系检查3-chain
	// blockCurrent (B2, 刚获得QC) ← blockParent (B1) ← blockGrandparent (B0)
	parentHash := blockCurrent.ParentHash()
	blockParent := st.proposalsByHashBlock[parentHash]
	if blockParent == nil {
		// 父区块不在proposals中，可能已经committed或在链中
		// 从链中获取
		if h.chain != nil {
			parentHeader := h.chain.GetHeaderByHash(parentHash)
			if parentHeader != nil {
				if bc, ok := h.chain.(*core.BlockChain); ok {
					blockParent = bc.GetBlock(parentHash, parentHeader.Number.Uint64())
				}
			}
		}
		if blockParent == nil {
			log.Debug("Parent block not found for 3-chain check",
				"current", blockCurrent.Number(),
				"parentHash", parentHash.Hex()[:8])
			return nil
		}
	}

	grandparentHash := blockParent.ParentHash()
	blockGrandparent := st.proposalsByHashBlock[grandparentHash]
	if blockGrandparent == nil {
		// 祖父区块不在proposals中
		if h.chain != nil {
			grandparentHeader := h.chain.GetHeaderByHash(grandparentHash)
			if grandparentHeader != nil {
				if bc, ok := h.chain.(*core.BlockChain); ok {
					blockGrandparent = bc.GetBlock(grandparentHash, grandparentHeader.Number.Uint64())
				}
			}
		}
		if blockGrandparent == nil {
			log.Debug("Grandparent block not found for 3-chain check",
				"current", blockCurrent.Number(),
				"parent", blockParent.Number(),
				"grandparentHash", grandparentHash.Hex()[:8])
			return nil
		}
	}

	// 检查三个区块是否都有QC
	qcParent := h.findQCForBlock(st, blockParent.Hash())
	qcGrandparent := h.findQCForBlock(st, blockGrandparent.Hash())

	if qcGrandparent == nil {
		log.Debug("Grandparent block has no QC, cannot commit",
			"current", blockCurrent.Number(),
			"parent", blockParent.Number(),
			"grandparent", blockGrandparent.Number())
		return nil
	}

	if qcParent == nil {
		log.Debug("Parent block has no QC, cannot commit grandparent",
			"current", blockCurrent.Number(),
			"parent", blockParent.Number())
		return nil
	}

	// We check if the QCs form a consecutive chain, not the blocks
	viewGrandparent := qcGrandparent.View
	viewParent := qcParent.View
	// viewCurrent is already set to currentView (the QC view)

	// ✅ STRICT CONSECUTIVE VIEW CHECK (LibraBFT/DiemBFT style)
	// Verify: round(QC0) + 1 = round(QC1) AND round(QC1) + 1 = round(QC2)
	if viewGrandparent+1 != viewParent || viewParent+1 != viewCurrent {
		log.Debug("3-chain views not consecutive, cannot commit yet",
			"grandparent", blockGrandparent.Number(),
			"viewGrandparent", viewGrandparent,
			"parent", blockParent.Number(),
			"viewParent", viewParent,
			"current", blockCurrent.Number(),
			"viewCurrent", viewCurrent,
			"expected", fmt.Sprintf("%d->%d->%d", viewGrandparent, viewGrandparent+1, viewGrandparent+2),
			"actual", fmt.Sprintf("%d->%d->%d", viewGrandparent, viewParent, viewCurrent))
		return nil
	}

	// 防重复提交
	if h.isBlockCommitted(st, blockGrandparent.Hash()) {
		log.Debug("Grandparent block already committed",
			"number", blockGrandparent.Number(),
			"hash", blockGrandparent.Hash().Hex()[:8])
		return nil
	}

	// ✅ 3-chain规则满足：
	// 1. blockGrandparent ← blockParent ← blockCurrent (parent-child chain)
	// 2. 三个区块都有QC
	// 3. 视图号严格连续：round(B0)+1 = round(B1), round(B1)+1 = round(B2)
	// 提交blockGrandparent及其未committed的祖先
	log.Info("3-chain rule satisfied with consecutive views",
		"grandparent", blockGrandparent.Number(),
		"viewGrandparent", viewGrandparent,
		"parent", blockParent.Number(),
		"viewParent", viewParent,
		"current", blockCurrent.Number(),
		"viewCurrent", viewCurrent)

	// tryCommitBlocks is called with h.lock held. InsertChain can execute and
	// persist several blocks, so it must not run while the consensus mutex is
	// held. commitBlockWithAncestors uses short lock sections for its snapshots
	// and cleanup, and stateLock serializes concurrent commit attempts.
	err := func() error {
		h.lock.Unlock()
		defer h.lock.Lock()
		return h.commitBlockWithAncestors(st, blockGrandparent)
	}()
	if err != nil {
		return err
	}

	if current := h.getHsState(); current != nil && (current.lockedQC == nil || qcParent.View > current.lockedQC.View) {
		current.lockedQC = qcParent
	}
	log.Info("HotStuff 3-chain commit completed",
		"committed", blockGrandparent.Number(),
		"hash", blockGrandparent.Hash().Hex()[:8],
		"consecutiveViews", fmt.Sprintf("%d->%d->%d", viewGrandparent, viewParent, viewCurrent))
	return nil
}

// findQCForBlock searches for a QC that certifies the given block hash
func (h *Hotstuff) findQCForBlock(st *hsState, blockHash common.Hash) *HsQC {
	// 遍历所有QC，查找certify这个区块的QC
	for _, qc := range st.qcsByView {
		if qc != nil && qc.BlockHash == blockHash && qc.hasAggregateProof() {
			return qc
		}
	}
	return nil
}

// isBlockCommitted checks if a block has already been committed
func (h *Hotstuff) isBlockCommitted(st *hsState, hash common.Hash) bool {
	if st == nil {
		return false
	}
	_, exists := st.committedBlocks[hash]
	return exists
}

func recordHotstuffCommitInsertResult(knownBlockFastPath bool, err error) {
	switch {
	case err == nil && knownBlockFastPath:
		hotstuffCommitFastPathCounter.Inc(1)
	case err == nil:
		hotstuffCommitReexecuteCounter.Inc(1)
	case errors.Is(err, core.ErrKnownBlock):
		hotstuffCommitFastPathCounter.Inc(1)
	default:
		hotstuffCommitInsertErrorCounter.Inc(1)
	}
}

// commitBlock commits a block to the chain
func (h *Hotstuff) commitBlock(block *types.Block) error {
	// ✅ CRITICAL: Genesis block should never be committed via InsertChain
	// It's already in the chain and attempting to insert it causes "unknown ancestor" error
	if block.NumberU64() == 0 {
		log.Warn("Attempted to commit genesis block, skipping InsertChain",
			"hash", block.Hash().Hex()[:8])
		// Still mark as committed in HotStuff state
		h.lock.Lock()
		if st := h.getHsState(); st != nil {
			st.committedBlocks[block.Hash()] = block.NumberU64()
		}
		h.lock.Unlock()
		return nil
	}

	// 1. Insert block into chain (BOTH local and remote blocks!)
	// CRITICAL FIX: Previously only inserted !IsLocalBlock, which meant leader's blocks
	// were never inserted into canonical chain, causing RPC queries to show blockNumber=0
	if h.chain != nil {
		// 类型断言为BlockChain以调用InsertChain方法
		if bc, ok := h.chain.(*core.BlockChain); ok {
			log.Debug("Inserting block into canonical chain via InsertChain",
				"number", block.NumberU64(),
				"hash", block.Hash().Hex()[:8],
				"isLocal", h.IsLocalBlock(block.Header()))

			knownBlockFastPath := bc.HasBlockAndExecutionCache(block.Hash(), block.NumberU64())
			_, err := bc.InsertChain([]*types.Block{block})
			recordHotstuffCommitInsertResult(knownBlockFastPath, err)
			if err != nil {
				// 检查是否为已知区块错误
				if errors.Is(err, core.ErrKnownBlock) {
					// 区块已存在且有效，InsertChain已经正确处理了这种情况
					log.Debug("Block already exists in chain", "hash", block.Hash().Hex(), "number", block.NumberU64())
				} else {
					return fmt.Errorf("failed to insert block into chain: %w", err)
				}
			} else {
				log.Info("Block successfully inserted into canonical chain",
					"number", block.NumberU64(),
					"hash", block.Hash().Hex()[:8],
					"knownBlockFastPath", knownBlockFastPath)
			}
		} else {
			return fmt.Errorf("chain is not a BlockChain instance")
		}
	}

	// 2. Record commit in HotStuff state
	h.lock.Lock()
	if st := h.getHsState(); st != nil {
		st.committedBlocks[block.Hash()] = block.NumberU64()
	}
	h.lock.Unlock()

	// 3. Notify local sealer only if we authored this block
	// Leaders (local author) wait on a hash-scoped waiter in Seal; replicas do not.
	if h.IsLocalBlock(block.Header()) {
		h.notifyCommittedBlock(block)
	}

	log.Debug("Block committed to chain",
		"hash", block.Hash().Hex(),
		"number", block.NumberU64())

	return nil
}

// collectUncommittedAncestors walks only the speculative HotStuff tail. It
// stops as soon as either the current block or its parent is canonical, so a
// restart with an empty committedBlocks map can never trigger a history replay.
func collectUncommittedAncestors(
	target *types.Block,
	isCanonical func(common.Hash, uint64) bool,
	getProposal func(common.Hash) *types.Block,
) (types.Blocks, error) {
	var blocks types.Blocks
	for current := target; current != nil; {
		number := current.NumberU64()
		if isCanonical(current.Hash(), number) {
			break
		}
		if number == 0 {
			return nil, fmt.Errorf("non-canonical genesis in HotStuff proposal chain: %s", current.Hash())
		}
		blocks = append(blocks, current)
		if len(blocks) > maxSpeculativeTailDepth {
			return nil, fmt.Errorf("HotStuff speculative tail exceeds recovery limit %d", maxSpeculativeTailDepth)
		}
		if isCanonical(current.ParentHash(), number-1) {
			break
		}
		parent := getProposal(current.ParentHash())
		if parent == nil {
			return nil, fmt.Errorf("speculative parent %s not found for block %d", current.ParentHash(), number)
		}
		if parent.NumberU64()+1 != number {
			return nil, fmt.Errorf("non-contiguous speculative parent %d for block %d", parent.NumberU64(), number)
		}
		current = parent
	}
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	return blocks, nil
}

// commitBlockWithAncestors commits the short speculative tail ending at block.
// It is called without h.lock held; stateLock serializes concurrent commit
// attempts while all consensus-map access remains in short h.lock sections.
func (h *Hotstuff) commitBlockWithAncestors(_ *hsState, block *types.Block) error {
	if block == nil {
		return errors.New("cannot commit nil block")
	}
	bc, ok := h.chain.(*core.BlockChain)
	if !ok {
		return errors.New("chain is not a BlockChain instance")
	}

	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		return errors.New("HotStuff state is not initialized")
	}
	blocks, err := collectUncommittedAncestors(
		block,
		func(hash common.Hash, number uint64) bool {
			header := bc.GetHeaderByNumber(number)
			return header != nil && header.Hash() == hash
		},
		func(hash common.Hash) *types.Block { return st.proposalsByHashBlock[hash] },
	)
	h.lock.RUnlock()
	if err != nil {
		return err
	}

	if len(blocks) > 0 {
		first := blocks[0]
		parent := bc.GetHeaderByNumber(first.NumberU64() - 1)
		if parent == nil || parent.Hash() != first.ParentHash() {
			return fmt.Errorf("canonical parent %s not found for first speculative block %d", first.ParentHash(), first.NumberU64())
		}
		knownFastPath := true
		for _, candidate := range blocks {
			knownFastPath = knownFastPath && bc.HasBlockAndExecutionCache(candidate.Hash(), candidate.NumberU64())
		}
		log.Info("Batch inserting HotStuff commit tail",
			"count", len(blocks),
			"oldest", blocks[0].NumberU64(),
			"newest", blocks[len(blocks)-1].NumberU64())
		_, insertErr := bc.InsertChain(blocks)
		recordHotstuffCommitInsertResult(knownFastPath, insertErr)
		if insertErr != nil && !errors.Is(insertErr, core.ErrKnownBlock) {
			return fmt.Errorf("failed to insert HotStuff commit tail ending at block %d: %w", block.NumberU64(), insertErr)
		}
	}

	canonical := bc.GetHeaderByNumber(block.NumberU64())
	if canonical == nil || canonical.Hash() != block.Hash() {
		return fmt.Errorf("HotStuff commit target %d is not canonical after insertion", block.NumberU64())
	}

	committed := blocks
	if len(committed) == 0 {
		committed = types.Blocks{block}
	}
	finalized := bc.CurrentFinalBlock()
	if finalized != nil && finalized.Number.Uint64() == block.NumberU64() && finalized.Hash() != block.Hash() {
		return fmt.Errorf("conflicting finalized block at height %d: have %s want %s", block.NumberU64(), finalized.Hash(), block.Hash())
	}
	h.cleanupCommittedProposalState(committed, block.NumberU64())
	if finalized == nil || finalized.Number.Uint64() < block.NumberU64() {
		bc.SetFinalized(block.Header())
	}

	if h.IsLocalBlock(block.Header()) {
		h.notifyCommittedBlock(block)
	}
	log.Info("HotStuff 3-chain target committed and finalized",
		"number", block.NumberU64(), "hash", block.Hash(), "inserted", len(blocks))
	return nil
}

func (h *Hotstuff) cleanupCommittedProposalState(committed types.Blocks, finalizedNumber uint64) {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return
	}
	for _, block := range committed {
		hash := block.Hash()
		st.committedBlocks[hash] = block.NumberU64()
		delete(st.proposalsByHashBlock, hash)
		delete(st.proposalsByHashReceipts, hash)
		delete(st.proposalStates, hash)
		delete(st.prewritten, hash)
		h.proposalBlocksCache.Delete(hash)
	}
	// States at or below a finalized height belong either to the committed
	// path or to a branch that can no longer become canonical.
	for hash := range st.proposalStates {
		if proposal := st.proposalsByHashBlock[hash]; proposal != nil && proposal.NumberU64() <= finalizedNumber {
			delete(st.proposalStates, hash)
		}
	}
}

// recoveryQCFromHeader validates the structural link between a recovered
// proposal and the QC carried in its SyncInfo. Signature and TC verification
// are performed by recoverSpeculativeTail after all referenced blocks are
// available through proposalBlocksCache.
func recoveryQCFromHeader(header, parent *types.Header, chainConfig *params.ChainConfig, allowProofless bool) (*HsQC, uint64, error) {
	if header == nil || parent == nil || header.Number == nil || parent.Number == nil {
		return nil, 0, errors.New("nil header in HotStuff recovery tail")
	}
	if parent.Number.Uint64()+1 != header.Number.Uint64() || header.ParentHash != parent.Hash() {
		return nil, 0, fmt.Errorf("non-contiguous recovered block %d", header.Number.Uint64())
	}
	has, qcView, qcHash, tcView, signersSet, sig, hasProof := parseSyncInfoWithProof(header, chainConfig)
	if !has {
		return nil, 0, fmt.Errorf("recovered block %d has no SyncInfo", header.Number.Uint64())
	}
	if qcHash != parent.Hash() {
		return nil, 0, fmt.Errorf("recovered block %d HighQC target mismatch", header.Number.Uint64())
	}
	parentView := getViewFromHeader(parent, chainConfig)
	if qcView != parentView {
		return nil, 0, fmt.Errorf("recovered block %d HighQC view %d does not match parent view %d", header.Number.Uint64(), qcView, parentView)
	}
	if !hasProof && !allowProofless {
		return nil, 0, fmt.Errorf("recovered block %d has proofless HighQC", header.Number.Uint64())
	}
	expectedView := qcView + 1
	if tcView > 0 && tcView >= qcView {
		expectedView = tcView + 1
	}
	if view := getViewFromHeader(header, chainConfig); view != expectedView || view <= parentView {
		return nil, 0, fmt.Errorf("recovered block %d has non-increasing view %d after parent view %d", header.Number.Uint64(), view, parentView)
	}
	return &HsQC{
		BlockHash: qcHash, View: qcView, SignersSet: signersSet, Sig: common.CopyBytes(sig),
	}, tcView, nil
}

func addRecoveredQC(qcs map[uint64]*HsQC, qc *HsQC) error {
	if qc == nil {
		return errors.New("cannot recover nil QC")
	}
	if existing := qcs[qc.View]; existing != nil && existing.BlockHash != qc.BlockHash {
		return fmt.Errorf("conflicting recovered QCs for view %d: %s and %s", qc.View, existing.BlockHash, qc.BlockHash)
	}
	if existing := qcs[qc.View]; existing == nil || (!existing.hasAggregateProof() && qc.hasAggregateProof()) {
		qcs[qc.View] = qc
	}
	return nil
}

// recoverSpeculativeTail rebuilds the non-canonical state overlay referenced
// by the proof-bearing HighQC restored from the safety WAL. It must be called
// after SetChainReader has installed h.chain and merged snapshot metadata, but
// before restartViewTimeout starts protocol activity.
func (h *Hotstuff) recoverSpeculativeTail() (err error) {
	defer func() {
		if err == nil {
			return
		}
		h.lock.Lock()
		if h.hsWALError == nil {
			h.hsWALError = fmt.Errorf("HotStuff speculative-tail recovery failed: %w", err)
		}
		h.lock.Unlock()
	}()
	if h.chain == nil || h.db == nil || h.chainConfig == nil {
		return errors.New("HotStuff recovery dependencies are not initialized")
	}
	head := h.chain.CurrentHeader()
	if head == nil || head.Number == nil {
		return errors.New("HotStuff recovery has no canonical head")
	}
	active := h.chainConfig.IsHotstuff(head.Number)
	if !active {
		next := new(big.Int).Add(head.Number, common.Big1)
		if !h.chainConfig.IsOnHotstuff(next) {
			return nil
		}
	}

	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		return errors.New("HotStuff state is not initialized")
	}
	if h.hsWALError != nil {
		walErr := h.hsWALError
		h.lock.RUnlock()
		return walErr
	}
	highQC := &HsQC{}
	if st.highQC != nil {
		*highQC = *st.highQC
		highQC.Sig = common.CopyBytes(st.highQC.Sig)
	} else {
		highQC = nil
	}
	h.lock.RUnlock()
	if highQC == nil {
		// A chain configured for HotStuff from genesis has no QC before its first
		// proposal. This is the only active-chain startup state that needs no WAL.
		if active && head.Number.Sign() == 0 && h.chainConfig.HotstuffBlock.Sign() == 0 {
			return nil
		}
		if active {
			return errors.New("active HotStuff chain has no WAL HighQC")
		}
		return nil
	}
	if highQC.BlockHash == (common.Hash{}) {
		return errors.New("WAL HighQC has an empty target")
	}

	number := rawdb.ReadHeaderNumber(h.db, highQC.BlockHash)
	if number == nil {
		return fmt.Errorf("WAL HighQC target %s is not present in rawdb", highQC.BlockHash)
	}
	target := rawdb.ReadBlock(h.db, highQC.BlockHash, *number)
	if target == nil {
		return fmt.Errorf("WAL HighQC target block %s is incomplete in rawdb", highQC.BlockHash)
	}
	tail, err := collectUncommittedAncestors(
		target,
		func(hash common.Hash, number uint64) bool {
			header := h.chain.GetHeaderByNumber(number)
			return header != nil && header.Hash() == hash
		},
		func(hash common.Hash) *types.Block {
			number := rawdb.ReadHeaderNumber(h.db, hash)
			if number == nil {
				return nil
			}
			return rawdb.ReadBlock(h.db, hash, *number)
		},
	)
	if err != nil {
		return fmt.Errorf("collect WAL HighQC tail: %w", err)
	}

	// QC verification needs the non-canonical target headers, but no unverified
	// block is exposed permanently if any later validation or execution fails.
	newCacheEntries := make(map[common.Hash]struct{}, len(tail))
	newStateEntries := make(map[common.Hash]struct{}, len(tail))
	success := false
	defer func() {
		if success {
			return
		}
		h.lock.Lock()
		if st := h.getHsState(); st != nil {
			for hash := range newStateEntries {
				delete(st.proposalStates, hash)
			}
		}
		h.lock.Unlock()
		for hash := range newCacheEntries {
			h.proposalBlocksCache.Delete(hash)
		}
	}()
	for _, block := range tail {
		if cached, loaded := h.proposalBlocksCache.Load(block.Hash()); loaded {
			cachedBlock, ok := cached.(*types.Block)
			if !ok || cachedBlock == nil || cachedBlock.Hash() != block.Hash() {
				return fmt.Errorf("invalid existing proposal cache entry for %s", block.Hash())
			}
		} else {
			h.proposalBlocksCache.Store(block.Hash(), block)
			newCacheEntries[block.Hash()] = struct{}{}
		}
		if !h.hasProposalState(block.Hash()) {
			newStateEntries[block.Hash()] = struct{}{}
		}
	}

	bootstrapTarget := h.isBootstrapHighQC(highQC.BlockHash, highQC.View)
	if !highQC.hasAggregateProof() && !bootstrapTarget {
		return errors.New("WAL HighQC target has no aggregate proof")
	}
	if getViewFromHeader(target.Header(), h.chainConfig) != highQC.View {
		return fmt.Errorf("WAL HighQC view %d does not match target block view %d", highQC.View, getViewFromHeader(target.Header(), h.chainConfig))
	}
	if !h.verifyHighQCPayload(highQC.BlockHash, highQC.View, highQC.SignersSet, highQC.Sig) {
		return errors.New("WAL HighQC aggregate proof is invalid")
	}

	recoveredQCs := make(map[uint64]*HsQC, len(tail)+1)
	recoveredViews := make(map[uint64]common.Hash, len(tail))
	for i, block := range tail {
		var parent *types.Header
		if i == 0 {
			parent = h.chain.GetHeader(block.ParentHash(), block.NumberU64()-1)
		} else {
			parent = tail[i-1].Header()
		}
		if parent == nil {
			return fmt.Errorf("recovered block %d has no parent at the canonical boundary", block.NumberU64())
		}
		allowProofless := h.chainConfig.IsOnHotstuff(block.Number()) && h.isBootstrapHighQC(block.ParentHash(), getViewFromHeader(parent, h.chainConfig))
		qc, tcView, linkErr := recoveryQCFromHeader(block.Header(), parent, h.chainConfig, allowProofless)
		if linkErr != nil {
			return linkErr
		}
		if qc.hasAggregateProof() && !h.verifyHighQCPayload(qc.BlockHash, qc.View, qc.SignersSet, qc.Sig) {
			return fmt.Errorf("recovered block %d carries an invalid HighQC", block.NumberU64())
		}
		tc, tcErr := h.parseTimeoutCert(block.Header())
		if tcErr != nil {
			return fmt.Errorf("parse recovered block %d TimeoutCert: %w", block.NumberU64(), tcErr)
		}
		if tcView > 0 && tcView >= qc.View && (tc == nil || tc.View != tcView) {
			return fmt.Errorf("recovered block %d is missing its declared TimeoutCert", block.NumberU64())
		}
		if tc != nil && (tcView == 0 || tcView < qc.View || tc.View != tcView) {
			return fmt.Errorf("recovered block %d carries an undeclared TimeoutCert", block.NumberU64())
		}
		if tc != nil && !h.verifyTimeoutCert(tc) {
			return fmt.Errorf("recovered block %d carries an invalid TimeoutCert", block.NumberU64())
		}
		if qc.hasAggregateProof() {
			if err := addRecoveredQC(recoveredQCs, qc); err != nil {
				return err
			}
		}
		view := getViewFromHeader(block.Header(), h.chainConfig)
		if existing, ok := recoveredViews[view]; ok && existing != block.Hash() {
			return fmt.Errorf("conflicting recovered proposals for view %d", view)
		}
		recoveredViews[view] = block.Hash()
	}
	if highQC.hasAggregateProof() {
		if err := addRecoveredQC(recoveredQCs, highQC); err != nil {
			return err
		}
	}

	receipts := make(map[common.Hash]types.Receipts, len(tail))
	for _, block := range tail {
		blockReceipts, executeErr := h.executeBlocks(block.Header(), block.Transactions())
		if executeErr != nil {
			return fmt.Errorf("re-execute recovered block %d: %w", block.NumberU64(), executeErr)
		}
		receipts[block.Hash()] = blockReceipts
	}

	h.lock.Lock()
	defer h.lock.Unlock()
	st = h.getHsState()
	if st == nil || st.highQC == nil || st.highQC.BlockHash != highQC.BlockHash || st.highQC.View != highQC.View {
		return errors.New("HotStuff HighQC changed during speculative-tail recovery")
	}
	for view, qc := range recoveredQCs {
		if existing := st.qcsByView[view]; existing != nil && existing.BlockHash != qc.BlockHash {
			return fmt.Errorf("recovered QC conflicts with runtime QC at view %d", view)
		}
	}
	for view, hash := range recoveredViews {
		if existing := st.proposalsByView[view]; existing != nil && existing.Hash() != hash {
			return fmt.Errorf("recovered proposal conflicts with runtime proposal at view %d", view)
		}
	}
	for view, qc := range recoveredQCs {
		if existing := st.qcsByView[view]; existing == nil || (!existing.hasAggregateProof() && qc.hasAggregateProof()) {
			st.qcsByView[view] = qc
		}
	}
	for _, block := range tail {
		header := block.Header()
		hash := block.Hash()
		view := getViewFromHeader(header, h.chainConfig)
		st.proposalsByView[view] = header
		st.proposalsByHash[hash] = header
		st.proposalsByHashBlock[hash] = block
		st.proposalsByHashReceipts[hash] = receipts[hash]
		st.prewritten[hash] = struct{}{}
	}
	success = true
	log.Info("Recovered HotStuff speculative tail from WAL HighQC",
		"target", highQC.BlockHash, "view", highQC.View, "blocks", len(tail), "qcs", len(recoveredQCs))
	return nil
}

// buildBlockAndReceipts finalizes header+txs against parent state without persisting
func (h *Hotstuff) buildBlockAndReceipts(header *types.Header, txs []*types.Transaction) (*types.Block, []*types.Receipt, error) {
	parent := h.chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return nil, nil, errors.New("parent not found for assemble")
	}
	if h.db == nil {
		return nil, nil, errors.New("db not set")
	}
	sdb := state.NewDatabase(triedb.NewDatabase(h.db, triedb.HashDefaults), nil)
	statedb, err := state.New(parent.Root, sdb)
	if err != nil {
		return nil, nil, fmt.Errorf("open parent state: %w", err)
	}

	// DEBUG: Check state root right after creating statedb
	checkRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[buildBlockAndReceipts] State root right after state.New",
		"block", header.Number,
		"parentRoot", parent.Root.Hex()[:10],
		"checkRoot", checkRoot.Hex()[:10],
		"match", checkRoot == parent.Root)

	body := &types.Body{Transactions: txs}
	receipts := make([]*types.Receipt, 0, len(txs))
	block, receipts, err := h.FinalizeAndAssemble(h.chain, header, statedb, body, receipts, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize and assemble: %w", err)
	}
	return block, receipts, nil
}

// prewriteBlock writes header/body/receipts into rawdb without touching canonical head.
// CRITICAL: This function acquires its own lock to protect map access
func (h *Hotstuff) prewriteBlock(hash common.Hash) bool {
	// Serialize metadata prewrites with canonical insertion so a late
	// synthetic-TD batch cannot overwrite InsertChain's canonical metadata.
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		hotstuffPrewriteMissCounter.Inc(1)
		return false
	}
	if _, ok := st.prewritten[hash]; ok {
		h.lock.RUnlock()
		return false
	}
	blk := st.proposalsByHashBlock[hash]
	rcpts, receiptsKnown := st.proposalsByHashReceipts[hash]
	h.lock.RUnlock()

	if blk == nil || !receiptsKnown || h.db == nil {
		hotstuffPrewriteMissCounter.Inc(1)
		return false
	}

	// Persist all metadata in one batch. The state trie deliberately remains
	// speculative and is re-executed by InsertChain after a 3-chain commit.
	batch := h.db.NewBatch()
	rawdb.WriteTd(batch, blk.Hash(), blk.NumberU64(), h.prewriteTotalDifficulty(blk))
	rawdb.WriteBlock(batch, blk)
	rawdb.WriteReceipts(batch, blk.Hash(), blk.NumberU64(), rcpts)
	if h.chainConfig != nil && h.chainConfig.IsCancun(blk.Number(), blk.Time()) {
		rawdb.WriteBlobSidecars(batch, blk.Hash(), blk.NumberU64(), blk.Sidecars())
	}
	if err := batch.Write(); err != nil {
		log.Error("Failed to prewrite HotStuff block", "number", blk.NumberU64(), "hash", blk.Hash(), "err", err)
		return false
	}

	h.lock.Lock()
	st = h.getHsState()
	if st != nil && st.proposalsByHashBlock[hash] != nil {
		st.prewritten[hash] = struct{}{}
	}
	h.lock.Unlock()
	log.Trace("prewrote block to rawdb", "number", blk.NumberU64(), "hash", blk.Hash())
	hotstuffPrewriteBlocksCounter.Inc(1)
	return true
}

func (h *Hotstuff) prewriteTotalDifficulty(block *types.Block) *big.Int {
	if block.NumberU64() == 0 {
		return block.Difficulty()
	}
	var parentTD *big.Int
	if h.chain != nil {
		parentTD = h.chain.GetTd(block.ParentHash(), block.NumberU64()-1)
	}
	if parentTD == nil {
		parentTD = rawdb.ReadTd(h.db, block.ParentHash(), block.NumberU64()-1)
	}
	if parentTD == nil {
		// HotStuff does not use TD for fork choice, but freezer and known-block
		// recovery still require a value for every persisted header.
		parentTD = new(big.Int).SetUint64(block.NumberU64() - 1)
		log.Warn("Synthesizing missing parent TD for HotStuff prewrite",
			"number", block.NumberU64(), "parent", block.ParentHash())
	}
	return new(big.Int).Add(parentTD, block.Difficulty())
}

// GetProposalState returns an isolated copy of a speculative proposal state.
// Callers may mutate the returned StateDB without changing the cached snapshot.
func (h *Hotstuff) GetProposalState(hash common.Hash) *state.StateDB {
	h.lock.RLock()
	st := h.getHsState()
	if st == nil || st.proposalStates == nil {
		h.lock.RUnlock()
		return nil
	}
	cached := st.proposalStates[hash]
	h.lock.RUnlock()
	if cached == nil {
		return nil
	}
	return cached.Copy()
}

func (h *Hotstuff) hasProposalState(hash common.Hash) bool {
	h.lock.RLock()
	defer h.lock.RUnlock()
	st := h.getHsState()
	return st != nil && st.proposalStates != nil && st.proposalStates[hash] != nil
}

func (h *Hotstuff) cacheProposalState(hash common.Hash, statedb *state.StateDB) {
	if statedb == nil {
		return
	}
	cached := statedb.Copy()
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return
	}
	if st.proposalStates == nil {
		st.proposalStates = make(map[common.Hash]*state.StateDB)
	}
	st.proposalStates[hash] = cached
}

// DiscardProposalState removes a failed proposal and any already-cached
// descendants. Protocol paths that reject a proposal after execution can call
// this method to avoid retaining an unusable branch.
func (h *Hotstuff) DiscardProposalState(hash common.Hash) {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil || st.proposalStates == nil {
		return
	}
	// A same-hash retransmission can fail after the original proposal was
	// accepted. Never remove the state backing an already-recorded proposal.
	if st.proposalsByHashBlock[hash] != nil {
		return
	}
	discarded := map[common.Hash]struct{}{hash: {}}
	for changed := true; changed; {
		changed = false
		for candidate := range st.proposalStates {
			proposal := st.proposalsByHashBlock[candidate]
			if proposal == nil {
				continue
			}
			if _, ok := discarded[proposal.ParentHash()]; ok {
				if _, seen := discarded[candidate]; !seen {
					discarded[candidate] = struct{}{}
					changed = true
				}
			}
		}
	}
	for candidate := range discarded {
		delete(st.proposalStates, candidate)
	}
}

// executeBlocks executes the transactions in a block and validates their correctness
// It prevents double-spending and malicious transactions by current view's leader
// Returns only receipts; block should be constructed by caller with received header
func (h *Hotstuff) executeBlocks(header *types.Header, txs []*types.Transaction) (types.Receipts, error) {
	headerHash := header.Hash()
	hadCachedState := h.hasProposalState(headerHash)
	validated := false
	defer func() {
		if !validated && !hadCachedState {
			h.DiscardProposalState(headerHash)
		}
	}()

	// Prefer the in-memory proposal because prewritten blocks are visible via
	// GetHeaderByHash even though their state is intentionally not in pathdb.
	var parent *types.Header
	if parentBlock := h.GetBlockFromState(header.ParentHash); parentBlock != nil {
		parent = parentBlock.Header()
	}
	if parent == nil {
		parent = h.chain.GetHeaderByHash(header.ParentHash)
	}

	if parent == nil {
		log.Error("executeBlocks: parent not found",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return nil, errors.New("parent not found for execution")
	}

	// Every child mutates a Copy. The cached parent snapshot remains immutable
	// and can safely back competing proposals in the same or later views.
	statedb := h.GetProposalState(header.ParentHash)
	var err error
	if statedb != nil {
		log.Debug("Executing proposal from speculative parent state",
			"parent", header.ParentHash, "number", header.Number)
	} else if bc, ok := h.chain.(interface {
		StateAt(root common.Hash) (*state.StateDB, error)
	}); ok {
		statedb, err = bc.StateAt(parent.Root)
		if err != nil {
			log.Error("Parent state not available - possible schema mismatch or database corruption",
				"parent", header.ParentHash.Hex()[:8],
				"parentNum", parent.Number,
				"parentRoot", parent.Root.Hex()[:8],
				"err", err,
				"hint", "ensure all nodes use the same --state.scheme and --db.engine")
			return nil, fmt.Errorf("parent state unavailable (schema mismatch?): %w", err)
		}
	} else {
		if h.db == nil {
			return nil, errors.New("state db not available")
		}
		tdb := state.NewDatabase(triedb.NewDatabase(h.db, triedb.HashDefaults), nil)
		statedb, err = state.New(parent.Root, tdb)
		if err != nil {
			return nil, fmt.Errorf("parent state unavailable: %w", err)
		}
	}

	// 1. DAO Hard Fork
	if h.chainConfig.DAOForkSupport && h.chainConfig.DAOForkBlock != nil && h.chainConfig.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(statedb)
		log.Warn("[executeBlocks] Applied DAO hard fork", "block", header.Number)
	}

	// 2. System Contract Upgrade
	systemcontracts.TryUpdateBuildInSystemContract(h.chainConfig, header.Number, parent.Time, header.Time, statedb, true)

	// DEBUG: State root after fork upgrade (before any transactions)
	afterForkUpgradeRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[executeBlocks] After TryUpdateBuildInSystemContract (atBlockBegin=true)",
		"block", header.Number,
		"hash", header.Hash().Hex()[:10],
		"parentRoot", parent.Root.Hex()[:10],
		"afterForkUpgradeRoot", afterForkUpgradeRoot.Hex()[:10])

	// 步骤1：执行所有普通交易
	gp := new(core.GasPool).AddGas(header.GasLimit)
	receipts := make([]*types.Receipt, 0)
	usedGas := uint64(0)
	signer := types.MakeSigner(h.chainConfig, header.Number, header.Time)

	// 创建 EVM context
	context := core.NewEVMBlockContext(header, chainContext{Chain: h.chain, parlia: h}, nil)
	evm := vm.NewEVM(context, statedb, h.chainConfig, vm.Config{})

	// 3. Beacon Root 处理
	if beaconRoot := header.ParentBeaconRoot; beaconRoot != nil {
		core.ProcessBeaconBlockRoot(*beaconRoot, evm)
		log.Warn("[executeBlocks] Processed beacon root", "block", header.Number, "beaconRoot", beaconRoot.Hex()[:10])
	}

	// 4. Parent Block Hash 处理
	if h.chainConfig.IsPrague(header.Number, header.Time) || h.chainConfig.IsVerkle(header.Number, header.Time) {
		core.ProcessParentBlockHash(header.ParentHash, evm)
		log.Warn("[executeBlocks] Processed parent block hash", "block", header.Number, "parentHash", header.ParentHash.Hex()[:10])
	}

	// DEBUG: State root after all pre-execution system calls
	afterSystemCallsRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[executeBlocks] After all pre-execution system calls",
		"block", header.Number,
		"stateRoot", afterSystemCallsRoot.Hex()[:10])

	// 分离系统交易和普通交易
	// usually do have two tx, one for validator set contract, another for system reward contract.
	systemTxs := make([]*types.Transaction, 0, 2)
	for i, tx := range txs {
		isSystemTx, err := h.IsSystemTransaction(tx, header)
		if err != nil {
			return nil, err
		}
		if isSystemTx {
			// 系统交易跳过，由 Finalize 处理
			systemTxs = append(systemTxs, tx)
			continue
		}
		// 执行普通交易
		msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		statedb.SetTxContext(tx.Hash(), i)
		receipt, err := core.ApplyTransactionWithEVM(msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, &usedGas, evm, core.NewReceiptBloomGenerator())
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)
	}

	// DEBUG: State root after user transactions (before Finalize)
	afterUserTxRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[executeBlocks] After user transactions (before Finalize)",
		"block", header.Number,
		"hash", header.Hash().Hex()[:10],
		"stateRoot", afterUserTxRoot.Hex()[:10],
		"userTxCount", len(txs)-len(systemTxs),
		"systemTxCount", len(systemTxs))

	// 步骤2：调用 Finalize 执行系统交易（slash、奖励分配、validator 更新等）
	// 复制 header 以避免修改原始 header
	headerCopy := types.CopyHeader(header)
	body := &types.Body{Transactions: txs}

	// Finalize 会执行系统交易并追加到 receipts
	err = h.Finalize(h.chain, headerCopy, statedb, &body.Transactions, nil, nil, &receipts, &systemTxs, &usedGas, nil)
	if err != nil {
		log.Error("Finalize failed", "error", err, "block", header.Number, "hash", header.Hash().Hex()[:10])
		return nil, fmt.Errorf("finalize failed: %w", err)
	}

	// DEBUG: State root after Finalize
	afterFinalizeRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[executeBlocks] After Finalize",
		"block", header.Number,
		"stateRoot", afterFinalizeRoot.Hex()[:10])

	// 步骤3：计算最终 root 并完整校验
	stateRoot := statedb.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))

	// DEBUG: Final comparison
	log.Warn("[executeBlocks] Final state root comparison",
		"block", header.Number,
		"got", stateRoot.Hex(),
		"want", header.Root.Hex(),
		"match", stateRoot == header.Root)

	if stateRoot != header.Root {
		return nil, fmt.Errorf("stateRoot mismatch: got %s want %s", stateRoot, header.Root)
	}

	receiptRoot := types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil))
	if receiptRoot != header.ReceiptHash {
		return nil, fmt.Errorf("receiptRoot mismatch: got %s want %s", receiptRoot, header.ReceiptHash)
	}

	txRoot := types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
	if txRoot != header.TxHash {
		return nil, fmt.Errorf("txRoot mismatch: got %s want %s", txRoot, header.TxHash)
	}

	if usedGas != header.GasUsed {
		return nil, fmt.Errorf("gasUsed mismatch: got %d want %d", usedGas, header.GasUsed)
	}

	// 计算 bloom 并校验
	bloom := types.MergeBloom(types.Receipts(receipts))
	if bloom != header.Bloom {
		return nil, fmt.Errorf("bloom mismatch")
	}

	// Keep the validated state in an isolated in-memory overlay. Canonical
	// InsertChain deliberately re-executes and performs the only durable commit.
	h.cacheProposalState(headerHash, statedb)
	validated = true
	return receipts, nil
}

// executeBlocksLightweight 轻量级校验（当父状态不可用时）
// 只做基础校验：签名、gas、tx hash等，不执行交易
func (h *Hotstuff) executeBlocksLightweight(header *types.Header, txs []*types.Transaction) error {
	signer := types.MakeSigner(h.chainConfig, header.Number, header.Time)

	// 校验交易签名和基础属性
	for i, tx := range txs {
		// 检查签名
		if _, err := types.Sender(signer, tx); err != nil {
			return fmt.Errorf("invalid tx signature idx=%d: %w", i, err)
		}
		// 检查 gas（系统交易除外）
		isSystemTx, _ := h.IsSystemTransaction(tx, header)
		if !isSystemTx && tx.Gas() > header.GasLimit {
			return fmt.Errorf("tx gas %d exceeds block gas limit %d", tx.Gas(), header.GasLimit)
		}
	}

	// 校验 TxRoot
	txRoot := types.DeriveSha(types.Transactions(txs), trie.NewStackTrie(nil))
	if txRoot != header.TxHash {
		return fmt.Errorf("txRoot mismatch: got %s want %s", txRoot, header.TxHash)
	}

	log.Debug("Lightweight validation passed (parent state unavailable)",
		"hash", header.Hash(), "txs", len(txs))
	return nil
}

// validateBasicTransaction performs basic validation of a transaction
func (h *Hotstuff) validateBasicTransaction(tx *types.Transaction, header *types.Header) error {
	// Basic transaction validation
	if tx.Value().Sign() < 0 {
		return errors.New("transaction value is negative")
	}

	if tx.Gas() > header.GasLimit {
		return fmt.Errorf("transaction gas %d exceeds block gas limit %d", tx.Gas(), header.GasLimit)
	}

	// Check transaction signature
	sender, err := types.Sender(types.MakeSigner(h.chainConfig, header.Number, header.Time), tx)
	if err != nil {
		return fmt.Errorf("failed to get transaction sender: %w", err)
	}

	// Basic validation passed
	_ = sender // Use sender to avoid unused variable warning
	return nil
}
