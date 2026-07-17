package hotstuff

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var errHotstuffProtocolInactive = errors.New("HotStuff protocol is not active")

func (h *Hotstuff) hsProtocolActive() bool {
	if h == nil || h.chain == nil || h.chainConfig == nil {
		return false
	}
	head := h.chain.CurrentHeader()
	if head == nil || head.Number == nil {
		return false
	}
	if h.chainConfig.IsHotstuff(head.Number) {
		return true
	}
	next := new(big.Int).Add(head.Number, common.Big1)
	active := h.chainConfig.IsOnHotstuff(next)
	if active {
		h.ensureBootstrapAtHead()
	}
	return active
}

// OnHsProposal handles a leader proposal: verify safety and record, then vote.
func (h *Hotstuff) OnHsProposal(peerID string, pkt interface{}) error {
	log.Info("[OnHsProposal] ENTER", "peerID", peerID)

	// ========== Phase 1: Decode and basic validation (no lock needed) ==========
	pp, ok := pkt.(*hs.ProposalPacket)
	if !ok {
		return errors.New("invalid proposal packet type")
	}

	var header types.Header
	log.Info("OnHsProposal", "header codes", fmt.Sprintf("%x", pp.HeaderRLP))
	if err := rlp.DecodeBytes(pp.HeaderRLP, &header); err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	if header.Number == nil || !h.chainConfig.IsHotstuff(header.Number) || !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	// Optionally decode body if provided for downstream use
	var body types.Body
	if err := rlp.DecodeBytes(pp.BodyRLP, &body); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	if err := h.validateProposalPacket(pp, &header, &body); err != nil {
		log.Debug("Reject proposal: invalid packet/body", "view", pp.View, "err", err)
		return nil
	}
	// Use view number carried by ProposalPacket.View
	view := pp.View
	headerView := getViewFromHeader(&header, h.chainConfig)
	if headerView != view {
		log.Debug("Reject proposal: packet view mismatch with header view",
			"packetView", view,
			"headerView", headerView,
			"block", header.Number.Uint64(),
			"hash", header.Hash().Hex()[:10])
		return nil
	}
	hasSyncInfo, headerQCView, headerQCHash, _, headerQCSigners, headerQCSig, _ := parseSyncInfoWithProof(&header, h.chainConfig)
	if !hasSyncInfo || headerQCHash != pp.HighQC.BlockHash || headerQCView != pp.HighQC.View ||
		headerQCSigners != pp.HighQC.SignersSet || !bytes.Equal(headerQCSig, pp.HighQC.Sig) {
		log.Debug("Reject proposal: packet HighQC does not match header SyncInfo", "view", view, "block", header.Hash())
		return nil
	}
	log.Debug("OnHsProposal", "peerID", peerID, "view", view, "block", header.Number.Uint64())

	// Debug: log header details to understand the structure
	log.Debug("OnHsProposal header details",
		"view", view,
		"headerParentHash", header.ParentHash.Hex()[:10],
		"headerNumber", header.Number.Uint64(),
		"headerCoinbase", header.Coinbase.Hex())

	// Expensive certificate and snapshot verification must not hold h.lock. A
	// malicious proposal can otherwise block votes, QCs and timeout processing.
	verifiedTC, err := h.parseTimeoutCert(&header)
	if err != nil {
		log.Debug("Invalid proposal: failed to parse TimeoutCert", "view", view, "err", err)
		return nil
	}
	if verifiedTC != nil && !h.verifyTimeoutCert(verifiedTC) {
		log.Debug("Invalid proposal: TimeoutCert aggregate signature invalid", "view", view)
		return nil
	}
	if pp.HighQC.BlockHash == (common.Hash{}) {
		log.Debug("Invalid proposal: missing HighQC after HotStuff activation", "view", view, "block", header.Number.Uint64())
		return nil
	}
	if pp.HighQC.View >= view {
		log.Debug("Invalid proposal: HighQC view not less than proposal view", "hqcv", pp.HighQC.View, "view", view)
		return nil
	}
	if header.ParentHash != pp.HighQC.BlockHash {
		log.Warn("Invalid proposal: HighQC does not justify parent",
			"view", view, "headerParentHash", header.ParentHash, "highQCBlockHash", pp.HighQC.BlockHash)
		return nil
	}
	if !h.verifyHighQCPayload(pp.HighQC.BlockHash, pp.HighQC.View, pp.HighQC.SignersSet, pp.HighQC.Sig) {
		log.Debug("Invalid proposal: HighQC aggregate signature invalid", "view", view)
		return nil
	}
	leader, err := h.getLeaderForViewAt(h.chain, header.ParentHash, view)
	if err != nil || leader != header.Coinbase {
		log.Debug("Ignore proposal from non-leader", "view", view, "leader", leader, "proposer", header.Coinbase, "err", err)
		return nil
	}

	// ========== Phase 2: Recheck safety state and reserve processing ==========
	log.Info("[OnHsProposal] Acquiring lock for state access", "peerID", peerID)
	h.lock.Lock()

	st := h.getHsState()
	if st == nil {
		log.Info("[OnHsProposal] State is nil", "peerID", peerID)
		h.lock.Unlock()
		return nil
	}

	if st.lockedQC != nil && (pp.HighQC.View < st.lockedQC.View ||
		(pp.HighQC.View == st.lockedQC.View && pp.HighQC.BlockHash != st.lockedQC.BlockHash)) {
		log.Warn("Reject proposal that violates persisted HotStuff lock",
			"proposalQCView", pp.HighQC.View, "proposalQCHash", pp.HighQC.BlockHash,
			"lockedView", st.lockedQC.View, "lockedHash", st.lockedQC.BlockHash)
		h.lock.Unlock()
		return nil
	}
	// Check safety condition: parent must exist or be processing
	// Distinguish between two cases:
	// 1. Parent is being processed (concurrent) → cache proposal and wait
	// 2. Parent doesn't exist (abnormal) → reject immediately
	headerHash := header.Hash() // Compute early for pending proposal indexing
	parentInChain := h.chain.GetHeaderByHash(header.ParentHash) != nil
	parentInState := h.getBlockFromStateUnsafe(header.ParentHash) != nil
	_, parentProcessing := st.processingBlocks[header.ParentHash]

	if !parentInChain && !parentInState {
		if parentProcessing {
			if _, exists := st.pendingProposalsByHash[headerHash]; exists {
				h.lock.Unlock()
				return nil
			}
			// Parent is currently being processed - cache this proposal
			log.Info("Proposal parent is being processed, caching proposal",
				"view", view,
				"proposalParent", header.ParentHash.Hex()[:10],
				"blockNumber", header.Number.Uint64(),
				"proposalHash", headerHash.Hex()[:10])

			pending := &pendingProposal{
				peerID: peerID,
				packet: pp,
				header: &header,
				body:   &body,
			}
			// Index by parent hash (for parent completion trigger)
			st.pendingProposals[header.ParentHash] = append(st.pendingProposals[header.ParentHash], pending)
			// Index by proposal hash (for QC-triggered processing)
			st.pendingProposalsByHash[headerHash] = pending
			cachedCount := len(st.pendingProposals[header.ParentHash])

			h.lock.Unlock()
			log.Debug("Proposal cached, waiting for parent to complete or QC arrival",
				"view", view,
				"parentHash", header.ParentHash.Hex()[:10],
				"proposalHash", headerHash.Hex()[:10],
				"cachedCount", cachedCount)
			return nil // Not an error, just waiting
		} else {
			// Parent is not processing and doesn't exist - reject as invalid
			log.Warn("Proposal parent not found and not processing - rejecting as invalid",
				"view", view,
				"proposalParent", header.ParentHash.Hex()[:10],
				"blockNumber", header.Number.Uint64(),
				"highQCBlock", func() string {
					if st.highQC != nil {
						return st.highQC.BlockHash.Hex()[:10]
					}
					return "none"
				}(),
				"highQCView", func() uint64 {
					if st.highQC != nil {
						return st.highQC.View
					}
					return 0
				}())
			h.lock.Unlock()
			return fmt.Errorf("parent not found and not being processed")
		}
	}

	// Parent exists - accept catch-up proposals only if they carry a verified higher HighQC.
	if st.highQC != nil && header.ParentHash != st.highQC.BlockHash {
		if pp.HighQC.View <= st.highQC.View {
			log.Warn("Reject proposal that does not extend local highQC",
				"view", view,
				"blockNumber", header.Number.Uint64(),
				"proposalParent", header.ParentHash.Hex()[:10],
				"localHighQCBlock", st.highQC.BlockHash.Hex()[:10],
				"localHighQCView", st.highQC.View,
				"proposalHighQCView", pp.HighQC.View)
			h.lock.Unlock()
			return nil
		}
		log.Info("Accepting proposal with higher verified HighQC",
			"view", view,
			"blockNumber", header.Number.Uint64(),
			"proposalParent", header.ParentHash.Hex()[:10],
			"localHighQCBlock", st.highQC.BlockHash.Hex()[:10],
			"localHighQCView", st.highQC.View,
			"proposalHighQCView", pp.HighQC.View,
			"parentInChain", parentInChain,
			"parentInState", parentInState)
	}

	parents, err := h.collectProposalParentsLocked(&header)
	if err != nil {
		log.Warn("Reject proposal: failed to collect parent headers",
			"view", view,
			"blockNumber", header.Number.Uint64(),
			"parentHash", header.ParentHash.Hex()[:10],
			"err", err)
		h.lock.Unlock()
		return nil
	}
	h.lock.Unlock()

	// Header verification performs signature recovery and snapshot traversal.
	// Recheck mutable safety state after it completes.
	if err := h.verifyHeader(h.chain, &header, parents); err != nil {
		log.Warn("Reject proposal: header verification failed",
			"view", view,
			"blockNumber", header.Number.Uint64(),
			"hash", headerHash.Hex()[:10],
			"err", err)
		return nil
	}

	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	if st.lockedQC != nil && (pp.HighQC.View < st.lockedQC.View ||
		(pp.HighQC.View == st.lockedQC.View && pp.HighQC.BlockHash != st.lockedQC.BlockHash)) {
		h.lock.Unlock()
		return nil
	}
	if st.highQC != nil && header.ParentHash != st.highQC.BlockHash && pp.HighQC.View <= st.highQC.View {
		h.lock.Unlock()
		return nil
	}
	if existing := st.proposalsByView[view]; existing != nil {
		h.lock.Unlock()
		if existing.Hash() != headerHash {
			log.Warn("Reject conflicting proposal before execution", "view", view, "existing", existing.Hash(), "proposal", headerHash)
		}
		return nil
	}
	if _, processing := st.processingBlocks[headerHash]; processing {
		h.lock.Unlock()
		return nil
	}
	parentInChain = h.chain.GetHeaderByHash(header.ParentHash) != nil
	parentInState = h.getBlockFromStateUnsafe(header.ParentHash) != nil
	if !parentInChain && !parentInState {
		h.lock.Unlock()
		return nil
	}
	if st.highQC == nil || pp.HighQC.View > st.highQC.View {
		st.highQC = &HsQC{
			BlockHash:  pp.HighQC.BlockHash,
			View:       pp.HighQC.View,
			Sig:        common.CopyBytes(pp.HighQC.Sig),
			SignersSet: pp.HighQC.SignersSet,
		}
		st.qcsByView[pp.HighQC.View] = st.highQC
		if err := h.checkpointHsStateLocked(); err != nil {
			log.Error("Failed to persist proposal HighQC", "view", pp.HighQC.View, "err", err)
		}
	}

	// Mark block as processing before releasing lock
	st.processingBlocks[headerHash] = struct{}{}
	log.Debug("[OnHsProposal] Marked block as processing", "hash", headerHash.Hex()[:10], "view", view)
	executingState := st

	// Release lock before executeBlocks (it may need to acquire RLock internally)
	log.Info("[OnHsProposal] Releasing lock before executeBlocks", "peerID", peerID)
	h.lock.Unlock()

	// ========== Phase 3: Execute block (no lock held) ==========
	log.Info("[OnHsProposal] Calling executeBlocks", "peerID", peerID)
	executeStart := time.Now()
	receipts, err := h.executeBlocks(&header, body.Transactions)
	hotstuffProposalExecuteTimer.Update(time.Since(executeStart))
	if err != nil {
		hotstuffProposalExecuteErrors.Inc(1)
	}
	log.Info("[OnHsProposal] executeBlocks returned", "peerID", peerID, "err", err)

	// ========== Phase 4: Update state (needs lock for writing st) ==========
	log.Info("[OnHsProposal] Re-acquiring lock for state update", "peerID", peerID)
	h.lock.Lock()
	log.Info("[OnHsProposal] Lock re-acquired", "peerID", peerID)

	if err != nil {
		if st := h.getHsState(); st != nil {
			delete(st.processingBlocks, headerHash)
		}
		h.lock.Unlock()
		log.Warn("Reject proposal: block execution failed", "view", view, "hash", headerHash.Hex()[:10], "err", err)
		return nil
	}

	// Re-get state after re-acquiring lock (state might have changed)
	st = h.getHsState()
	if st == nil {
		delete(executingState.proposalStates, headerHash)
		h.lock.Unlock()
		return nil
	}

	// Construct block using received header (already has correct hashes and signature)
	// CRITICAL: Use NewBlockWithHeader + WithBody to preserve original header
	// NewBlock would recalculate TxHash/ReceiptHash and modify the header copy,
	// causing signature verification to fail later
	if header.EmptyWithdrawalsHash() {
		body.Withdrawals = make([]*types.Withdrawal, 0)
	}
	block := types.NewBlockWithHeader(&header).WithBody(body)

	// Header SyncInfo is verified during block validation. Runtime highQC is
	// advanced only from proposal/QC messages carrying aggregate proof.

	// CRITICAL FIX: Check if this view already has a proposal
	// In HotStuff, each view should have only ONE valid proposal
	// If we receive a different proposal for the same view, reject it
	if existingProposal, exists := st.proposalsByView[view]; exists {
		if existingProposal.Hash() != header.Hash() {
			log.Warn("OnHsProposal: rejecting duplicate proposal for same view (different block)",
				"view", view,
				"existingBlock", existingProposal.Hash().Hex()[:10],
				"existingNumber", existingProposal.Number.Uint64(),
				"newBlock", header.Hash().Hex()[:10],
				"newNumber", header.Number.Uint64(),
				"leader", header.Coinbase.Hex())
			delete(st.proposalStates, headerHash)
			h.lock.Unlock()
			return nil // Reject the duplicate proposal
		} else {
			log.Debug("OnHsProposal: received duplicate proposal for same view (same block), ignoring",
				"view", view,
				"block", header.Hash().Hex()[:10])
			// Same block, just return (already processed)
			h.lock.Unlock()
			return nil
		}
	}

	// Record proposal (including receipts for prewrite)
	// The block may have different hash due to NewBlock modifications,
	// but we must use header.Hash() for vote matching and signature verification
	// Note: headerHash was already computed before Phase 3
	st.proposalsByView[view] = &header
	st.proposalsByHash[headerHash] = &header
	st.proposalsByHashBlock[headerHash] = block
	st.proposalsByHashReceipts[headerHash] = receipts
	if view > st.currentView {
		st.currentView = view
	}

	// CRITICAL: Also update lock-free cache to prevent RWMutex priority deadlock
	// This allows GetBlockFromState to access blocks without acquiring h.lock,
	// which is essential when executeBlocks (Phase 3, no lock) calls GetBlockFromState
	// while another goroutine is waiting for h.lock.Lock().
	h.proposalBlocksCache.Store(headerHash, block)

	// A QC may arrive before its proposal body has finished execution. Retry the
	// 3-chain commit only after the certified block is present in state.
	if err := h.tryCommitBlocks(st, headerHash, view); err != nil {
		log.Debug("Try commit blocks failed", "view", view, "hash", headerHash, "err", err)
	}

	// Remove from processing set and trigger pending proposals
	delete(st.processingBlocks, headerHash)
	// Clean up pendingProposalsByHash entry for this block (if it was cached)
	delete(st.pendingProposalsByHash, headerHash)
	log.Debug("[OnHsProposal] Removed block from processing set", "hash", headerHash.Hex()[:10], "view", view)

	// Collect pending proposals before releasing lock
	var pendingToProcess []*pendingProposal
	if pending := st.pendingProposals[headerHash]; len(pending) > 0 {
		log.Info("[OnHsProposal] Found pending proposals waiting for this block",
			"parentHash", headerHash.Hex()[:10],
			"pendingCount", len(pending))
		pendingToProcess = pending
		delete(st.pendingProposals, headerHash)
		// Also clean up pendingProposalsByHash for each pending proposal
		for _, p := range pendingToProcess {
			delete(st.pendingProposalsByHash, p.header.Hash())
		}
	}

	// Check if voting is still needed
	// If currentView has advanced beyond this proposal's view, it means a QC was already
	// received for a newer view, so voting for this old view is unnecessary.
	// The proposal is already recorded in state for potential commit via 3-chain rule.
	shouldVote := (view >= st.currentView)
	if !shouldVote {
		log.Info("[OnHsProposal] Skipping vote - view already superseded by QC",
			"proposalView", view,
			"currentView", st.currentView,
			"blockHash", headerHash.Hex()[:10])
	}

	// Persist the anti-double-vote decision under lock. The potentially slow
	// wallet/BLS operation is performed after releasing the state lock.
	shouldSignVote := false
	if shouldVote && h.blsVoteSigner() != nil {
		if err := h.prepareVoteLocked(view, headerHash); err != nil {
			log.Error("Refusing to sign HotStuff vote without durable safety state", "view", view, "hash", headerHash, "err", err)
			h.lock.Unlock()
			return nil
		}
		shouldSignVote = true
	} else if shouldVote {
		// No signer configured
		log.Warn("No BLS vote signer configured; skip voting")
	}

	// Release lock before calling functions that acquire their own locks
	log.Info("[OnHsProposal] Releasing lock before post-processing", "peerID", peerID)
	h.lock.Unlock()

	// ========== Phase 5: Post-processing (no lock held) ==========
	var vote *hs.VotePacket
	if shouldSignVote {
		digest := h.hsVoteDigest(headerHash, view)
		pub, sig, signErr := h.signHsVote(digest)
		if signErr != nil {
			log.Warn("Failed to sign hs vote", "view", view, "err", signErr)
		} else {
			candidate := &hs.VotePacket{BlockHash: headerHash, ViewNumber: view, VotePubKey: pub, Signature: sig}
			h.lock.Lock()
			st = h.getHsState()
			if st != nil && st.hasLastVote && st.lastVotedView == view && st.lastVotedHash == headerHash && view >= st.currentView {
				addr := h.ConsensusAddress()
				if st.votes[view] == nil {
					st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
				}
				if st.votes[view][headerHash] == nil {
					st.votes[view][headerHash] = make(map[common.Address]*hs.VotePacket)
				}
				st.votes[view][headerHash][addr] = candidate
				vote = candidate
			}
			h.lock.Unlock()
		}
	}

	// Process pending proposals asynchronously
	for _, p := range pendingToProcess {
		go func(pending *pendingProposal) {
			log.Debug("[OnHsProposal] Processing previously cached proposal",
				"view", pending.packet.View,
				"parentHash", pending.header.ParentHash.Hex()[:10])
			if err := h.OnHsProposal(pending.peerID, pending.packet); err != nil {
				log.Debug("Failed to process cached proposal", "view", pending.packet.View, "err", err)
			}
		}(p)
	}

	// Restart view timeout (this function acquires its own lock)
	h.restartViewTimeout()

	// Send vote to leader only if voting is still relevant
	if shouldVote && vote != nil {
		log.Info("[OnHsProposal] Sending vote to leader", "view", view, "leader", leader.Hex()[:10])
		h.sendHsVote(leader, vote)
	}

	// CRITICAL: Check if we need to notify miner after processing this proposal
	// This handles the case where:
	// 1. QC arrived before proposal was processed
	// 2. View advanced and this node became leader
	// 3. But miner notification was skipped because highQC block wasn't ready
	// 4. Now that proposal is processed, we should notify miner
	h.lock.RLock()
	st = h.getHsState()
	var shouldNotifyMiner bool
	var currentViewForMiner uint64
	var highQCHashForMiner common.Hash
	if st != nil && st.highQC != nil && st.highQC.BlockHash == headerHash {
		// This proposal is now the highQC block
		currentViewForMiner = st.currentView
		highQCHashForMiner = st.highQC.BlockHash
		h.lock.RUnlock()

		// Check if we are the leader for current view
		leaderForCurrent, _ := h.getLeaderForViewAt(h.chain, highQCHashForMiner, currentViewForMiner)
		if leaderForCurrent == h.ConsensusAddress() {
			shouldNotifyMiner = true
		}
	} else {
		h.lock.RUnlock()
	}

	if shouldNotifyMiner {
		log.Info("[OnHsProposal] Proposal processed is highQC block, notifying miner",
			"proposalView", view,
			"currentView", currentViewForMiner,
			"highQCHash", highQCHashForMiner.Hex()[:10])
		h.proposeFromHighQC(currentViewForMiner, highQCHashForMiner)
	}

	log.Info("[OnHsProposal] EXIT", "peerID", peerID, "view", view)
	return nil
}

