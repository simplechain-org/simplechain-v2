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
	network := h.hsNetwork()
	if network == nil {
		return errors.New("hs network not set")
	}
	return network.BroadcastProposal(prop)
}

// sendHsVote sends a vote to the leader of the current view
func (h *Hotstuff) sendHsVote(to common.Address, vote *hs.VotePacket) {
	if network := h.hsNetwork(); network != nil {
		network.SendVoteToLeader(to, vote)
	}
}

// broadcastHsNewView broadcasts a NewView message to all validators
func (h *Hotstuff) broadcastHsNewView(nv *hs.NewViewPacket) {
	if network := h.hsNetwork(); network != nil {
		network.BroadcastNewView(nv)
	}
}

// broadcastHsQC broadcasts a QuorumCert to all validators
func (h *Hotstuff) broadcastHsQC(qc *hs.QuorumCertPacket) {
	if network := h.hsNetwork(); network != nil {
		network.BroadcastQC(qc)
	}
}

// broadcastHsQCWithAggLocked constructs and broadcasts a QuorumCert with aggregated signature
// Assumes caller already holds h.lock (read or write)
// Uses the provided bitset instead of computing it (to avoid calling snapshot under lock)
func (h *Hotstuff) broadcastHsQCWithAggLocked(hash common.Hash, view uint64, agg []byte, bitset types.ValidatorsBitSet) error {
	log.Debug("broadcastHsQCWithAggLocked", "hash", hash, "view", view, "agg", fmt.Sprintf("%x", agg), "bitset", bitset)
	network := h.hsNetwork()
	if network == nil || len(agg) == 0 {
		log.Debug("broadcastHsQCWithAggLocked hs network not set or agg is empty")
		return errors.New("hs network not set or agg is empty")
	}
	sent := network.BroadcastQC(&hs.QuorumCertPacket{
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
func (h *Hotstuff) broadcastHsQCWithAgg(hash common.Hash, view uint64, agg []byte, bitset types.ValidatorsBitSet) error {
	// The caller formed and verified this exact proof against the target
	// proposal's validator snapshot. Recomputing the bitset at the current head
	// would change its meaning across validator-set transitions.
	return h.broadcastHsQCWithAggLocked(hash, view, agg, bitset)
}
