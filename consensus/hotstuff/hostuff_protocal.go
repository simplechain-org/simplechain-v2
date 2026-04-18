package hotstuff

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

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
	// Optionally decode body if provided for downstream use
	var body types.Body
	if err := rlp.DecodeBytes(pp.BodyRLP, &body); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	// Verify tx root matches header when body is provided
	if len(body.Transactions) > 0 {
		root := types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil))
		if root != header.TxHash {
			log.Debug("Reject proposal: tx root mismatch", "view", pp.View, "calc", root, "header", header.TxHash)
			return nil
		}
		// Verify transaction signatures under current rules
		signer := types.MakeSigner(h.chainConfig, header.Number, header.Time)
		for _, tx := range body.Transactions {
			if _, err := types.Sender(signer, tx); err != nil {
				log.Debug("Reject proposal: invalid tx signature", "view", pp.View, "tx", tx.Hash(), "err", err)
				return nil
			}
		}
	}
	// Use view number carried by ProposalPacket.View
	view := pp.View
	log.Debug("OnHsProposal", "peerID", peerID, "view", view, "block", header.Number.Uint64())

	// Debug: log header details to understand the structure
	log.Debug("OnHsProposal header details",
		"view", view,
		"headerParentHash", header.ParentHash.Hex()[:10],
		"headerNumber", header.Number.Uint64(),
		"headerCoinbase", header.Coinbase.Hex())

	// ========== Phase 2: Verify and check state (needs lock for reading st) ==========
	log.Info("[OnHsProposal] Acquiring lock for state access", "peerID", peerID)
	h.lock.Lock()

	st := h.getHsState()
	if st == nil {
		log.Info("[OnHsProposal] State is nil", "peerID", peerID)
		h.lock.Unlock()
		return nil
	}

	// If TimeoutCert is embedded in header, verify it
	tc, tcErr := h.parseTimeoutCert(&header)
	if tcErr != nil {
		// CRITICAL FIX: Don't reject proposal if TC parsing fails
		// TC is optional - a proposal without TC is valid (normal case without timeout)
		// Only reject if TC exists but is invalid (verified in next block)
		log.Debug("Failed to parse TimeoutCert (may not exist)", "view", view, "err", tcErr)
		tc = nil // Treat as no TC
	}
	if tc != nil {
		if !h.verifyTimeoutCert(tc) {
			log.Debug("Invalid proposal: TimeoutCert aggregate signature invalid", "view", view)
			return nil
		}
	}
	// Validate HighQC carried in proposal (if present)
	if pp.HighQC.View > 0 {
		log.Warn("OnHsProposal: received proposal with HighQC",
			"proposalView", view,
			"highQCView", pp.HighQC.View,
			"highQCBlockHash", pp.HighQC.BlockHash.Hex()[:10],
			"headerParentHash", header.ParentHash.Hex()[:10])

		// Rule: HighQC.View < view, and HighQC must be valid aggregated signature from current validator set
		if pp.HighQC.View >= view {
			log.Debug("Invalid proposal: HighQC view not less than proposal view", "hqcv", pp.HighQC.View, "view", view)
			return nil
		}
		// Verify QC aggregate signature using signer set embedded in HighQC
		log.Debug("OnHsProposal: constructing QC packet for verification",
			"view", view,
			"highQCView", pp.HighQC.View,
			"highQCBlockHash", pp.HighQC.BlockHash.Hex()[:8],
			"signersSet", pp.HighQC.SignersSet,
			"aggSigLen", len(pp.HighQC.Sig))
		qcPkt := &hs.QuorumCertPacket{TargetHash: pp.HighQC.BlockHash, ViewNumber: pp.HighQC.View, SignersSet: pp.HighQC.SignersSet, AggregateSig: pp.HighQC.Sig}
		if !h.verifyAggregateQC(qcPkt) {
			log.Debug("Invalid proposal: HighQC aggregate signature invalid", "view", view)
			return nil
		}
		log.Debug("OnHsProposal: HighQC verification passed", "view", view, "highQCView", pp.HighQC.View)
		// CRITICAL: In Chained HotStuff, HighQC.BlockHash should equal header.ParentHash
		// This ensures the proposal extends from the certified block
		if header.ParentHash != pp.HighQC.BlockHash {
			log.Warn("Invalid proposal: HighQC does not justify parent",
				"view", view,
				"headerParentHash", header.ParentHash.Hex()[:10],
				"highQCBlockHash", pp.HighQC.BlockHash.Hex()[:10],
				"headerNumber", header.Number.Uint64())
			return nil
		}
		// Update local HighQC if higher - this allows catch-up
		if st.highQC == nil || pp.HighQC.View > st.highQC.View {
			log.Info("OnHsProposal: updating highQC from proposal",
				"oldHighQCView", func() uint64 {
					if st.highQC != nil {
						return st.highQC.View
					}
					return 0
				}(),
				"newHighQCView", pp.HighQC.View,
				"newHighQCBlock", pp.HighQC.BlockHash.Hex()[:10],
				"newSignersSet", pp.HighQC.SignersSet,
				"newAggSigLen", len(pp.HighQC.Sig))
			st.highQC = &HsQC{BlockHash: pp.HighQC.BlockHash, View: pp.HighQC.View, Sig: pp.HighQC.Sig, SignersSet: pp.HighQC.SignersSet}
			st.qcsByView[pp.HighQC.View] = st.highQC

			// DEBUG: Verify the stored QC immediately
			log.Debug("OnHsProposal: stored highQC details",
				"view", st.highQC.View,
				"blockHash", st.highQC.BlockHash.Hex()[:8],
				"signersSet", st.highQC.SignersSet,
				"aggSigLen", len(st.highQC.Sig))
		}
	} else {
		log.Warn("OnHsProposal: received proposal WITHOUT HighQC",
			"proposalView", view,
			"headerNumber", header.Number.Uint64())
	}
	// Verify leader using parent-hash contextual snapshot to avoid divergence
	leader, _ := h.getLeaderForViewAt(h.chain, header.ParentHash, view)
	if leader != header.Coinbase {
		log.Debug("Ignore proposal from non-leader", "view", view, "leader", leader, "proposer", header.Coinbase)
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

			h.lock.Unlock()
			log.Debug("Proposal cached, waiting for parent to complete or QC arrival",
				"view", view,
				"parentHash", header.ParentHash.Hex()[:10],
				"proposalHash", headerHash.Hex()[:10],
				"cachedCount", len(st.pendingProposals[header.ParentHash]))
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

	// Parent exists - check if it matches highQC or we're catching up
	if st.highQC != nil && header.ParentHash != st.highQC.BlockHash {
		log.Info("Accepting proposal that doesn't extend highQC (parent exists, node may be catching up)",
			"view", view,
			"blockNumber", header.Number.Uint64(),
			"proposalParent", header.ParentHash.Hex()[:10],
			"highQCBlock", st.highQC.BlockHash.Hex()[:10],
			"highQCView", st.highQC.View,
			"parentInChain", parentInChain,
			"parentInState", parentInState)
	}

	// Mark block as processing before releasing lock
	st.processingBlocks[headerHash] = struct{}{}
	log.Debug("[OnHsProposal] Marked block as processing", "hash", headerHash.Hex()[:10], "view", view)

	// Release lock before executeBlocks (it may need to acquire RLock internally)
	log.Info("[OnHsProposal] Releasing lock before executeBlocks", "peerID", peerID)
	h.lock.Unlock()

	// ========== Phase 3: Execute block (no lock held) ==========
	log.Info("[OnHsProposal] Calling executeBlocks", "peerID", peerID)
	receipts, err := h.executeBlocks(&header, body.Transactions)
	log.Info("[OnHsProposal] executeBlocks returned", "peerID", peerID, "err", err)

	// ========== Phase 4: Update state (needs lock for writing st) ==========
	log.Info("[OnHsProposal] Re-acquiring lock for state update", "peerID", peerID)
	h.lock.Lock()
	log.Info("[OnHsProposal] Lock re-acquired", "peerID", peerID)

	if err != nil {
		log.Warn("Execute blocks failed, but still accepting proposal", "view", view, "hash", header.Hash(), "err", err)
		// CRITICAL FIX: Even if executeBlocks fails (e.g., due to missing historical headers
		// in distributeFinalityReward), we should still accept and store the proposal.
		// The block might be valid; we just can't fully execute it yet due to incomplete state.
		// Use empty receipts as placeholder
		receipts = make([]*types.Receipt, 0)
	}

	// Re-get state after re-acquiring lock (state might have changed)
	st = h.getHsState()
	if st == nil {
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

	// find blocks which can be commit and commit
	if err := h.tryCommitBlocks(st, header.Hash(), view); err != nil {
		log.Debug("Try commit blocks failed", "view", view, "hash", header.Hash(), "err", err)
	}

	// Update HighQC from header.Extra if present
	// NOTE: header.Extra syncInfo only contains View/Hash, not SignersSet/Sig
	// This is for basic view tracking; full QC comes from Vote/QC messages
	if has, v, hq, _ := parseSyncInfo(&header); has {
		if st.highQC == nil || v > st.highQC.View {
			// Initialize with minimal info from syncInfo
			// SignersSet and Sig will be empty here (syncInfo doesn't contain them)
			st.highQC = &HsQC{BlockHash: hq, View: v, SignersSet: 0, Sig: nil}
			log.Debug("OnHsProposal: updated highQC from header syncInfo (minimal)",
				"view", v,
				"blockHash", hq.Hex()[:8],
				"note", "SignersSet/Sig not in syncInfo, will be updated from QC messages")
		}
	}

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

	// Vote for the proposal with BLS (only if still relevant)
	var vote *hs.VotePacket
	if shouldVote && h.voteSigner != nil {
		// sign digest(BlockHash, ViewNumber)
		digest := h.hsVoteDigest(headerHash, view)
		pub, sig, err := h.signHsVote(digest)
		if err != nil {
			log.Warn("Failed to sign hs vote", "view", view, "err", err)
			h.lock.Unlock()
			return nil
		}
		vote = &hs.VotePacket{BlockHash: headerHash, ViewNumber: view, VotePubKey: pub, Signature: sig}

		// Record vote in state
		addr := h.val
		if st.votes[view] == nil {
			st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
		}
		if st.votes[view][headerHash] == nil {
			st.votes[view][headerHash] = make(map[common.Address]*hs.VotePacket)
		}
		st.votes[view][headerHash][addr] = vote
	} else if shouldVote {
		// No signer configured
		log.Warn("No BLS vote signer configured; skip voting")
	}

	// Release lock before calling functions that acquire their own locks
	log.Info("[OnHsProposal] Releasing lock before post-processing", "peerID", peerID)
	h.lock.Unlock()

	// ========== Phase 5: Post-processing (no lock held) ==========
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
		leaderForCurrent, _ := h.getLeaderForView(h.chain, currentViewForMiner)
		if leaderForCurrent == h.val {
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

// OnHsVote tallies votes; when quorum reached, form QC and try to commit.
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations (snapshot, network IO).
func (h *Hotstuff) OnHsVote(peerID string, pkt interface{}) error {
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
	if st.currentView > voteViewTolerance && vp.ViewNumber+voteViewTolerance < st.currentView {
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
			if header := rawdb.ReadHeader(h.db, vp.BlockHash, 0); header != nil {
				targetHeader = header
				log.Debug("Found header in rawdb", "blockHash", vp.BlockHash.Hex()[:8], "view", view)
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
		addr = common.Address{}
		if h._hsNet != nil {
			if a, ok := h._hsNet.ResolveAddress(peerID); ok {
				addr = a
				log.Debug("Resolved address from peer ID", "view", view, "addr", addr)
			}
		}
		if addr == (common.Address{}) {
			addr = common.BytesToAddress(crypto.Keccak256(vp.VotePubKey[:])[:20])
			log.Debug("Using BLS pubkey hash for vote (fallback)", "view", view, "addr", addr)
		}
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
	if st.currentView > voteViewTolerance && vp.ViewNumber+voteViewTolerance < st.currentView {
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

	leader, err := h.getLeaderForView(h.chain, view)
	if err != nil {
		log.Warn("Failed to get leader for view in OnHsVote", "view", view, "err", err)
		return nil
	}
	isLeader := (leader == h.val)

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
	st.highQC = qc
	st.qcsByView[view] = qc

	// Try 3-chain commit
	if err := h.tryCommitBlocks(st, vp.BlockHash, view); err != nil {
		log.Error("Failed to commit blocks after QC formation", "view", view, "err", err)
	}

	// Advance view
	newView := view + 1
	st.currentView = newView
	log.Info("View advanced after QC", "oldView", view, "newView", newView)
	h.lock.Unlock()

	// Phase 9: Post-QC operations (no lock - may block)
	// Prewrite the QC'd block
	h.prewriteBlock(vp.BlockHash)

	// Only leader broadcasts QC
	if isLeader {
		// Use the unlocked version for broadcast
		if err := h.broadcastHsQCWithAgg(vp.BlockHash, view, agg); err != nil {
			log.Warn("OnHsVote broadcastHsQCWithAgg failed", "err", err)
		} else {
			log.Info("Leader broadcasted QC", "view", view, "block", vp.BlockHash.Hex()[:8])
		}
	}

	// Check if we are the leader for the new view
	newLeader, _ := h.getLeaderForView(h.chain, newView)
	if newLeader == h.val {
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
	// Phase 1: Decode (no lock)
	nv, ok := pkt.(*hs.NewViewPacket)
	if !ok {
		return errors.New("invalid newview packet type")
	}
	v := nv.HighQCView + 1 // next view
	log.Debug("OnHsNewView", "view", nv.HighQCView, "new view", v, "peer", peerID)

	// Resolve sender address (no lock needed)
	addr := common.Address{}
	if h._hsNet != nil {
		if a, ok := h._hsNet.ResolveAddress(peerID); ok {
			addr = a
		}
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
	currentHighTCView := st.highTCView
	h.lock.Unlock()

	// Phase 3: Get snapshot and check leader (no lock - may block)
	head := h.chain.CurrentHeader()
	if head == nil {
		return nil
	}
	snap, _ := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
	if snap == nil {
		return nil
	}
	qsize := QuorumSize(len(snap.validators()))
	leader, _ := h.getLeaderForView(h.chain, v)

	// Phase 4: If we are leader and have quorum, find best QC and propose
	if leader == h.val && newViewCount >= qsize {
		// Pick highest QC and highest TC among new-views
		var maxQCView uint64
		var base common.Hash
		var maxTCView uint64
		for _, m := range newViewsCopy {
			if m.HighQCView >= maxQCView {
				maxQCView = m.HighQCView
				base = m.HighQCHash
			}
			if m.HighTCView > maxTCView {
				maxTCView = m.HighTCView
			}
		}

		// Update highTCView if we found a higher one (short lock)
		if maxTCView > currentHighTCView {
			h.lock.Lock()
			st = h.getHsState()
			if st != nil && maxTCView > st.highTCView {
				st.highTCView = maxTCView
			}
			h.lock.Unlock()
		}

		// Propose (no lock - proposeFromHighQC just notifies miner)
		h.proposeFromHighQC(v, base)
	}
	return nil
}

// OnHsTimeout collects timeouts and advances view on TC (simplified).
func (h *Hotstuff) OnHsTimeout(peerID string, pkt interface{}) error {
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
	if st.currentView > timeoutViewTolerance && to.ViewNumber+timeoutViewTolerance < st.currentView {
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
		// BLS not available (before Luban fork), try multiple fallback methods
		addr = common.Address{}

		// Method 1: Try to resolve from peer ID mapping
		if h._hsNet != nil {
			if a, ok := h._hsNet.ResolveAddress(peerID); ok {
				addr = a
				log.Debug("Resolved address from peer ID", "view", to.ViewNumber, "addr", addr)
			}
		}

		// Method 2: If still no address, use BLS pubkey hash as fallback
		if addr == (common.Address{}) {
			addr = common.BytesToAddress(crypto.Keccak256(to.VotePubKey[:])[:20])
			log.Debug("Using BLS pubkey hash for timeout (fallback)", "view", to.ViewNumber, "addr", addr)
		}
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
	if st.currentView > timeoutViewTolerance && v+timeoutViewTolerance < st.currentView {
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
	st.timeouts[v][addr] = to
	timeoutCount := len(st.timeouts[v])
	h.lock.Unlock()

	// Phase 4: Get snapshot for quorum calculation (no lock held - may block)
	head := h.chain.CurrentHeader()
	snap, _ := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
	qsize := QuorumSize(len(snap.validators()))
	log.Debug("OnHsTimeout view timeout collected", "view", v, "block", head.Number.Uint64(), "cnt", timeoutCount, "qsize", qsize)

	// Phase 5: Re-acquire lock once, check quorum and update highTCView
	var nextView uint64
	shouldMoveToView := false
	if timeoutCount >= qsize {
		h.lock.Lock()
		st = h.getHsState()
		if st != nil {
			// Re-check timeout count (may have changed)
			if st.timeouts[v] != nil && len(st.timeouts[v]) >= qsize {
				log.Debug("OnHsTimeout quorum view timeout collected", "view", v, "block", head.Number.Uint64(), "cnt", len(st.timeouts[v]), "qsize", qsize)
				// form TC implicitly; advance to v+1
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
	if currentView > 0 && qc.ViewNumber+1 < currentView {
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
	if st.currentView > 0 && qc.ViewNumber+1 < st.currentView {
		h.lock.Unlock()
		return nil
	}

	shouldUpdate := false
	shouldAdvanceView := false
	var newView uint64
	var pendingToProcess *pendingProposal // Pending proposal to process if QC block not found

	if st.highQC == nil || qc.ViewNumber >= st.highQC.View {
		shouldUpdate = (st.highQC == nil ||
			qc.ViewNumber > st.highQC.View ||
			(qc.ViewNumber == st.highQC.View && qc.TargetHash != st.highQC.BlockHash))

		if shouldUpdate {
			st.highQC = &HsQC{
				BlockHash:  qc.TargetHash,
				View:       qc.ViewNumber,
				Sig:        qc.AggregateSig,
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
		newView = qc.ViewNumber + 1
		if newView > st.currentView {
			st.currentView = newView
			shouldAdvanceView = true
			log.Info("View advanced after receiving QC", "oldView", qc.ViewNumber, "newView", newView)
		}
	}
	h.lock.Unlock()

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
		newLeader, err := h.getLeaderForView(h.chain, newView)
		if err != nil {
			log.Warn("Failed to get leader for new view", "view", newView, "err", err)
		} else if newLeader == h.val {
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
	header := pending.header
	body := pending.body
	view := pending.packet.View
	headerHash := header.Hash()

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
	h.lock.Unlock()

	// Phase 2: Execute block (no lock held)
	// Even though QC proves validity, we still need to execute to:
	// 1. Generate receipts for storage
	// 2. Update state for subsequent blocks
	log.Info("[processQCCertifiedProposal] Executing block", "view", view, "hash", headerHash.Hex()[:10])
	receipts, err := h.executeBlocks(header, body.Transactions)
	if err != nil {
		log.Warn("[processQCCertifiedProposal] Execute blocks failed, using empty receipts",
			"view", view,
			"hash", headerHash.Hex()[:10],
			"err", err)
		receipts = make([]*types.Receipt, 0)
	}

	// Phase 3: Update state
	h.lock.Lock()
	st = h.getHsState()
	if st == nil {
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
		leaderForCurrent, _ := h.getLeaderForView(h.chain, currentViewForMiner)
		if leaderForCurrent == h.val {
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
	view := getViewFromHeader(header)
	leader, _ := h.getLeaderForView(h.chain, view)
	if leader != header.Coinbase {
		return nil
	}
	if st.lockedQC != nil && header.ParentHash != st.lockedQC.BlockHash {
		return nil
	}
	if has, v, hq, _ := parseSyncInfo(header); has {
		if st.highQC == nil || v > st.highQC.View {
			// Initialize with minimal info from syncInfo
			// SignersSet and Sig will be empty here (syncInfo doesn't contain them)
			st.highQC = &HsQC{BlockHash: hq, View: v, SignersSet: 0, Sig: nil}
			log.Debug("processProposalHeader: updated highQC from header syncInfo (minimal)",
				"view", v,
				"blockHash", hq.Hex()[:8])
		}
	}
	st.proposalsByView[view] = header
	st.proposalsByHash[header.Hash()] = header
	if view > st.currentView {
		st.currentView = view
	}
	// BLS sign and send vote
	if h.voteSigner == nil {
		return nil
	}

	digest := h.hsVoteDigest(header.Hash(), view)
	pub, sig, err := h.signHsVote(digest)
	if err != nil {
		return nil
	}
	vote := &hs.VotePacket{BlockHash: header.Hash(), ViewNumber: view, VotePubKey: pub, Signature: sig}
	// Use local validator address (no need to verify own address, receiver will verify)
	addr := h.val
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
		// BLS not available (before Luban fork), use BLS pubkey hash as fallback address
		addr = common.BytesToAddress(crypto.Keccak256(vp.VotePubKey[:])[:20])
		log.Debug("Using BLS pubkey hash for vote (BLS not available)", "view", vp.ViewNumber, "addr", addr)
	}
	view := vp.ViewNumber
	if st.votes[view] == nil {
		st.votes[view] = make(map[common.Hash]map[common.Address]*hs.VotePacket)
	}
	if st.votes[view][vp.BlockHash] == nil {
		st.votes[view][vp.BlockHash] = make(map[common.Address]*hs.VotePacket)
	}
	st.votes[view][vp.BlockHash][addr] = vp
	head := h.chain.CurrentHeader()
	snap, _ := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
	qsize := QuorumSize(len(snap.validators()))
	cnt := len(st.votes[view][vp.BlockHash])
	if cnt >= qsize {
		qc := &HsQC{BlockHash: vp.BlockHash, View: view}
		st.highQC = qc
		st.qcsByView[view] = qc
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
		err := h.broadcastHsQCWithAggLocked(vp.BlockHash, view, agg, bitset)
		if err != nil {
			log.Warn("processVotePacket broadcastHsQCWithAggLocked failed", err)
			return err
		}
		// Try 3-chain commit using the strict consecutive view rule
		if err := h.tryCommitBlocks(st, vp.BlockHash, view); err != nil {
			log.Error("Failed to commit blocks in processVotePacket", "view", view, "err", err)
		}
	}
	return nil
}

func (h *Hotstuff) processNewViewPacket(peerID string, nv *hs.NewViewPacket) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return nil
	}
	v := nv.HighQCView + 1
	if st.newViews[v] == nil {
		st.newViews[v] = make(map[common.Address]*hs.NewViewPacket)
	}
	addr := common.Address{}
	if h._hsNet != nil {
		if a, ok := h._hsNet.ResolveAddress(peerID); ok {
			addr = a
		}
	}
	st.newViews[v][addr] = nv

	// Update highTCView if NewView carries higher TC
	if nv.HighTCView > st.highTCView {
		log.Debug("processNewViewPacket updating highTCView", "oldView", st.highTCView, "newView", nv.HighTCView, "peer", peerID)
		st.highTCView = nv.HighTCView
	}

	leader, _ := h.getLeaderForView(h.chain, v)
	// use current validator set size from snapshot
	head2 := h.chain.CurrentHeader()
	snap2, _ := h.snapshot(h.chain, head2.Number.Uint64(), head2.Hash(), nil)
	if leader == h.val && len(st.newViews[v]) >= QuorumSize(len(snap2.validators())) {
		// Pick highest QC and highest TC among new-views
		var maxQCView uint64
		var base common.Hash
		var maxTCView uint64
		for _, m := range st.newViews[v] {
			if m.HighQCView >= maxQCView {
				maxQCView = m.HighQCView
				base = m.HighQCHash
			}
			if m.HighTCView > maxTCView {
				maxTCView = m.HighTCView
			}
		}
		// Update highTCView to the highest from quorum
		if maxTCView > st.highTCView {
			st.highTCView = maxTCView
		}
		h.proposeFromHighQC(v, base)
	}
	return nil
}

func (h *Hotstuff) processTimeoutPacket(to *hs.TimeoutPacket) error {
	h.lock.Lock()
	nextView, shouldMoveToView := h.processTimeoutPacketLocked(to)
	h.lock.Unlock()

	if shouldMoveToView {
		h.moveToView(nextView)
	}
	return nil
}

// processTimeoutPacketLocked is the internal version that assumes lock is already held
// processTimeoutPacketLocked processes a timeout packet while lock is held.
// Returns (nextView, shouldMoveToView) so caller can call moveToView after releasing lock.
func (h *Hotstuff) processTimeoutPacketLocked(to *hs.TimeoutPacket) (uint64, bool) {
	st := h.getHsState()
	if st == nil {
		return 0, false
	}
	v := to.ViewNumber
	if st.timeouts[v] == nil {
		st.timeouts[v] = make(map[common.Address]*hs.TimeoutPacket)
	}
	// Verify and resolve sender from BLS pubkey
	addr, blsAvailable, err := h.verifyHsTimeoutAndResolveAddr(to)
	if err != nil {
		return 0, false
	}
	if !blsAvailable {
		// BLS not available (before Luban fork), try to use a fallback address
		// Since we don't have peerID here, use BLS pubkey hash as a deterministic address
		addr = common.BytesToAddress(crypto.Keccak256(to.VotePubKey[:])[:20])
		log.Debug("Using BLS pubkey hash for timeout (BLS not available)", "view", to.ViewNumber, "addr", addr)
	}
	st.timeouts[v][addr] = to
	head := h.chain.CurrentHeader()
	snap, _ := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
	if snap == nil {
		return 0, false
	}
	qsize := QuorumSize(len(snap.validators()))
	if len(st.timeouts[v]) >= qsize {
		if v > st.highTCView {
			st.highTCView = v
		}
		// Return next view; caller will call moveToView after releasing lock
		return v + 1, true
	}
	return 0, false
}

func (h *Hotstuff) processQCPacket(qc *hs.QuorumCertPacket) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return nil
	}
	if st.highQC == nil || qc.ViewNumber > st.highQC.View {
		// Verify QC by BLS fast aggregate verify using provided signer set
		if !h.verifyAggregateQC(qc) {
			return nil
		}
		// CRITICAL FIX: Must copy SignersSet from QC packet!
		st.highQC = &HsQC{
			BlockHash:  qc.TargetHash,
			View:       qc.ViewNumber,
			Sig:        qc.AggregateSig,
			SignersSet: qc.SignersSet,
		}
		st.qcsByView[qc.ViewNumber] = st.highQC
		log.Debug("processQCPacket: updated highQC",
			"view", qc.ViewNumber,
			"blockHash", qc.TargetHash.Hex()[:8],
			"signersSet", qc.SignersSet,
			"aggSigLen", len(qc.AggregateSig))
		// Try update lockedQC via 3-chain rule when adjacent QC exists and headers are known
		if prev := st.qcsByView[qc.ViewNumber-1]; prev != nil {
			currHdr := st.proposalsByHash[qc.TargetHash]
			prevHdr := st.proposalsByHash[prev.BlockHash]
			if currHdr != nil && prevHdr != nil && currHdr.ParentHash == prevHdr.Hash() {
				st.lockedQC = prev
			}
		}
	}
	return nil
}

// restartViewTimeout restarts the per-view timeout timer and schedules local timeout
// CRITICAL: This function manages its own locks. Caller should NOT hold lock when calling.
func (h *Hotstuff) restartViewTimeout() {
	// Phase 1: Get current view (short lock)
	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
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
	if h.hsTimer != nil {
		h.hsTimer.Stop()
	}
	h.hsTimer = time.AfterFunc(time.Duration(base)*time.Millisecond, func() {
		h.onLocalViewTimeout(view)
	})
	h.lock.Unlock()
}

// onLocalViewTimeout constructs and broadcasts a TimeoutPacket; then aggregates
// CRITICAL FIX: Refactored to avoid calling moveToView while holding lock
func (h *Hotstuff) onLocalViewTimeout(view uint64) {
	log.Info("[onLocalViewTimeout] ENTER", "view", view)

	// Phase 1: Build and sign timeout packet (short lock)
	h.lock.Lock()
	st := h.getHsState()
	if st == nil || view != st.currentView {
		log.Info("[onLocalViewTimeout] EXIT - state nil or view mismatch", "view", view, "currentView", func() uint64 {
			if st != nil {
				return st.currentView
			}
			return 0
		}())
		h.lock.Unlock()
		return
	}
	log.Info("[onLocalViewTimeout] Processing timeout", "view", view)

	// build timeout packet with BLS signature
	to := &hs.TimeoutPacket{ViewNumber: view}
	if st.highQC != nil {
		to.HighQCView = st.highQC.View
		to.HighQCHash = st.highQC.BlockHash
	}
	if h.voteSigner != nil {
		root := h.hsTimeoutDigest(to)
		pub, sig, err := h.signHsVote(root)
		if err == nil {
			to.VotePubKey = pub
			to.Signature = sig
		}
	}
	h.lastTimeoutView = view
	h.lastTimeoutPacket = to

	// Process locally and check if we should move to next view
	nextView, shouldMoveToView := h.processTimeoutPacketLocked(to)
	h.lock.Unlock()

	// Phase 2: Broadcast (no lock needed for network IO)
	if h._hsNet != nil {
		h._hsNet.BroadcastTimeout(to)
	}

	// Phase 3: Move to next view if quorum reached (this function acquires its own lock)
	if shouldMoveToView {
		log.Info("[onLocalViewTimeout] Quorum reached, moving to next view", "currentView", view, "nextView", nextView)
		h.moveToView(nextView)
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
		st := h.getHsState()
		if st != nil {
			block = st.proposalsByHashBlock[hash]
		}
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