func (h *Hotstuff) validateProposalPacket(pp *hs.ProposalPacket, header *types.Header, body *types.Body) error {
	if header.Number == nil {
		return errUnknownBlock
	}
	headerHash := header.Hash()
	if pp.BlockHash != headerHash {
		return fmt.Errorf("packet block hash mismatch: packet %s header %s", pp.BlockHash, headerHash)
	}
	if pp.ParentHash != header.ParentHash {
		return fmt.Errorf("packet parent hash mismatch: packet %s header %s", pp.ParentHash, header.ParentHash)
	}
	if header.UncleHash != types.EmptyUncleHash {
		return errInvalidUncleHash
	}
	if len(body.Uncles) != 0 {
		return fmt.Errorf("proposal body contains %d uncles", len(body.Uncles))
	}

	txRoot := types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil))
	if txRoot != header.TxHash {
		return fmt.Errorf("tx root mismatch: got %s want %s", txRoot, header.TxHash)
	}

	if header.WithdrawalsHash == nil {
		if len(body.Withdrawals) != 0 {
			return errors.New("proposal body has withdrawals but header has nil withdrawals hash")
		}
	} else {
		withdrawalsRoot := types.DeriveSha(types.Withdrawals(body.Withdrawals), trie.NewStackTrie(nil))
		if withdrawalsRoot != *header.WithdrawalsHash {
			return fmt.Errorf("withdrawals root mismatch: got %s want %s", withdrawalsRoot, *header.WithdrawalsHash)
		}
	}

	if header.RequestsHash != nil && *header.RequestsHash != types.EmptyRequestsHash {
		return fmt.Errorf("proposal protocol does not carry requests: header requests hash %s", *header.RequestsHash)
	}
	if header.BlobGasUsed != nil && *header.BlobGasUsed != 0 {
		return fmt.Errorf("proposal protocol does not carry blob sidecars: blobGasUsed %d", *header.BlobGasUsed)
	}

	signer := types.MakeSigner(h.chainConfig, header.Number, header.Time)
	for i, tx := range body.Transactions {
		if _, err := types.Sender(signer, tx); err != nil {
			return fmt.Errorf("invalid tx signature idx=%d hash=%s: %w", i, tx.Hash(), err)
		}
		if tx.BlobGas() != 0 || tx.BlobTxSidecar() != nil {
			return fmt.Errorf("proposal protocol does not carry blob sidecars for tx idx=%d hash=%s", i, tx.Hash())
		}
	}
	return nil
}

