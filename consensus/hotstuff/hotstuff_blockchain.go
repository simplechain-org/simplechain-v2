package hotstuff

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// getGoroutineID returns the ID of the current goroutine for debugging
func getGoroutineID() uint64 {
	defer func() {
		recover() // Recover from any panic in goroutine ID extraction
	}()
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	// Stack trace format: "goroutine 123 [running]:"
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	idx := bytes.IndexByte(b, ' ')
	if idx == -1 {
		return 0 // Failed to parse, return 0
	}
	b = b[:idx]
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

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
	if qcCurrent == nil {
		return nil
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

	if err := h.commitBlockWithAncestors(st, blockGrandparent); err != nil {
		return err
	}

	st.lockedQC = qcParent
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
		if qc != nil && qc.BlockHash == blockHash {
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

// commitBlock commits a block to the chain
func (h *Hotstuff) commitBlock(block *types.Block) error {
	// ✅ CRITICAL: Genesis block should never be committed via InsertChain
	// It's already in the chain and attempting to insert it causes "unknown ancestor" error
	if block.NumberU64() == 0 {
		log.Warn("Attempted to commit genesis block, skipping InsertChain",
			"hash", block.Hash().Hex()[:8])
		// Still mark as committed in HotStuff state
		if st := h.getHsState(); st != nil {
			st.committedBlocks[block.Hash()] = block.NumberU64()
		}
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

			_, err := bc.InsertChain([]*types.Block{block})
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
					"hash", block.Hash().Hex()[:8])
			}
		} else {
			return fmt.Errorf("chain is not a BlockChain instance")
		}
	}

	// 2. Record commit in HotStuff state
	if st := h.getHsState(); st != nil {
		st.committedBlocks[block.Hash()] = block.NumberU64()
	}

	// 3. Notify local sealer only if we authored this block
	// Leaders (local author) wait on commitCh in Seal; replicas do not.
	if h.IsLocalBlock(block.Header()) {
		select {
		case h.commitCh <- block:
			log.Debug("Committed block sent to commit channel",
				"hash", block.Hash().Hex(),
				"number", block.NumberU64())
		default:
			log.Warn("Commit channel is full, cannot send committed block",
				"hash", block.Hash().Hex(),
				"number", block.NumberU64())
		}
	}

	log.Debug("Block committed to chain",
		"hash", block.Hash().Hex(),
		"number", block.NumberU64())

	return nil
}

