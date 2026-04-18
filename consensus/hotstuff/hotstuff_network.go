package hotstuff

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
)

// broadcastHsProposal broadcasts a HotStuff proposal to all validators
func (h *Hotstuff) broadcastHsProposal(prop *hs.ProposalPacket) error {
	if h._hsNet == nil {
		return errors.New("hs network not set")
	}
	return h._hsNet.BroadcastProposal(prop)
}

// sendHsVote sends a vote to the leader of the current view
func (h *Hotstuff) sendHsVote(to common.Address, vote *hs.VotePacket) {
	if h._hsNet != nil {
		h._hsNet.SendVoteToLeader(to, vote)
	}
}

// broadcastHsNewView broadcasts a NewView message to all validators
func (h *Hotstuff) broadcastHsNewView(nv *hs.NewViewPacket) {
	if h._hsNet != nil {
		h._hsNet.BroadcastNewView(nv)
	}
}

// broadcastHsQC broadcasts a QuorumCert to all validators
func (h *Hotstuff) broadcastHsQC(qc *hs.QuorumCertPacket) {
	if h._hsNet != nil {
		h._hsNet.BroadcastQC(qc)
	}
}

// broadcastHsQCWithAggLocked constructs and broadcasts a QuorumCert with aggregated signature
// Assumes caller already holds h.lock (read or write)
// Uses the provided bitset instead of computing it (to avoid calling snapshot under lock)
func (h *Hotstuff) broadcastHsQCWithAggLocked(hash common.Hash, view uint64, agg []byte, bitset types.ValidatorsBitSet) error {
	log.Debug("broadcastHsQCWithAggLocked", "hash", hash, "view", view, "agg", fmt.Sprintf("%x", agg), "bitset", bitset)
	if h._hsNet == nil || len(agg) == 0 {
		log.Debug("broadcastHsQCWithAggLocked hs network not set or agg is empty")
		return errors.New("hs network not set or agg is empty")
	}
	sent := h._hsNet.BroadcastQC(&hs.QuorumCertPacket{
		TargetHash:   hash,
		ViewNumber:   view,
		SignersSet:   bitset,
		AggregateSig: agg,
	})
	log.Debug("broadcastHsQCWithAggLocked", "sent", sent)
	if sent == 0 {
		return errors.New("no validators to broadcast to")
	}
	return nil
}

// broadcastHsQCWithAgg constructs and broadcasts a QuorumCert with aggregated signature
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations.
func (h *Hotstuff) broadcastHsQCWithAgg(hash common.Hash, view uint64, agg []byte) error {
	log.Debug("broadcastHsQCWithAgg", "hash", hash, "view", view)
	if h._hsNet == nil || len(agg) == 0 {
		log.Debug("broadcastHsQCWithAgg hs network not set or agg is empty")
		return errors.New("hs network not set or agg is empty")
	}

	// Phase 1: Get voters from state (short lock)
	h.lock.RLock()
	st := h.getHsState()
	var votersCopy map[common.Address]*hs.VotePacket
	if st != nil {
		if voters, ok := st.votes[view][hash]; ok && len(voters) > 0 {
			votersCopy = make(map[common.Address]*hs.VotePacket, len(voters))
			for k, v := range voters {
				votersCopy[k] = v
			}
		}
	}
	h.lock.RUnlock()

	// Phase 2: Get snapshot and compute bitset (no lock - may block)
	var bitset types.ValidatorsBitSet
	if votersCopy != nil {
		head := h.chain.CurrentHeader()
		if head != nil {
			snap, _ := h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
			if snap != nil {
				validators := snap.validators()
				for idx, addr := range validators {
					if _, ok := votersCopy[addr]; ok {
						bitset |= types.ValidatorsBitSet(1) << uint(idx)
					}
				}
			}
		}
	}

	// Phase 3: Broadcast (no lock needed for network IO)
	sent := h._hsNet.BroadcastQC(&hs.QuorumCertPacket{
		TargetHash:   hash,
		ViewNumber:   view,
		SignersSet:   bitset,
		AggregateSig: agg,
	})
	log.Debug("broadcastHsQCWithAgg", "sent", sent)
	if sent == 0 {
		return errors.New("no validators to broadcast to")
	}
	return nil
}