// collectProposalParentsLocked returns a contiguous ancestor chain ending at
// header.ParentHash. Callers must hold h.lock so uncommitted HotStuff state can
// be read through getBlockFromStateUnsafe.
func (h *Hotstuff) collectProposalParentsLocked(header *types.Header) ([]*types.Header, error) {
	if header.Number == nil {
		return nil, errUnknownBlock
	}
	if header.Number.Sign() == 0 {
		return nil, nil
	}

	hash := header.ParentHash
	number := header.Number.Uint64() - 1
	parents := make([]*types.Header, 0, 4)

	for {
		parent := h.chain.GetHeader(hash, number)
		if parent != nil {
			parents = append(parents, parent)
			break
		}

		block := h.getBlockFromStateUnsafe(hash)
		if block == nil || block.NumberU64() != number {
			return nil, consensus.ErrUnknownAncestor
		}
		parent = block.Header()
		parents = append(parents, parent)

		if number == 0 || len(parents) >= int(checkpointInterval) {
			break
		}
		hash = parent.ParentHash
		number--
	}

	for i, j := 0, len(parents)-1; i < j; i, j = i+1, j-1 {
		parents[i], parents[j] = parents[j], parents[i]
	}
	return parents, nil
}

func newViewTargetView(nv *hs.NewViewPacket) uint64 {
	if nv == nil {
		return 0
	}
	if nv.HighQCView == math.MaxUint64 || nv.HighTCView == math.MaxUint64 {
		return 0
	}
	v := nv.HighQCView + 1
	if nv.HighTCView > 0 && nv.HighTCView+1 > v {
		v = nv.HighTCView + 1
	}
	return v
}

func viewTooOld(view, current, tolerance uint64) bool {
	return current > view && current-view > tolerance
}

func (h *Hotstuff) verifyNewViewTimeoutCert(nv *hs.NewViewPacket, targetView uint64) bool {
	if nv == nil || nv.HighQCHash == (common.Hash{}) || targetView == 0 || nv.HighQCView >= targetView {
		return false
	}
	if !h.verifyHighQCPayload(nv.HighQCHash, nv.HighQCView, nv.HighQCSignersSet, nv.HighQCAggSig) {
		log.Debug("Invalid NewView: HighQC proof verification failed", "targetView", targetView, "highQCView", nv.HighQCView)
		return false
	}
	if nv.HighTCView == 0 {
		return nv.TimeoutSignersSet == 0 && nv.TimeoutAggSig == (types.BLSSignature{})
	}
	if nv.HighTCView >= targetView {
		log.Debug("Invalid NewView: target view does not follow HighTC",
			"targetView", targetView,
			"highTCView", nv.HighTCView,
			"highQCView", nv.HighQCView)
		return false
	}
	tc := h.timeoutCertFromNewView(nv)
	if tc == nil {
		log.Debug("Invalid NewView: missing timeout certificate",
			"targetView", targetView,
			"highTCView", nv.HighTCView)
		return false
	}
	return h.verifyTimeoutCert(tc)
}

func (h *Hotstuff) resolveNewViewValidator(peerID string, nv *hs.NewViewPacket) (common.Address, error) {
	network := h.hsNetwork()
	if network == nil {
		return common.Address{}, errors.New("HotStuff network is not set")
	}
	addr, ok := network.ResolveAddress(peerID)
	if !ok || addr == (common.Address{}) {
		return common.Address{}, errors.New("NewView peer does not resolve to a consensus address")
	}
	snap, err := h.getSnapshotAtHashOrView(h.chain, nv.HighQCHash, nv.HighQCView)
	if err != nil || snap == nil {
		return common.Address{}, fmt.Errorf("cannot resolve NewView validator set: %w", err)
	}
	if _, ok := snap.Validators[addr]; !ok {
		return common.Address{}, errUnauthorizedValidator(addr.String())
	}
	return addr, nil
}