// commitBlockWithAncestors commits a block and all its uncommitted ancestors
// This fixes the "unknown ancestor" error when view timeouts cause blocks to be skipped
func (h *Hotstuff) commitBlockWithAncestors(st *hsState, block *types.Block) error {
	if block == nil {
		return fmt.Errorf("cannot commit nil block")
	}

	log.Info("commitBlockWithAncestors: acquiring stateLock",
		"block", block.NumberU64(),
		"hash", block.Hash().Hex()[:8],
		"goroutine", getGoroutineID())

	// CRITICAL FIX: Acquire state lock at the start to prevent concurrent state operations
	// This prevents both:
	// 1. Concurrent InsertChain calls causing "parent diff layer is stale" panics
	// 2. Conflicts with executeBlocks (OnHsProposal) which also modifies state
	h.stateLock.Lock()
	log.Info("commitBlockWithAncestors: acquired stateLock",
		"block", block.NumberU64(),
		"hash", block.Hash().Hex()[:8],
		"goroutine", getGoroutineID())
	defer func() {
		h.stateLock.Unlock()
		log.Info("commitBlockWithAncestors: released stateLock",
			"block", block.NumberU64(),
			"hash", block.Hash().Hex()[:8],
			"goroutine", getGoroutineID())
	}()

	// 收集所有未提交的祖先区块
	var uncommittedAncestors []*types.Block

	// 从当前区块向上追溯，直到找到已提交的区块或genesis
	currentBlock := block
	for currentBlock != nil {
		// 检查当前区块是否已提交（在 HotStuff state 中）
		if h.isBlockCommitted(st, currentBlock.Hash()) {
			break
		}

		// ✅ CRITICAL: 如果是 genesis 区块，停止追溯
		// Genesis 区块不需要提交（已经在链中），也不应该调用 InsertChain
		if currentBlock.NumberU64() == 0 {
			log.Debug("Reached genesis block, stopping ancestor search",
				"hash", currentBlock.Hash().Hex()[:8])
			break
		}

		// ✅ CRITICAL FIX: 不再检查 GetHeaderByHash，因为它只能判断 rawdb，不能判断 canonical chain
		// prewriteBlock 会将区块写入 rawdb，但不会更新 canonical chain
		// 导致 GetHeaderByHash 返回非 nil，但 RPC 查询不到区块
		// 解决方案：直接添加到未提交列表，让 commitBlock 去调用 InsertChain 更新 canonical chain
		// commitBlock 内部已处理 ErrKnownBlock，不会重复插入

		// 添加到未提交列表
		uncommittedAncestors = append(uncommittedAncestors, currentBlock)

		// 查找父区块
		parentHash := currentBlock.ParentHash()

		// 首先尝试从HotStuff state中获取父区块
		parentBlock := st.proposalsByHashBlock[parentHash]

		// 如果HotStuff state中没有，尝试从链中获取
		if parentBlock == nil && h.chain != nil {
			if bc, ok := h.chain.(*core.BlockChain); ok {
				parentHeader := h.chain.GetHeaderByHash(parentHash)
				if parentHeader != nil {
					parentBlock = bc.GetBlock(parentHash, parentHeader.Number.Uint64())
				}
			}
		}

		// 如果找不到父区块，说明父区块已经在链中或不存在
		if parentBlock == nil {
			// 检查父区块是否在链中
			if h.chain != nil {
				if parentHeader := h.chain.GetHeaderByHash(parentHash); parentHeader != nil {
					log.Debug("Parent block found in chain, stopping ancestor search",
						"parentNumber", parentHeader.Number.Uint64(),
						"parentHash", parentHash.Hex()[:8])
					break
				}
			}
			// 如果父区块既不在HotStuff state也不在链中，这是一个错误
			return fmt.Errorf("parent block not found: %s (for block %d)",
				parentHash.Hex()[:8], currentBlock.NumberU64())
		}

		currentBlock = parentBlock
	}

	// 如果没有未提交的祖先，直接返回
	if len(uncommittedAncestors) == 0 {
		log.Debug("No uncommitted ancestors found, block already committed",
			"number", block.NumberU64(),
			"hash", block.Hash().Hex()[:8])
		return nil
	}

	// 反转列表，使其从最老的祖先到最新的区块
	for i, j := 0, len(uncommittedAncestors)-1; i < j; i, j = i+1, j-1 {
		uncommittedAncestors[i], uncommittedAncestors[j] = uncommittedAncestors[j], uncommittedAncestors[i]
	}

	log.Info("Committing block with ancestors",
		"targetBlock", block.NumberU64(),
		"ancestorCount", len(uncommittedAncestors),
		"oldestAncestor", uncommittedAncestors[0].NumberU64(),
		"newestAncestor", uncommittedAncestors[len(uncommittedAncestors)-1].NumberU64())

	// Batch insert all ancestors at once to avoid multiple snapshot.Cap operations
	// Multiple sequential InsertChain calls cause "parent diff layer is stale" panics
	// because each InsertChain triggers snapshot.Cap, which flattens diff layers
	if h.chain != nil {
		if bc, ok := h.chain.(*core.BlockChain); ok {
			// Check if blocks are in CANONICAL chain (not just in rawdb)
			var blocksToInsert []*types.Block
			foundInChain := false
			for i, ancestor := range uncommittedAncestors {
				ancestorNumber := ancestor.NumberU64()
				canonicalHeader := bc.GetHeaderByNumber(ancestorNumber)

				if canonicalHeader != nil && canonicalHeader.Hash() == ancestor.Hash() {
					log.Info("Ancestor block already in canonical chain, truncating ancestor list from here",
						"number", ancestorNumber,
						"hash", ancestor.Hash().Hex()[:8],
						"remainingCount", len(uncommittedAncestors)-i-1)
					if i+1 < len(uncommittedAncestors) {
						blocksToInsert = uncommittedAncestors[i+1:]
					}
					foundInChain = true
					break
				}
			}

			if !foundInChain {
				blocksToInsert = uncommittedAncestors
			}

			if len(blocksToInsert) == 0 {
				log.Warn("All ancestor blocks already in canonical chain (double-check result)",
					"targetBlock", block.NumberU64(),
					"currentChainHead", bc.CurrentBlock().Number.Uint64())
				if st := h.getHsState(); st != nil {
					for _, ancestor := range uncommittedAncestors {
						st.committedBlocks[ancestor.Hash()] = ancestor.NumberU64()
					}
				}
				return nil
			}

			log.Info("Batch inserting ancestor blocks into canonical chain",
				"count", len(blocksToInsert),
				"oldest", blocksToInsert[0].NumberU64(),
				"newest", blocksToInsert[len(blocksToInsert)-1].NumberU64(),
				"currentChainHead", bc.CurrentBlock().Number.Uint64())

			// Verify first block's parent is in chain before batch insert
			firstBlock := blocksToInsert[0]
			firstParentHeader := bc.GetHeaderByNumber(firstBlock.NumberU64() - 1)
			if firstParentHeader == nil || firstParentHeader.Hash() != firstBlock.ParentHash() {
				log.Error("First block's parent not in canonical chain, cannot batch insert",
					"firstBlock", firstBlock.NumberU64(),
					"parentHash", firstBlock.ParentHash().Hex()[:8],
					"expectedParentNumber", firstBlock.NumberU64()-1)

				// This should not happen if our logic is correct
				// Try sequential insert with better error handling
				log.Warn("Attempting sequential insert despite parent chain issue")

				for i, ancestor := range blocksToInsert {
					parentHeader := bc.GetHeaderByNumber(ancestor.NumberU64() - 1)
					if parentHeader == nil {
						return fmt.Errorf("cannot insert block %d: parent number %d not in canonical chain",
							ancestor.NumberU64(), ancestor.NumberU64()-1)
					}
					if parentHeader.Hash() != ancestor.ParentHash() {
						return fmt.Errorf("cannot insert block %d: parent hash mismatch (expected %s, got %s)",
							ancestor.NumberU64(),
							ancestor.ParentHash().Hex()[:8],
							parentHeader.Hash().Hex()[:8])
					}

					_, insertErr := bc.InsertChain(types.Blocks{ancestor})
					if insertErr != nil {
						if errors.Is(insertErr, core.ErrKnownBlock) {
							log.Debug("Ancestor block already exists in chain",
								"number", ancestor.NumberU64())
							continue
						}
						return fmt.Errorf("failed to insert ancestor block %d: %w", ancestor.NumberU64(), insertErr)
					}
					log.Debug("Ancestor block inserted",
						"number", ancestor.NumberU64(),
						"progress", fmt.Sprintf("%d/%d", i+1, len(blocksToInsert)))
					if i < len(blocksToInsert)-1 {
						time.Sleep(1000 * time.Millisecond)
					}
				}
				log.Info("All blocks inserted sequentially after parent verification")
				goto commitComplete
			}

			// Batch insert causes "parent diff layer is stale" panics in HotStuff mode
			// due to fast block production and concurrent StateDB.Commit → snap.Cap calls.
			// Sequential insertion with delays ensures snapshot operations complete before the next insert.
			log.Info("Sequentially inserting ancestor blocks to avoid snapshot conflicts",
				"count", len(blocksToInsert))

			for i, ancestor := range blocksToInsert {
				_, insertErr := bc.InsertChain(types.Blocks{ancestor})
				if insertErr != nil {
					if errors.Is(insertErr, core.ErrKnownBlock) {
						log.Debug("Ancestor block already exists in chain",
							"number", ancestor.NumberU64(),
							"hash", ancestor.Hash().Hex()[:8])
						continue
					}
					return fmt.Errorf("failed to insert ancestor block %d: %w", ancestor.NumberU64(), insertErr)
				}
				log.Info("Ancestor block inserted",
					"number", ancestor.NumberU64(),
					"hash", ancestor.Hash().Hex()[:8],
					"progress", fmt.Sprintf("%d/%d", i+1, len(blocksToInsert)))

				// Delay between inserts to allow snapshot Cap operations to complete
				// The "parent diff layer is stale" panic occurs when multiple InsertChain calls
				// trigger concurrent StateDB.Commit → snap.Cap → flatten operations.
				// Each flatten modifies the snapshot diff layer tree, and concurrent modifications
				// cause the stale parent panic. This delay ensures operations are serialized.
				if i < len(blocksToInsert)-1 {
					time.Sleep(2000 * time.Millisecond) // Increased from 1000ms for better safety
				}
			}
			log.Info("All ancestor blocks inserted sequentially",
				"count", len(blocksToInsert))
		} else {
			return fmt.Errorf("chain is not a BlockChain instance")
		}
	}

commitComplete:
	// Record all ancestors as committed in HotStuff state and cleanup mappings
	// After blocks are persisted to db, we can safely remove them from memory mappings
	if st := h.getHsState(); st != nil {
		for _, ancestor := range uncommittedAncestors {
			ancestorHash := ancestor.Hash()
			ancestorNumber := ancestor.NumberU64()

			// Record as committed
			st.committedBlocks[ancestorHash] = ancestorNumber
			log.Debug("Marked ancestor as committed in HotStuff state",
				"number", ancestorNumber,
				"hash", ancestorHash.Hex()[:8])

			// Cleanup: remove from memory mappings since block is now persisted to db
			// 1. Remove full block from proposalsByHashBlock
			if _, exists := st.proposalsByHashBlock[ancestorHash]; exists {
				delete(st.proposalsByHashBlock, ancestorHash)
				log.Debug("Cleaned up proposalsByHashBlock for committed block",
					"number", ancestorNumber,
					"hash", ancestorHash.Hex()[:8])
			}

			// 2. Remove receipts from proposalsByHashReceipts
			if _, exists := st.proposalsByHashReceipts[ancestorHash]; exists {
				delete(st.proposalsByHashReceipts, ancestorHash)
				log.Debug("Cleaned up proposalsByHashReceipts for committed block",
					"number", ancestorNumber,
					"hash", ancestorHash.Hex()[:8])
			}

			// 3. Also clean up prewritten marker since block is now committed
			if _, exists := st.prewritten[ancestorHash]; exists {
				delete(st.prewritten, ancestorHash)
				log.Debug("Cleaned up prewritten for committed block",
					"number", ancestorNumber,
					"hash", ancestorHash.Hex()[:8])
			}
		}
	}

	// Also clean up the lock-free proposalBlocksCache
	// This cache is used by GetBlockFromState to avoid deadlocks
	for _, ancestor := range uncommittedAncestors {
		h.proposalBlocksCache.Delete(ancestor.Hash())
	}

	// Notify local sealer only for blocks we authored
	// Only send the target block (newest), not all ancestors
	targetBlock := uncommittedAncestors[len(uncommittedAncestors)-1]
	if h.IsLocalBlock(targetBlock.Header()) {
		select {
		case h.commitCh <- targetBlock:
			log.Debug("Committed target block sent to commit channel",
				"hash", targetBlock.Hash().Hex()[:8],
				"number", targetBlock.NumberU64())
		default:
			log.Warn("Commit channel is full, cannot send committed block",
				"hash", targetBlock.Hash().Hex()[:8],
				"number", targetBlock.NumberU64())
		}
	}

	log.Info("Successfully committed block with all ancestors",
		"targetBlock", block.NumberU64(),
		"committedCount", len(uncommittedAncestors))

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

// prewriteBlock writes header/body/receipts into rawdb without touching canonical head
// CRITICAL: This function acquires its own lock to protect map access
func (h *Hotstuff) prewriteBlock(hash common.Hash) {
	// Phase 1: Get block and receipts from state
	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		return
	}
	if _, ok := st.prewritten[hash]; ok {
		h.lock.RUnlock()
		return
	}
	blk := st.proposalsByHashBlock[hash]
	rcpts := st.proposalsByHashReceipts[hash]
	h.lock.RUnlock()

	if blk == nil {
		return
	}

	// Phase 2: Write to database
	rawdb.WriteBlock(h.db, blk)
	withdrawalsHashStr := "nil"
	if blk.Header().WithdrawalsHash != nil {
		withdrawalsHashStr = blk.Header().WithdrawalsHash.Hex()
	}
	log.Info("prewriteBlock", "number", blk.Number().Uint64(), "with", withdrawalsHashStr)
	if rcpts != nil {
		rawdb.WriteReceipts(h.db, blk.Hash(), blk.NumberU64(), rcpts)
	}
	rawdb.WriteHeaderNumber(h.db, blk.Hash(), blk.NumberU64())

	// Phase 3: Mark as prewritten (short lock)
	h.lock.Lock()
	st = h.getHsState()
	if st != nil {
		st.prewritten[hash] = struct{}{}
	}
	h.lock.Unlock()
	log.Trace("prewrote block to rawdb", "number", blk.NumberU64(), "hash", blk.Hash())
}