// OnHsVote tallies votes; when quorum reached, form QC and try to commit.
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations (snapshot, network IO).
func (h *Hotstuff) OnHsVote(peerID string, pkt interface{}) error {
	if !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	// Phase 1: Decode and basic validation (no lock)
	vp, ok := pkt.(*hs.VotePacket)
	if !ok {
		return errors.New("invalid vote packet type")
	}
	log.Debug("OnHsVote", "view", vp.ViewNumber, "peer", peerID, "blockHash", vp.BlockHash, "voter", fmt.Sprintf("%x", vp.VotePubKey.Bytes()))

	view := vp.ViewNumber

	// Phase 2: Quick state checks and get targetHeader from state (short lock)
	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		log.Error("Hotstuff state not initialized")
		return errors.New("Hotstuff state not initialized")
	}

	// Reject votes for past views (with tolerance for pipelining)
	// In Chained HotStuff, votes may arrive slightly late due to processing delays
	// Allow votes from currentView-2 to handle pipelining scenarios
	const voteViewTolerance = 2
	if viewTooOld(vp.ViewNumber, st.currentView, voteViewTolerance) {
		log.Warn("Rejecting stale vote from past view",
			"voteView", vp.ViewNumber,
			"currentView", st.currentView,
			"peer", peerID)
		h.lock.Unlock()
		return nil
	}

	if st.lockedQC != nil && vp.ViewNumber < st.lockedQC.View {
		log.Debug("Vote view is less than locked view, skip vote", "view", vp.ViewNumber, "lockedView", st.lockedQC.View)
		h.lock.Unlock()
		return nil
	}

	// Try to get header from HotStuff state (fast path)
	var targetHeader *types.Header
	if st.proposalsByHash[vp.BlockHash] != nil {
		targetHeader = st.proposalsByHash[vp.BlockHash]
		log.Debug("Found header in proposalsByHash", "blockHash", vp.BlockHash.Hex()[:8], "view", view)
	}
	h.lock.Unlock()

	// Phase 3: Get header from chain/rawdb if not in state (no lock - may block)
	if targetHeader == nil {
		targetHeader = h.chain.GetHeaderByHash(vp.BlockHash)
		if targetHeader == nil && h.db != nil {
			if number := rawdb.ReadHeaderNumber(h.db, vp.BlockHash); number != nil {
				if header := rawdb.ReadHeader(h.db, vp.BlockHash, *number); header != nil {
					targetHeader = header
					log.Debug("Found header in rawdb", "blockHash", vp.BlockHash.Hex()[:8], "view", view)
				}
			}
		}
	}
	if targetHeader == nil {
		log.Debug("Target header not found in OnHsVote", "blockHash", vp.BlockHash.Hex()[:8], "view", view)
		return nil
	}

	// Phase 4: Verify vote and resolve address (no lock - may call snapshot)
	addr, blsAvailable, err := h.verifyHsBlsVoteAndResolveAddr(vp, targetHeader)
	if err != nil {
		log.Debug("Invalid hs vote", "view", view, "err", err)
		return nil
	}
	if !blsAvailable {
		log.Debug("Rejecting vote without a validator BLS mapping", "view", view, "peer", peerID)
		return nil
	}

	// Phase 5: Update votes in state (short lock)
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	// Double-check view hasn't advanced too far
	// Use same tolerance as initial check
	if viewTooOld(vp.ViewNumber, st.currentView, voteViewTolerance) {
		h.lock.Unlock()
		return nil
	}

	if st.votes[view] == nil {
		st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
	}
	if st.votes[view][vp.BlockHash] == nil {
		st.votes[view][vp.BlockHash] = make(map[common.Address]*hs.VotePacket)
	}
	st.votes[view][vp.BlockHash][addr] = vp
	voteCount := len(st.votes[view][vp.BlockHash])

	// Copy voters for later use (deep copy to avoid holding lock)
	votersCopy := make(map[common.Address]*hs.VotePacket, len(st.votes[view][vp.BlockHash]))
	for k, v := range st.votes[view][vp.BlockHash] {
		votersCopy[k] = v
	}
	h.lock.Unlock()

	// Phase 6: Get snapshot for quorum calculation (no lock - may block)
	var snap *Snapshot
	if targetHeader.Number.Uint64() > 0 {
		snap, _ = h.snapshot(h.chain, targetHeader.Number.Uint64()-1, targetHeader.ParentHash, nil)
	} else {
		genesisHeader := h.chain.GetHeaderByNumber(0)
		if genesisHeader != nil {
			snap, _ = h.snapshot(h.chain, 0, genesisHeader.Hash(), nil)
		}
	}
	if snap == nil {
		log.Debug("Failed to get snapshot for vote quorum", "view", view)
		return nil
	}

	qsize := QuorumSize(len(snap.validators()))
	log.Debug("Vote count check", "view", view, "block", targetHeader.Number.Uint64(), "cnt", voteCount, "qsize", qsize)

	if voteCount < qsize {
		return nil // Not enough votes yet
	}

	// Phase 7: Quorum reached - check leader and prepare QC (no lock for snapshot/leader)
	log.Info("Quorum vote collected", "view", view, "block", targetHeader.Number.Uint64(), "cnt", voteCount, "qsize", qsize)

	leader, err := h.getLeaderForViewAt(h.chain, targetHeader.ParentHash, view)
	if err != nil {
		log.Warn("Failed to get leader for view in OnHsVote", "view", view, "err", err)
		return nil
	}
	isLeader := (leader == h.ConsensusAddress())

	// Aggregate signatures (no lock needed)
	agg := h.aggregateHsVoteSignatures(votersCopy)

	// Get validators for SignersSet (snapshot already obtained)
	validators := snap.validators()

	// Build QC with SignersSet
	qc := &HsQC{BlockHash: vp.BlockHash, View: view}
	matchedCount := 0
	for idx, valAddr := range validators {
		if _, ok := votersCopy[valAddr]; ok {
			qc.SignersSet |= types.ValidatorsBitSet(1) << uint(idx)
			matchedCount++
		}
	}
	qc.Sig = agg
	if matchedCount < qsize || !qc.hasAggregateProof() || !h.verifyAggregateQC(&hs.QuorumCertPacket{
		TargetHash: qc.BlockHash, ViewNumber: qc.View, SignersSet: qc.SignersSet, AggregateSig: qc.Sig,
	}) {
		log.Warn("Formed QC without aggregate proof, ignoring",
			"view", view,
			"blockHash", vp.BlockHash.Hex()[:8],
			"signersSet", qc.SignersSet,
			"aggSigLen", len(qc.Sig))
		return nil
	}

	log.Info("Formed QC with aggregate signature",
		"view", view,
		"blockHash", vp.BlockHash.Hex()[:8],
		"signersSet", qc.SignersSet,
		"aggSigLen", len(agg),
		"voterCount", len(votersCopy),
		"matchedCount", matchedCount)

	// Phase 8: Update state with QC (short lock)
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	if existing := st.qcsByView[view]; existing != nil && existing.BlockHash != qc.BlockHash {
		h.lock.Unlock()
		log.Warn("Rejecting conflicting QC for an already certified view", "view", view, "existing", existing.BlockHash, "received", qc.BlockHash)
		return nil
	}
	if st.highQC != nil && view == st.highQC.View && st.highQC.BlockHash != qc.BlockHash {
		h.lock.Unlock()
		log.Warn("Rejecting QC conflicting with local highQC", "view", view, "local", st.highQC.BlockHash, "received", qc.BlockHash)
		return nil
	}
	st.qcsByView[view] = qc
	if st.highQC == nil || view > st.highQC.View {
		st.highQC = qc
	}

	// Try 3-chain commit
	if err := h.tryCommitBlocks(st, vp.BlockHash, view); err != nil {
		log.Error("Failed to commit blocks after QC formation", "view", view, "err", err)
	}

	// Advance view
	if view == math.MaxUint64 {
		h.lock.Unlock()
		return nil
	}
	newView := view + 1
	shouldAdvanceView := newView > st.currentView
	if shouldAdvanceView {
		st.currentView = newView
		log.Info("View advanced after QC", "oldView", view, "newView", newView)
	}
	if err := h.checkpointHsStateLocked(); err != nil {
		log.Error("Failed to persist formed QC", "view", view, "err", err)
	}
	h.lock.Unlock()

	// Phase 9: Post-QC operations (no lock - may block)
	// Prewrite the QC'd block
	h.prewriteBlock(vp.BlockHash)

	// Only leader broadcasts QC
	if isLeader {
		// Use the unlocked version for broadcast
		if err := h.broadcastHsQCWithAgg(vp.BlockHash, view, agg, qc.SignersSet); err != nil {
			log.Warn("OnHsVote broadcastHsQCWithAgg failed", "err", err)
		} else {
			log.Info("Leader broadcasted QC", "view", view, "block", vp.BlockHash.Hex()[:8])
		}
	}

	// Check if we are the leader for the new view
	newLeader, _ := h.getLeaderForViewAt(h.chain, vp.BlockHash, newView)
	if shouldAdvanceView && newLeader == h.ConsensusAddress() {
		log.Info("We are the leader for new view after forming QC, triggering proposal", "view", newView)
		// CRITICAL FIX: Notify miner to start block production immediately!
		h.proposeFromHighQC(newView, vp.BlockHash)
	}

	// Restart view timeout
	h.restartViewTimeout()

	return nil
}

// OnHsNewView collects NewView; leader proposes when quorum of NewView received.
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations.
func (h *Hotstuff) OnHsNewView(peerID string, pkt interface{}) error {
	if !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	// Phase 1: Decode (no lock)
	nv, ok := pkt.(*hs.NewViewPacket)
	if !ok {
		return errors.New("invalid newview packet type")
	}
	v := newViewTargetView(nv)
	log.Debug("OnHsNewView", "highQCView", nv.HighQCView, "highTCView", nv.HighTCView, "new view", v, "peer", peerID)
	if !h.verifyNewViewTimeoutCert(nv, v) {
		log.Debug("Invalid NewView: timeout certificate verification failed", "view", v, "peer", peerID)
		return nil
	}

	addr, err := h.resolveNewViewValidator(peerID, nv)
	if err != nil {
		log.Debug("Invalid NewView sender", "peer", peerID, "err", err)
		return nil
	}

	// Phase 2: Update state (short lock)
	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	if st.newViews[v] == nil {
		st.newViews[v] = make(map[common.Address]*hs.NewViewPacket)
	}
	st.newViews[v][addr] = nv

	// Update highTCView if NewView carries higher TC
	if nv.HighTCView > st.highTCView {
		log.Debug("OnHsNewView updating highTCView", "oldView", st.highTCView, "newView", nv.HighTCView, "peer", peerID)
		st.highTCView = nv.HighTCView
	}

	// Copy newViews for processing outside lock
	newViewCount := len(st.newViews[v])
	var newViewsCopy map[common.Address]*hs.NewViewPacket
	if newViewCount > 0 {
		newViewsCopy = make(map[common.Address]*hs.NewViewPacket, newViewCount)
		for k, val := range st.newViews[v] {
			newViewsCopy[k] = val
		}
	}
	h.lock.Unlock()

	// Phase 4: Select the highest QC first, then determine its contextual leader.
	{
		// Pick highest QC and highest TC among new-views
		var maxQCView uint64
		var base common.Hash
		var maxTCView uint64
		var selected *hs.NewViewPacket
		for _, m := range newViewsCopy {
			if m.HighQCView > maxQCView || (m.HighQCView == maxQCView && bytes.Compare(m.HighQCHash[:], base[:]) > 0) {
				maxQCView = m.HighQCView
				base = m.HighQCHash
				selected = m
			}
			if m.HighTCView > maxTCView {
				maxTCView = m.HighTCView
			}
		}
		if base == (common.Hash{}) || selected == nil {
			log.Debug("OnHsNewView no HighQC base selected", "view", v)
			return nil
		}
		snap, err := h.getSnapshotAtHashOrView(h.chain, base, maxQCView)
		if err != nil || snap == nil {
			log.Debug("OnHsNewView failed to get quorum snapshot",
				"view", v,
				"highQCView", maxQCView,
				"highQCHash", base,
				"err", err)
			return nil
		}
		validNewViews := 0
		maxTCView = 0
		for addr, message := range newViewsCopy {
			if _, ok := snap.Validators[addr]; ok {
				validNewViews++
				if message.HighTCView > maxTCView {
					maxTCView = message.HighTCView
				}
			}
		}
		if validNewViews < QuorumSize(len(snap.validators())) {
			return nil
		}
		if selectedLeader, err := h.getLeaderForViewAt(h.chain, base, v); err != nil || selectedLeader != h.ConsensusAddress() {
			return nil
		}

		// Install the quorum-selected sync state before waking the miner. This is
		// safety-critical because GetProposalParent and Seal read these fields.
		h.lock.Lock()
		st = h.getHsState()
		if st == nil || v < st.currentView {
			h.lock.Unlock()
			return nil
		}
		if existing := st.qcsByView[maxQCView]; existing != nil && existing.BlockHash != base {
			h.lock.Unlock()
			return nil
		}
		if st.highQC == nil || maxQCView > st.highQC.View {
			st.highQC = &HsQC{
				BlockHash: base, View: maxQCView, SignersSet: selected.HighQCSignersSet,
				Sig: common.CopyBytes(selected.HighQCAggSig),
			}
			st.qcsByView[maxQCView] = st.highQC
		} else if maxQCView == st.highQC.View && st.highQC.BlockHash != base {
			h.lock.Unlock()
			return nil
		}
		if maxTCView > st.highTCView {
			st.highTCView = maxTCView
		}
		viewAdvanced := v > st.currentView
		if viewAdvanced {
			st.currentView = v
		}
		if err := h.checkpointHsStateLocked(); err != nil {
			log.Error("Failed to persist NewView quorum state", "view", v, "err", err)
		}
		h.lock.Unlock()
		if viewAdvanced {
			h.restartViewTimeout()
		}

		// Propose (no lock - proposeFromHighQC just notifies miner)
		h.proposeFromHighQC(v, base)
	}
	return nil
}

// OnHsTimeout collects timeouts and advances view on TC (simplified).
func (h *Hotstuff) OnHsTimeout(peerID string, pkt interface{}) error {
	if !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	// Aggregate timeouts; if reach quorum, advance to next view and trigger NewView/proposal
	// CRITICAL FIX: Do NOT call moveToView while holding h.lock, as moveToView needs to acquire it.
	// Optimized lock usage: minimize lock acquisition and state retrieval.
	// Phase 1: Decode and verify timeout (no lock)
	// Phase 2: Acquire lock once, check stale, update state, release lock
	// Phase 3: Calculate quorum (no lock)
	// Phase 4: Acquire lock once, check quorum, update highTCView, release lock
	// Phase 5: Call moveToView if needed (no lock)

	// Phase 1: Decode and basic validation (no lock)
	to, ok := pkt.(*hs.TimeoutPacket)
	if !ok {
		return errors.New("invalid timeout packet type")
	}
	log.Debug("OnHsTimeout", "view", to.ViewNumber, "peer", peerID)

	// Phase 2: Acquire lock once, check stale timeout, then release for verification
	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}

	// CRITICAL FIX: Reject timeouts for past views (with tolerance for pipelining)
	// Allow timeouts from currentView-2 to handle network delays and pipelining
	const timeoutViewTolerance = 2
	if viewTooOld(to.ViewNumber, st.currentView, timeoutViewTolerance) {
		log.Debug("Rejecting stale timeout from past view",
			"timeoutView", to.ViewNumber,
			"currentView", st.currentView,
			"peer", peerID)
		h.lock.Unlock()
		return nil
	}

	v := to.ViewNumber
	h.lock.Unlock()

	// verify timeout and resolve address (no lock held - may call snapshot/chain operations)
	addr, blsAvailable, err := h.verifyHsTimeoutAndResolveAddr(to)
	if err != nil {
		log.Debug("Invalid hs timeout", "view", to.ViewNumber, "err", err)
		return nil
	}
	if !blsAvailable {
		log.Debug("Rejecting timeout without a validator BLS mapping", "view", to.ViewNumber, "peer", peerID)
		return nil
	}

	// Phase 3: Re-acquire lock once, update state, then release for snapshot calculation
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}

	// Double-check view hasn't advanced too far (race condition protection)
	// Use same tolerance as initial check
	if viewTooOld(v, st.currentView, timeoutViewTolerance) {
		log.Debug("Rejecting stale timeout (view advanced during processing)",
			"timeoutView", v,
			"currentView", st.currentView,
			"peer", peerID)
		h.lock.Unlock()
		return nil
	}

	// Update timeouts map
	if st.timeouts[v] == nil {
		st.timeouts[v] = make(map[common.Address]*hs.TimeoutPacket)
	}
	if _, exists := st.timeouts[v][addr]; exists {
		h.lock.Unlock()
		return nil
	}
	st.timeouts[v][addr] = to
	timeoutMapCopy := make(map[common.Address]*hs.TimeoutPacket, len(st.timeouts[v]))
	for validator, timeout := range st.timeouts[v] {
		timeoutMapCopy[validator] = timeout
	}
	h.lock.Unlock()

	// Phase 4: Count only validators in the deterministic highest-HighQC set.
	timeoutCount, qsize, err := h.timeoutQuorum(timeoutMapCopy)
	if err != nil {
		log.Debug("OnHsTimeout failed to get timeout snapshot", "view", v, "highQCView", to.HighQCView, "highQCHash", to.HighQCHash, "err", err)
		return nil
	}
	log.Debug("OnHsTimeout view timeout collected", "view", v, "cnt", timeoutCount, "qsize", qsize)

	// Phase 5: Re-acquire lock once, check quorum and update highTCView
	var nextView uint64
	shouldMoveToView := false
	if timeoutCount >= qsize {
		h.lock.Lock()
		st = h.getHsState()
		if st != nil {
			// Re-check timeout count (may have changed)
			if st.timeouts[v] != nil {
				log.Debug("OnHsTimeout quorum view timeout collected", "view", v, "cnt", timeoutCount, "qsize", qsize)
				// form TC implicitly; advance to v+1
				if v == math.MaxUint64 {
					h.lock.Unlock()
					return nil
				}
				if v > st.highTCView {
					st.highTCView = v
				}
				nextView = v + 1
				shouldMoveToView = true
			}
		}
		h.lock.Unlock()
	}

	// Phase 6: Call moveToView WITHOUT holding lock (moveToView will acquire its own lock)
	if shouldMoveToView {
		log.Info("OnHsTimeout: calling moveToView after releasing lock", "nextView", nextView)
		h.moveToView(nextView)
	}
	return nil
}

// OnHsQuorumCert updates HighQC from remote
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations.
func (h *Hotstuff) OnHsQuorumCert(peerID string, pkt interface{}) error {
	if !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	// Phase 1: Decode (no lock)
	qc, ok := pkt.(*hs.QuorumCertPacket)
	if !ok {
		return errors.New("invalid qc packet type")
	}

	// Phase 2: Quick state check (short lock)
	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	currentView := st.currentView
	var highQCView uint64
	if st.highQC != nil {
		highQCView = st.highQC.View
	}
	h.lock.Unlock()

	log.Debug("OnHsQuorumCert", "view", qc.ViewNumber, "currentView", currentView, "highQC", highQCView, "peer", peerID)

	// Reject stale QCs
	if viewTooOld(qc.ViewNumber, currentView, 1) {
		log.Debug("Rejecting stale QC from old view",
			"qcView", qc.ViewNumber,
			"currentView", currentView,
			"peer", peerID)
		return nil
	}

	// Phase 3: Verify QC (no lock - may call snapshot)
	if !h.verifyAggregateQC(qc) {
		log.Debug("Invalid aggregated QC", "view", qc.ViewNumber)
		return nil
	}

	// Phase 4: Update state (short lock)
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}

	// Re-check if QC is still relevant
	if viewTooOld(qc.ViewNumber, st.currentView, 1) {
		h.lock.Unlock()
		return nil
	}

	shouldUpdate := false
	shouldAdvanceView := false
	shouldPrewriteQCBlock := false
	var newView uint64
	var pendingToProcess *pendingProposal // Pending proposal to process if QC block not found

	// A valid QC means this block has enough votes. If the block was already
	// executed locally, write it to rawdb now so final commit can use the
	// known-block fast path more often.
	shouldPrewriteQCBlock = st.proposalsByHashBlock[qc.TargetHash] != nil
	if existing := st.qcsByView[qc.ViewNumber]; existing != nil && existing.BlockHash != qc.TargetHash {
		h.lock.Unlock()
		log.Warn("Rejecting conflicting QC for an already certified view", "view", qc.ViewNumber, "existing", existing.BlockHash, "received", qc.TargetHash)
		return nil
	}
	if st.highQC != nil && qc.ViewNumber == st.highQC.View && qc.TargetHash != st.highQC.BlockHash {
		h.lock.Unlock()
		log.Warn("Rejecting QC conflicting with local highQC", "view", qc.ViewNumber, "local", st.highQC.BlockHash, "received", qc.TargetHash)
		return nil
	}

	if st.highQC == nil || qc.ViewNumber >= st.highQC.View {
		shouldUpdate = (st.highQC == nil ||
			qc.ViewNumber > st.highQC.View)

		if shouldUpdate {
			st.highQC = &HsQC{
				BlockHash:  qc.TargetHash,
				View:       qc.ViewNumber,
				Sig:        common.CopyBytes(qc.AggregateSig),
				SignersSet: qc.SignersSet,
			}
			st.qcsByView[qc.ViewNumber] = st.highQC
			log.Info("OnHsQuorumCert updated highQC",
				"view", qc.ViewNumber,
				"blockHash", qc.TargetHash.Hex()[:10],
				"signersSet", qc.SignersSet)
		}

		// OPTIMIZATION: Check if QC block exists in state
		// If not, check if there's a pending proposal we can process immediately
		// BUT: Only process if parent is ready (not still in processingBlocks)
		qcBlockExists := st.proposalsByHashBlock[qc.TargetHash] != nil
		if !qcBlockExists {
			if pending, ok := st.pendingProposalsByHash[qc.TargetHash]; ok {
				parentHash := pending.header.ParentHash
				_, parentProcessing := st.processingBlocks[parentHash]
				parentInState := st.proposalsByHashBlock[parentHash] != nil
				parentInChain := h.chain.GetHeaderByHash(parentHash) != nil

				if parentProcessing {
					// Parent is still being processed - DON'T remove from pending!
					// Let parent completion trigger this proposal naturally
					log.Info("[OnHsQuorumCert] QC received for pending block, but parent still processing - waiting",
						"view", qc.ViewNumber,
						"blockHash", qc.TargetHash.Hex()[:10],
						"blockNumber", pending.header.Number.Uint64(),
						"parentHash", parentHash.Hex()[:10])
					// Don't set pendingToProcess - let normal flow handle it
				} else if parentInState || parentInChain {
					// Parent is ready - can process immediately
					log.Info("[OnHsQuorumCert] QC received for pending block, parent ready - will process immediately",
						"view", qc.ViewNumber,
						"blockHash", qc.TargetHash.Hex()[:10],
						"blockNumber", pending.header.Number.Uint64(),
						"parentInState", parentInState,
						"parentInChain", parentInChain)
					pendingToProcess = pending
					// Clean up from pending maps
					delete(st.pendingProposalsByHash, qc.TargetHash)
					if parentPending := st.pendingProposals[parentHash]; len(parentPending) > 0 {
						newList := make([]*pendingProposal, 0, len(parentPending)-1)
						for _, p := range parentPending {
							if p.header.Hash() != qc.TargetHash {
								newList = append(newList, p)
							}
						}
						if len(newList) > 0 {
							st.pendingProposals[parentHash] = newList
						} else {
							delete(st.pendingProposals, parentHash)
						}
					}
				} else {
					// Parent not found and not processing - abnormal state
					log.Warn("[OnHsQuorumCert] QC received for pending block, but parent not found",
						"view", qc.ViewNumber,
						"blockHash", qc.TargetHash.Hex()[:10],
						"parentHash", parentHash.Hex()[:10])
				}
			} else {
				log.Debug("[OnHsQuorumCert] QC block not in state and no pending proposal",
					"view", qc.ViewNumber,
					"blockHash", qc.TargetHash.Hex()[:10])
			}
		}

		// Try 3-chain commit (may succeed if block exists, otherwise will return early)
		if err := h.tryCommitBlocks(st, qc.TargetHash, qc.ViewNumber); err != nil {
			log.Error("Failed to commit blocks after receiving QC", "view", qc.ViewNumber, "err", err)
		}

		// Check if we should advance view
		if qc.ViewNumber == math.MaxUint64 {
			h.lock.Unlock()
			return nil
		}
		newView = qc.ViewNumber + 1
		if newView > st.currentView {
			st.currentView = newView
			shouldAdvanceView = true
			log.Info("View advanced after receiving QC", "oldView", qc.ViewNumber, "newView", newView)
		}
		if err := h.checkpointHsStateLocked(); err != nil {
			log.Error("Failed to persist received QC", "view", qc.ViewNumber, "err", err)
		}
	}
	h.lock.Unlock()

	if shouldPrewriteQCBlock {
		h.prewriteBlock(qc.TargetHash)
	}

	// Phase 4.5: Process pending proposal triggered by QC (no lock - will acquire its own)
	if pendingToProcess != nil {
		log.Info("[OnHsQuorumCert] Processing QC-triggered pending proposal",
			"view", pendingToProcess.packet.View,
			"blockHash", pendingToProcess.header.Hash().Hex()[:10])
		// Process the pending proposal - this will execute the block and update state
		// Since we have the QC, the block is already certified by quorum, so we can trust it
		go func(p *pendingProposal) {
			if err := h.processQCCertifiedProposal(p, qc); err != nil {
				log.Warn("[OnHsQuorumCert] Failed to process QC-certified proposal",
					"view", p.packet.View,
					"err", err)
			}
		}(pendingToProcess)
	}

	// Phase 5: Post-update operations (no lock - may block)
	if shouldAdvanceView {
		// Check if we are the leader for the new view
		newLeader, err := h.getLeaderForViewAt(h.chain, qc.TargetHash, newView)
		if err != nil {
			log.Warn("Failed to get leader for new view", "view", newView, "err", err)
		} else if newLeader == h.ConsensusAddress() {
			// CRITICAL FIX: Only notify miner if highQC block state is ready!
			// If highQC block is still pending (state not committed), miner will fail
			// with "missing trie node" error. In that case, wait for pending proposal
			// to complete, which will trigger miner notification.
			h.lock.RLock()
			var highQCHash common.Hash
			var highQCBlockReady bool
			if st := h.getHsState(); st != nil && st.highQC != nil {
				highQCHash = st.highQC.BlockHash
				// Check if highQC block is ready (in proposalsByHashBlock or canonical chain)
				highQCBlockReady = st.proposalsByHashBlock[highQCHash] != nil
			}
			h.lock.RUnlock()

			if !highQCBlockReady {
				highQCBlockReady = h.chain.GetHeaderByHash(highQCHash) != nil
			}

			if highQCBlockReady {
				log.Info("We are the leader for new view after receiving QC, triggering proposal",
					"view", newView,
					"highQCHash", highQCHash.Hex()[:10])
				h.proposeFromHighQC(newView, highQCHash)
			} else {
				log.Info("We are the leader for new view, but highQC block not ready yet - waiting for pending proposal",
					"view", newView,
					"highQCHash", highQCHash.Hex()[:10],
					"note", "Miner will be notified after pending proposal is processed")
				// Don't notify miner now - it will be notified after pending proposal completes
				// See: OnHsProposal Phase 5 and processQCCertifiedProposal
			}
		}

		// Restart view timeout
		h.restartViewTimeout()
	}
	return nil
}