// executeBlocks executes the transactions in a block and validates their correctness
// It prevents double-spending and malicious transactions by current view's leader
// Returns only receipts; block should be constructed by caller with received header
func (h *Hotstuff) executeBlocks(header *types.Header, txs []*types.Transaction) (types.Receipts, error) {
	// Try to get parent from canonical chain first
	parent := h.chain.GetHeaderByHash(header.ParentHash)

	// If not in canonical chain, try HotStuff state (for pipelined proposals)
	if parent == nil {
		log.Debug("executeBlocks: parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
			log.Debug("executeBlocks: got parent from HotStuff state",
				"parentHash", header.ParentHash.Hex()[:10],
				"parentNumber", parent.Number.Uint64())
		}
	}

	if parent == nil {
		log.Error("executeBlocks: parent not found",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return nil, errors.New("parent not found for execution")
	}

	// 尝试通过 BlockChain.StateAt 获取状态（使用正确的 triedb 配置）
	var statedb *state.StateDB
	var err error

	if bc, ok := h.chain.(interface {
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
		// 降级方案：使用临时 triedb（可能在创世块场景下有问题）
		if h.db == nil {
			return nil, errors.New("state db not available")
		}
		tdb := state.NewDatabase(triedb.NewDatabase(h.db, triedb.HashDefaults), nil)
		statedb, err = state.New(parent.Root, tdb)
		if err != nil {
			log.Debug("Parent state not available, skip full execution", "parent", header.ParentHash, "err", err)
			return nil, h.executeBlocksLightweight(header, txs)
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

	// CRITICAL: Must persist state for Chained HotStuff!
	// In Chained HotStuff, Block N+1 needs Block N's state, but Block N is not yet
	// committed to canonical chain (waiting for 3-chain rule).
	//
	// Strategy: Commit state WITHOUT triggering snapshot operations
	// - statedb.Commit(noStorageWiping=true): Skip snapshot update/cap to avoid conflicts
	// - tdb.Commit(): Write trie nodes to database for future access
	// - InsertChain will do the full commit (with snapshot) later
	log.Info("executeBlocks: acquiring stateLock for Commit",
		"block", header.Number,
		"hash", header.Hash().Hex()[:8],
		"goroutine", getGoroutineID())
	h.stateLock.Lock()
	log.Info("executeBlocks: acquired stateLock",
		"block", header.Number,
		"hash", header.Hash().Hex()[:8],
		"goroutine", getGoroutineID())

	// Use noStorageWiping=true to skip snapshot operations and avoid "parent disk layer is stale"
	root, err := statedb.Commit(header.Number.Uint64(), h.chainConfig.IsEIP158(header.Number), true)

	h.stateLock.Unlock()
	log.Info("executeBlocks: released stateLock",
		"block", header.Number,
		"hash", header.Hash().Hex()[:8],
		"goroutine", getGoroutineID())

	if err != nil {
		return nil, fmt.Errorf("state commit failed: %w", err)
	}

	// Double check: committed root should match header.Root
	if root != header.Root {
		log.Warn("Committed state root doesn't match header root",
			"committed", root.Hex()[:8],
			"header", header.Root.Hex()[:8])
	}

	// Write trie nodes to database so they're available for future block building
	// This is the critical part - makes state accessible for next block
	if tdb := statedb.Database().TrieDB(); tdb != nil {
		if err := tdb.Commit(root, false); err != nil {
			// Non-fatal: state changes are in memory, can still be used
			// But future blocks might have issues loading this state
			log.Warn("Failed to commit state trie to db (will retry on blockchain insert)",
				"block", header.Number,
				"root", root.Hex()[:8],
				"err", err)
		} else {
			log.Debug("State trie committed to database",
				"block", header.Number,
				"root", root.Hex()[:8],
				"hash", header.Hash().Hex()[:8])
		}
	}

	// 验证通过，说明：
	// 1. 所有交易（普通+系统）执行结果正确
	// 2. 没有双花（state 变更正确）
	// 3. 系统交易合法（如奖励分配、validator 更新等）
	// 4. State已持久化到database，可供下一个block使用

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