// processQCCertifiedProposal processes a pending proposal that has been certified by a QC.
// Since the QC proves the block's validity (quorum voted for it), we can process it
// without waiting for parent completion. This enables faster catch-up when QC arrives
// before parent block is fully processed.
func (h *Hotstuff) processQCCertifiedProposal(pending *pendingProposal, qc *hs.QuorumCertPacket) error {
	if pending == nil || pending.packet == nil || pending.header == nil || pending.body == nil {
		return errors.New("invalid pending proposal")
	}
	header := pending.header
	body := pending.body
	view := pending.packet.View
	headerHash := header.Hash()
	if qc == nil || qc.TargetHash != headerHash || qc.ViewNumber != view {
		return fmt.Errorf("QC does not certify pending proposal: proposal view %d hash %s", view, headerHash)
	}
	if err := h.validateProposalPacket(pending.packet, header, body); err != nil {
		return fmt.Errorf("invalid pending proposal packet/body: %w", err)
	}

	log.Info("[processQCCertifiedProposal] ENTER",
		"view", view,
		"blockNumber", header.Number.Uint64(),
		"blockHash", headerHash.Hex()[:10],
		"qcView", qc.ViewNumber)

	// Phase 1: Mark as processing
	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return errors.New("HotStuff state not initialized")
	}

	// Check if already processed
	if st.proposalsByHashBlock[headerHash] != nil {
		log.Debug("[processQCCertifiedProposal] Block already processed", "hash", headerHash.Hex()[:10])
		h.lock.Unlock()
		return nil
	}

	// Mark as processing
	st.processingBlocks[headerHash] = struct{}{}
	executingState := st
	h.lock.Unlock()

	// Phase 2: Verify header and execute block
	h.lock.Lock()
	parents, err := h.collectProposalParentsLocked(header)
	if err != nil {
		if st := h.getHsState(); st != nil {
			delete(st.processingBlocks, headerHash)
		}
		h.lock.Unlock()
		log.Warn("[processQCCertifiedProposal] Reject QC-certified proposal: failed to collect parent headers",
			"view", view,
			"hash", headerHash.Hex()[:10],
			"err", err)
		return err
	}
	if err := h.verifyHeader(h.chain, header, parents); err != nil {
		if st := h.getHsState(); st != nil {
			delete(st.processingBlocks, headerHash)
		}
		h.lock.Unlock()
		log.Warn("[processQCCertifiedProposal] Reject QC-certified proposal: header verification failed",
			"view", view,
			"hash", headerHash.Hex()[:10],
			"err", err)
		return err
	}
	h.lock.Unlock()

	// Even though QC proves validity, we still need to execute to:
	// 1. Generate receipts for storage
	// 2. Update state for subsequent blocks
	log.Info("[processQCCertifiedProposal] Executing block", "view", view, "hash", headerHash.Hex()[:10])
	executeStart := time.Now()
	receipts, err := h.executeBlocks(header, body.Transactions)
	hotstuffProposalExecuteTimer.Update(time.Since(executeStart))
	if err != nil {
		hotstuffProposalExecuteErrors.Inc(1)
	}
	if err != nil {
		h.lock.Lock()
		if st := h.getHsState(); st != nil {
			delete(st.processingBlocks, headerHash)
		}
		h.lock.Unlock()
		log.Warn("[processQCCertifiedProposal] Reject QC-certified proposal: block execution failed",
			"view", view,
			"hash", headerHash.Hex()[:10],
			"err", err)
		return err
	}

	// Phase 3: Update state
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
		delete(executingState.proposalStates, headerHash)
		h.lock.Unlock()
		return errors.New("HotStuff state lost")
	}

	// Construct block
	bodyData := types.Body{Transactions: body.Transactions}
	if header.EmptyWithdrawalsHash() {
		bodyData.Withdrawals = make([]*types.Withdrawal, 0)
	}
	block := types.NewBlockWithHeader(header).WithBody(bodyData)

	// Record in state
	st.proposalsByView[view] = header
	st.proposalsByHash[headerHash] = header
	st.proposalsByHashBlock[headerHash] = block
	st.proposalsByHashReceipts[headerHash] = receipts
	if view > st.currentView {
		st.currentView = view
	}

	// Update lock-free cache
	h.proposalBlocksCache.Store(headerHash, block)

	// Clean up processing
	delete(st.processingBlocks, headerHash)

	// Check for nested pending proposals
	var nestedPending []*pendingProposal
	if children := st.pendingProposals[headerHash]; len(children) > 0 {
		log.Info("[processQCCertifiedProposal] Found nested pending proposals",
			"parentHash", headerHash.Hex()[:10],
			"count", len(children))
		nestedPending = children
		delete(st.pendingProposals, headerHash)
		for _, p := range nestedPending {
			delete(st.pendingProposalsByHash, p.header.Hash())
		}
	}

	// Try 3-chain commit now that block is in state
	if err := h.tryCommitBlocks(st, headerHash, view); err != nil {
		log.Debug("[processQCCertifiedProposal] tryCommitBlocks failed", "view", view, "err", err)
	}

	h.lock.Unlock()

	// Phase 4: Process nested pending proposals
	for _, p := range nestedPending {
		go func(child *pendingProposal) {
			log.Debug("[processQCCertifiedProposal] Processing nested pending proposal",
				"view", child.packet.View,
				"hash", child.header.Hash().Hex()[:10])
			if err := h.OnHsProposal(child.peerID, child.packet); err != nil {
				log.Debug("Failed to process nested pending proposal", "err", err)
			}
		}(p)
	}

	// CRITICAL: Check if we need to notify miner after processing this proposal
	// This handles the case where QC arrived before proposal was processed,
	// and miner notification was deferred until now.
	h.lock.RLock()
	st = h.getHsState()
	var shouldNotifyMiner bool
	var currentViewForMiner uint64
	var highQCHashForMiner common.Hash
	if st != nil && st.highQC != nil && st.highQC.BlockHash == headerHash {
		// This proposal is now the highQC block
		currentViewForMiner = st.currentView
		highQCHashForMiner = st.highQC.BlockHash
		h.lock.RUnlock()

		// Check if we are the leader for current view
		leaderForCurrent, _ := h.getLeaderForViewAt(h.chain, highQCHashForMiner, currentViewForMiner)
		if leaderForCurrent == h.ConsensusAddress() {
			shouldNotifyMiner = true
		}
	} else {
		h.lock.RUnlock()
	}

	if shouldNotifyMiner {
		log.Info("[processQCCertifiedProposal] Proposal processed is highQC block, notifying miner",
			"proposalView", view,
			"currentView", currentViewForMiner,
			"highQCHash", highQCHashForMiner.Hex()[:10])
		h.proposeFromHighQC(currentViewForMiner, highQCHashForMiner)
	}

	log.Info("[processQCCertifiedProposal] EXIT - block processed and stored",
		"view", view,
		"blockNumber", header.Number.Uint64(),
		"blockHash", headerHash.Hex()[:10])

	h.prewriteBlock(headerHash)

	return nil
}

// internal helpers to reuse minimal state machine from relab adapter
func (h *Hotstuff) processProposalHeader(header *types.Header) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return nil
	}
	// Use view number from header, not block number
	view := getViewFromHeader(header, h.chainConfig)
	leader, _ := h.getLeaderForViewAt(h.chain, header.ParentHash, view)
	if leader != header.Coinbase {
		return nil
	}
	if st.lockedQC != nil && header.ParentHash != st.lockedQC.BlockHash {
		return nil
	}
	// Do not advance runtime highQC from header-only SyncInfo here; legacy
	// blocks may not carry aggregate proof.
	st.proposalsByView[view] = header
	st.proposalsByHash[header.Hash()] = header
	if view > st.currentView {
		st.currentView = view
	}
	// BLS sign and send vote
	if h.blsVoteSigner() == nil {
		return nil
	}

	if err := h.prepareVoteLocked(view, header.Hash()); err != nil {
		log.Error("Refusing to sign HotStuff vote without durable safety state", "view", view, "hash", header.Hash(), "err", err)
		return nil
	}
	digest := h.hsVoteDigest(header.Hash(), view)
	pub, sig, err := h.signHsVote(digest)
	if err != nil {
		return nil
	}
	vote := &hs.VotePacket{BlockHash: header.Hash(), ViewNumber: view, VotePubKey: pub, Signature: sig}
	// Use local validator address (no need to verify own address, receiver will verify)
	addr := h.ConsensusAddress()
	if st.votes[view] == nil {
		st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
	}
	if st.votes[view][header.Hash()] == nil {
		st.votes[view][header.Hash()] = make(map[common.Address]*hs.VotePacket)
	}
	st.votes[view][header.Hash()][addr] = vote
	h.sendHsVote(leader, vote)
	return nil
}

func (h *Hotstuff) processVotePacket(vp *hs.VotePacket) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return nil
	}
	// Verify BLS vote and resolve consensus address
	targetHeader := h.chain.GetHeaderByHash(vp.BlockHash)
	if targetHeader == nil {
		log.Warn("Target header not found in processVotePacket", "blockHash", vp.BlockHash)
		return nil
	}
	addr, blsAvailable, err := h.verifyHsBlsVoteAndResolveAddr(vp, targetHeader)
	if err != nil {
		log.Debug("Invalid hs vote in processVotePacket", "view", vp.ViewNumber, "err", err)
		return nil
	}
	if !blsAvailable {
		return nil
	}
	view := vp.ViewNumber
	if st.votes[view] == nil {
		st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
	}
	if st.votes[view][vp.BlockHash] == nil {
		st.votes[view][vp.BlockHash] = make(map[common.Address]*hs.VotePacket)
	}
	st.votes[view][vp.BlockHash][addr] = vp
	var snap *Snapshot
	if targetHeader.Number.Uint64() > 0 {
		snap, _ = h.snapshot(h.chain, targetHeader.Number.Uint64()-1, targetHeader.ParentHash, nil)
	} else if genesisHeader := h.chain.GetHeaderByNumber(0); genesisHeader != nil {
		snap, _ = h.snapshot(h.chain, 0, genesisHeader.Hash(), nil)
	}
	if snap == nil {
		log.Debug("processVotePacket failed to get target snapshot", "view", view, "blockHash", vp.BlockHash)
		return nil
	}
	qsize := QuorumSize(len(snap.validators()))
	cnt := len(st.votes[view][vp.BlockHash])
	if cnt >= qsize {
		qc := &HsQC{BlockHash: vp.BlockHash, View: view}
		// Aggregate signatures for QC
		agg := h.aggregateHsVoteSignatures(st.votes[view][vp.BlockHash])
		// Build SignersSet bitset (already have snap from above)
		var bitset types.ValidatorsBitSet
		validators := snap.validators()
		voters := st.votes[view][vp.BlockHash]
		for idx, valAddr := range validators {
			if _, ok := voters[valAddr]; ok {
				bitset |= types.ValidatorsBitSet(1) << uint(idx)
			}
		}
		qc.Sig = agg
		qc.SignersSet = bitset
		if !qc.hasAggregateProof() || !h.verifyAggregateQC(&hs.QuorumCertPacket{
			TargetHash: qc.BlockHash, ViewNumber: qc.View, SignersSet: qc.SignersSet, AggregateSig: qc.Sig,
		}) {
			log.Warn("processVotePacket formed QC without aggregate proof",
				"view", view,
				"blockHash", vp.BlockHash.Hex()[:8],
				"signersSet", qc.SignersSet,
				"aggSigLen", len(qc.Sig))
			return nil
		}
		if existing := st.qcsByView[view]; existing != nil && existing.BlockHash != qc.BlockHash {
			return nil
		}
		if st.highQC != nil && view == st.highQC.View && st.highQC.BlockHash != qc.BlockHash {
			return nil
		}
		st.qcsByView[view] = qc
		if st.highQC == nil || view > st.highQC.View {
			st.highQC = qc
		}
		err := h.broadcastHsQCWithAggLocked(vp.BlockHash, view, agg, bitset)
		if err != nil {
			log.Warn("processVotePacket broadcastHsQCWithAggLocked failed", err)
			return err
		}
		// Try 3-chain commit using the strict consecutive view rule
		if err := h.tryCommitBlocks(st, vp.BlockHash, view); err != nil {
			log.Error("Failed to commit blocks in processVotePacket", "view", view, "err", err)
		}
		if err := h.checkpointHsStateLocked(); err != nil {
			log.Error("Failed to persist formed QC", "view", view, "err", err)
		}
	}
	return nil
}

func (h *Hotstuff) processNewViewPacket(peerID string, nv *hs.NewViewPacket) error {
	if !h.hsProtocolActive() {
		return errHotstuffProtocolInactive
	}
	v := newViewTargetView(nv)
	if !h.verifyNewViewTimeoutCert(nv, v) {
		return nil
	}
	addr, err := h.resolveNewViewValidator(peerID, nv)
	if err != nil {
		return nil
	}

	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return nil
	}
	if st.newViews[v] == nil {
		st.newViews[v] = make(map[common.Address]*hs.NewViewPacket)
	}
	st.newViews[v][addr] = nv

	// Update highTCView if NewView carries higher TC
	if nv.HighTCView > st.highTCView {
		log.Debug("processNewViewPacket updating highTCView", "oldView", st.highTCView, "newView", nv.HighTCView, "peer", peerID)
		st.highTCView = nv.HighTCView
	}
	newViewCount := len(st.newViews[v])
	newViewsCopy := make(map[common.Address]*hs.NewViewPacket, newViewCount)
	for k, val := range st.newViews[v] {
		newViewsCopy[k] = val
	}
	currentHighTCView := st.highTCView
	h.lock.Unlock()

	{
		// Pick highest QC and highest TC among new-views
		var maxQCView uint64
		var base common.Hash
		var maxTCView uint64
		for _, m := range newViewsCopy {
			if m.HighQCView > maxQCView || (m.HighQCView == maxQCView && bytes.Compare(m.HighQCHash[:], base[:]) > 0) {
				maxQCView = m.HighQCView
				base = m.HighQCHash
			}
			if m.HighTCView > maxTCView {
				maxTCView = m.HighTCView
			}
		}
		if base == (common.Hash{}) {
			return nil
		}
		snap, err := h.getSnapshotAtHashOrView(h.chain, base, maxQCView)
		if err != nil || snap == nil {
			return nil
		}
		validNewViews := 0
		maxTCView = 0
		for addr, message := range newViewsCopy {
			if _, ok := snap.Validators[addr]; ok {
				validNewViews++
				if message.HighTCView > maxTCView {
					maxTCView = message.HighTCView
				}
			}
		}
		if validNewViews < QuorumSize(len(snap.validators())) {
			return nil
		}
		if selectedLeader, err := h.getLeaderForViewAt(h.chain, base, v); err != nil || selectedLeader != h.ConsensusAddress() {
			return nil
		}
		// Update highTCView to the highest from quorum
		if maxTCView > currentHighTCView {
			h.lock.Lock()
			if st = h.getHsState(); st != nil && maxTCView > st.highTCView {
				st.highTCView = maxTCView
			}
			h.lock.Unlock()
		}
		h.proposeFromHighQC(v, base)
	}
	return nil
}

// restartViewTimeout restarts the per-view timeout timer and schedules local timeout
// CRITICAL: This function manages its own locks. Caller should NOT hold lock when calling.
func (h *Hotstuff) restartViewTimeout() {
	// Phase 1: Get current view (short lock)
	h.lock.RLock()
	st := h.getHsState()
	if st == nil || h.closed {
		h.lock.RUnlock()
		return
	}
	view := st.currentView
	h.lock.RUnlock()

	// Phase 2: Calculate timeout (no lock - may call snapshot which can block)
	base := h.hsBaseTimeoutMS
	if h.chain != nil {
		head := h.chain.CurrentHeader()
		if head != nil {
			if snap, err := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil); err == nil && snap != nil {
				if snap.BlockInterval > 0 {
					base = snap.BlockInterval + 5000 // 5 second buffer
				}
			}
		}
	}
	if base == 0 {
		base = defaultBlockInterval + 5000 // 5 second buffer
	}
	log.Info("restartViewTimeout: resetting timer", "view", view, "timeoutMs", base)

	// Phase 3: Update timer (short lock)
	h.lock.Lock()
	if h.closed {
		h.lock.Unlock()
		return
	}
	if h.hsTimer != nil {
		h.hsTimer.Stop()
	}
	h.hsTimer = time.AfterFunc(time.Duration(base)*time.Millisecond, func() {
		h.onLocalViewTimeout(view)
	})
	h.lock.Unlock()
}

// onLocalViewTimeout constructs, verifies and broadcasts a local TimeoutPacket.
func (h *Hotstuff) onLocalViewTimeout(view uint64) {
	log.Info("[onLocalViewTimeout] ENTER", "view", view)
	if !h.hsProtocolActive() {
		h.restartViewTimeout()
		return
	}

	// Snapshot the current safety state, then release the lock before wallet and
	// BLS operations.
	h.lock.RLock()
	st := h.getHsState()
	if st == nil || view != st.currentView {
		log.Info("[onLocalViewTimeout] EXIT - state nil or view mismatch", "view", view, "currentView", func() uint64 {
			if st != nil {
				return st.currentView
			}
			return 0
		}())
		h.lock.RUnlock()
		return
	}
	log.Info("[onLocalViewTimeout] Processing timeout", "view", view)
	to := &hs.TimeoutPacket{ViewNumber: view}
	if st.highQC != nil {
		to.HighQCView = st.highQC.View
		to.HighQCHash = st.highQC.BlockHash
		to.HighQCSignersSet = st.highQC.SignersSet
		to.HighQCAggSig = common.CopyBytes(st.highQC.Sig)
	}
	h.lock.RUnlock()

	if h.blsVoteSigner() == nil {
		log.Debug("Skip local timeout without BLS signer", "view", view)
		h.restartViewTimeout()
		return
	}
	root := h.hsTimeoutDigest(to)
	pub, sig, err := h.signHsVote(root)
	if err != nil {
		log.Warn("Failed to sign local HotStuff timeout", "view", view, "err", err)
		h.restartViewTimeout()
		return
	}
	to.VotePubKey = pub
	to.Signature = sig

	// Recheck the view before publishing the packet as this node's latest local
	// timeout. OnHsTimeout performs full proof, membership and quorum validation.
	h.lock.Lock()
	st = h.getHsState()
	if st == nil || st.currentView != view {
		h.lock.Unlock()
		return
	}
	h.lastTimeoutView = view
	h.lastTimeoutPacket = to
	h.lock.Unlock()
	if err := h.OnHsTimeout("local", to); err != nil {
		log.Debug("Local timeout validation failed", "view", view, "err", err)
		return
	}

	if network := h.hsNetwork(); network != nil {
		network.BroadcastTimeout(to)
	}
}

// proposeFromHighQC builds and broadcasts a proposal for view v extending base block.
// In Chained HotStuff, we ALWAYS propose a NEW block extending highQC, regardless of view timeout.
// This enables pipelining: blocks N, N+1, N+2... can be proposed continuously without waiting for commits.
// Commits happen asynchronously via the 3-chain rule.
func (h *Hotstuff) proposeFromHighQC(view uint64, base common.Hash) {
	log.Info("proposeFromHighQC called (Chained HotStuff)", "view", view, "base", base)

	// In Chained HotStuff, we don't re-propose the same block at the same height.
	// Instead, we ALWAYS propose a NEW block (height+1) that extends the highQC block.
	// The miner's normal flow (commitWork -> Seal) will handle this.

	// The key insight:
	// - View timeout means the previous view's proposal didn't get QC
	// - New leader should propose a NEW block (not retry old one)
	// - This new block extends highQC (which might be several blocks back)
	// - Commits happen asynchronously via 3-chain rule

	// Strategy: Signal to miner that we're ready to propose
	// The miner's periodic timer will trigger commitWork -> Seal
	// Seal will embed the current view number and broadcast

	// For now, we rely on miner's newWorkLoop timer to trigger block production
	// The view number is tracked in hsState and will be used by Seal

	log.Info("proposeFromHighQC: notifying miner to immediately produce block", "view", view, "extendsQC", base)

	// Notify miner to immediately produce a block instead of waiting for the timer
	// This is critical because the view timeout (5s) is shorter than miner's recommit interval (10s),
	// which would cause continuous view timeouts if we wait for the timer.
	select {
	case h.notifyMinerCh <- struct{}{}:
		log.Info("proposeFromHighQC: miner notification sent", "view", view)
	default:
		// Channel is full, skip (non-blocking send)
		log.Debug("proposeFromHighQC: miner notification channel full, skipping", "view", view)
	}
}

func (h *Hotstuff) onHsCommit(hash common.Hash) {
	// CRITICAL: Insert block into canonical chain via InsertChain
	// Without this, block is only in rawdb but not in blockchain's canonical chain
	if h.chain == nil {
		log.Warn("onHsCommit: chain reader not set", "hash", hash)
		return
	}

	// Get the block to commit
	type blockChainReader interface {
		GetBlock(common.Hash, uint64) *types.Block
	}

	var block *types.Block
	if bcr, ok := h.chain.(blockChainReader); ok {
		// Try to get from chain first
		header := h.chain.GetHeaderByHash(hash)
		if header == nil {
			log.Warn("onHsCommit: header not found", "hash", hash)
			return
		}
		block = bcr.GetBlock(hash, header.Number.Uint64())
	}

	// If not in chain, try to get from hsState (prewritten blocks)
	if block == nil {
		h.lock.RLock()
		st := h.getHsState()
		if st != nil {
			block = st.proposalsByHashBlock[hash]
		}
		h.lock.RUnlock()
	}

	if block == nil {
		log.Error("onHsCommit: block not found anywhere", "hash", hash.Hex()[:8])
		return
	}

	log.Info("HotStuff 3-chain commit: inserting block into canonical chain",
		"number", block.NumberU64(),
		"hash", hash.Hex()[:8],
		"txs", len(block.Transactions()))

	if block.NumberU64() == 0 {
		log.Info("Genesis block committed to canonical chain")
		return
	}

	// ✅ CRITICAL: Call InsertChain to add block to canonical chain
	// This is what makes the block visible via RPC (eth_blockNumber, eth_getBlockByNumber, etc.)
	if err := h.commitBlock(block); err != nil {
		log.Error("onHsCommit: failed to commit block to canonical chain",
			"number", block.NumberU64(),
			"hash", hash.Hex()[:8],
			"err", err)
		return
	}

	log.Info("HotStuff block committed to canonical chain",
		"number", block.NumberU64(),
		"hash", hash.Hex()[:8])

	// Also set as finalized if supported
	type finalizedChain interface{ SetFinalized(*types.Header) }
	if bc, ok := h.chain.(finalizedChain); ok {
		bc.SetFinalized(block.Header())
		log.Info("HotStuff block marked as finalized",
			"number", block.NumberU64(),
			"hash", hash.Hex()[:8])
	}
}

// Network functions moved to hotstuff_network.go

// Signature functions moved to hotstuff_signature.go

// getChain is a placeholder to retrieve the chain reader from the surrounding service
func (h *Hotstuff) getChain() consensus.ChainHeaderReader { return h.chain }

// GetProposalParent returns the block hash that should be used as parent for next proposal.
// In Chained HotStuff, this is the block pointed to by highQC (not the committed chain head).
// This enables pipelining: Block1 <- QC1 <- Block2 <- QC2 <- Block3 <- QC3
func (h *Hotstuff) GetProposalParent(chain consensus.ChainHeaderReader) common.Hash {
	h.ensureBootstrapAtHead()
	h.lock.RLock()
	defer h.lock.RUnlock()

	st := h.getHsState()
	if st == nil || st.highQC == nil {
		// No highQC yet, use current chain head (genesis or last committed block)
		return chain.CurrentHeader().Hash()
	}

	// IMPORTANT: For Chained HotStuff, MUST use highQC block as parent
	// This maintains the chain property: Block1 <- QC1 <- Block2 <- QC2 <- Block3
	//
	// Note: The highQC block might only be prewritten (not committed yet).
	// The state availability issue should be handled by:
	// 1. executeBlocks has fallback to lightweight validation if state unavailable
	// 2. miner's prepareWork should handle state creation gracefully
	// 3. prewriteBlock ensures block data is available in rawdb

	// CRITICAL FIX: Check if highQC block was already proposed in any view
	// This prevents duplicate proposals when view advances but highQC block was already proposed
	currentView := st.currentView

	log.Debug("GetProposalParent: detailed state",
		"currentView", currentView,
		"highQC_view", st.highQC.View,
		"highQC_hash", st.highQC.BlockHash.Hex()[:8],
		"proposalsByViewCount", len(st.proposalsByView))

	// Log all existing proposals
	// for view, header := range st.proposalsByView {
	// 	log.Debug("GetProposalParent: existing proposal",
	// 		"view", view,
	// 		"blockNumber", header.Number.Uint64(),
	// 		"hash", header.Hash().Hex()[:8])
	// }

	highQCBlock := st.proposalsByHashBlock[st.highQC.BlockHash]
	if highQCBlock != nil {
		log.Debug("GetProposalParent: highQC block found in proposals",
			"blockNumber", highQCBlock.NumberU64(),
			"hash", st.highQC.BlockHash.Hex()[:8])

		// Check if this block was already proposed in currentView or any previous view
		for view, proposalHeader := range st.proposalsByView {
			if proposalHeader.Hash() == st.highQC.BlockHash {
				// This block was already proposed in view 'view'
				// If it's the current view, we should use chain head
				// If it's a previous view, we should still use chain head to avoid duplicate
				if view >= currentView {
					log.Warn("GetProposalParent: highQC block already proposed in current/newer view, using chain head",
						"highQC_hash", st.highQC.BlockHash.Hex()[:8],
						"highQC_view", st.highQC.View,
						"proposedView", view,
						"currentView", currentView,
						"chainHeadNumber", chain.CurrentHeader().Number.Uint64(),
						"chainHead", chain.CurrentHeader().Hash().Hex()[:8])
					return chain.CurrentHeader().Hash()
				}
			}
		}
	} else {
		log.Debug("GetProposalParent: highQC block NOT found in proposals (already committed?)",
			"hash", st.highQC.BlockHash.Hex()[:8])
	}

	log.Debug("GetProposalParent using highQC (Chained HotStuff)",
		"highQC_hash", st.highQC.BlockHash.Hex()[:8],
		"highQC_view", st.highQC.View,
		"currentView", currentView,
		"chainHeadNumber", chain.CurrentHeader().Number.Uint64(),
		"currentHeader", chain.CurrentHeader().Hash().Hex()[:8])

	// Return highQC block hash to maintain chain structure
	parentHash := st.highQC.BlockHash

	log.Error("GetProposalParent: starting validation",
		"highQCHash", parentHash.Hex()[:8],
		"highQCView", st.highQC.View,
		"currentView", currentView,
		"proposalsByHashBlockCount", len(st.proposalsByHashBlock))

	// CRITICAL FIX: Verify that the parent block actually exists
	// The highQC might point to a block that's been cleaned up from HotStuff state
	// or is on a different fork. If the block doesn't exist, fall back to chain head.
	parentExists := false

	// Check if parent is in canonical chain
	if chain.GetHeaderByHash(parentHash) != nil {
		parentExists = true
		log.Error("GetProposalParent: parent found in canonical chain", "hash", parentHash.Hex()[:8])
	}

	// Check if parent is in HotStuff state
	if !parentExists && st.proposalsByHashBlock[parentHash] != nil {
		parentExists = true
		log.Error("GetProposalParent: parent found in HotStuff state", "hash", parentHash.Hex()[:8])
	}

	// If parent doesn't exist anywhere, fall back to chain head
	if !parentExists {
		log.Error("GetProposalParent: highQC parent NOT FOUND, falling back to chain head",
			"highQCHash", parentHash.Hex()[:8],
			"highQCView", st.highQC.View,
			"chainHead", chain.CurrentHeader().Hash().Hex()[:8],
			"chainHeadNumber", chain.CurrentHeader().Number.Uint64())
		return chain.CurrentHeader().Hash()
	}

	log.Error("GetProposalParent FINAL DECISION - CRITICAL",
		"returning", parentHash.Hex()[:10],
		"returningSuffix", parentHash.Hex()[len(parentHash.Hex())-10:],
		"highQCView", st.highQC.View,
		"currentView", currentView,
		"fullHash", parentHash.Hex())
	return parentHash
}
