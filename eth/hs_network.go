package eth

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/hotstuff"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
)

// hsNetworkAdapter bridges Hotstuff engine network calls to connected hs peers
type hsNetworkAdapter struct {
	h *handler
}

func newHsNetworkAdapter(h *handler) *hsNetworkAdapter { return &hsNetworkAdapter{h: h} }

// BroadcastProposal sends a proposal to all hs-capable peers
func (a *hsNetworkAdapter) BroadcastProposal(prop *hs.ProposalPacket) error {
	peers := a.h.peers.headPeers(uint(a.h.peers.len()))
	sent := 0
	for _, p := range peers {
		if p.hsExt != nil {
			err := p.hsExt.SendProposal(prop)
			if err != nil {
				log.Warn("Failed to broadcast proposal", "err", err, "peer", p.ID())
			}
			sent++
		}
	}
	log.Debug("BroadcastProposal sent to peers", "sent", sent)
	if sent == 0 {
		return errors.New("no hs-capable peers to broadcast proposal")
	}
	return nil
}

// SendVoteToLeader sends a vote to leader; fallback to broadcast if no direct match
func (a *hsNetworkAdapter) SendVoteToLeader(leader common.Address, vote *hs.VotePacket) error {
	// Check if leader is self - if so, handle locally without network
	if a.isSelfValidator(leader) {
		log.Debug("SendVoteToLeader: leader is self, processing locally", "leader", leader.Hex())
		return a.sendVoteToSelf(vote)
	}

	// try to find a connected hs peer belonging to the leader's node IDs
	if ext := a.findHsPeerByValidator(leader); ext != nil {
		err := ext.SendVote(vote)
		if err != nil {
			log.Warn("Failed to send vote to leader", "err", err, "leader", leader.Hex())
			// Don't return error, fallthrough to broadcast
		} else {
			log.Debug("SendVoteToLeader successfully", "leader", leader.Hex())
			return nil
		}
	}

	// Fallback: broadcast to all hs-capable peers
	// This is necessary when validatorNodeIDsMap is not yet initialized
	// (e.g., before validators register their node IDs in the contract)
	log.Debug("SendVoteToLeader using broadcast fallback", "leader", leader.Hex())
	peers := a.h.peers.headPeers(uint(a.h.peers.len()))
	sent := 0
	for _, p := range peers {
		if p.hsExt != nil {
			err := p.hsExt.SendVote(vote)
			if err != nil {
				log.Debug("Failed to broadcast vote to peer", "err", err, "peer", p.ID())
			} else {
				sent++
			}
		}
	}
	if sent == 0 {
		log.Warn("SendVoteToLeader fallback failed: no hs-capable peers")
		return errors.New("no hs-capable peers to send vote")
	}
	log.Debug("SendVoteToLeader fallback broadcast", "sent", sent)
	return nil
}

// isSelfValidator checks if the given validator address is the current node's consensus address
func (a *hsNetworkAdapter) isSelfValidator(validator common.Address) bool {
	// Try HotStuff engine
	if hs, ok := a.h.chain.Engine().(interface{ ConsensusAddress() common.Address }); ok {
		selfAddr := hs.ConsensusAddress()
		if selfAddr == validator {
			return true
		}
	}
	return false
}

// sendVoteToSelf processes vote locally when the leader is self
func (a *hsNetworkAdapter) sendVoteToSelf(vote *hs.VotePacket) error {
	// Get HotStuff engine
	engine := a.h.chain.Engine()
	hs, ok := engine.(interface {
		OnHsVote(string, interface{}) error
	})
	if !ok {
		return errors.New("engine does not support OnHsVote")
	}

	// CRITICAL FIX: Process vote asynchronously to avoid deadlock
	// The caller (OnHsProposal Phase 4) holds h.lock, but OnHsVote also needs h.lock
	// Running in a new goroutine allows OnHsProposal to release its lock first
	log.Debug("sendVoteToSelf: scheduling async processing", "view", vote.ViewNumber, "blockHash", vote.BlockHash.Hex()[:8])

	go func() {
		err := hs.OnHsVote("self", vote)
		if err != nil {
			log.Warn("Failed to process self vote", "err", err, "view", vote.ViewNumber)
		} else {
			log.Debug("Successfully processed self vote locally", "view", vote.ViewNumber, "blockHash", vote.BlockHash.Hex()[:8])
		}
	}()

	return nil
}

// BroadcastNewView broadcasts NewView to all hs-capable peers
func (a *hsNetworkAdapter) BroadcastNewView(nv *hs.NewViewPacket) int {
	peers := a.h.peers.headPeers(uint(a.h.peers.len()))
	sent := 0
	for _, p := range peers {
		if p.hsExt != nil {
			err := p.hsExt.SendNewView(nv)
			if err != nil {
				log.Warn("Failed to broadcast NewView", "err", err, "peer", p.ID())
			} else {
				sent++
			}
		}
	}
	log.Debug("BroadcastNewView sent to peers", "sent", sent)
	return sent
}

// BroadcastQC broadcasts QC to all hs-capable peers
func (a *hsNetworkAdapter) BroadcastQC(qc *hs.QuorumCertPacket) int {
	peers := a.h.peers.headPeers(uint(a.h.peers.len()))
	sent := 0
	for _, p := range peers {
		if p.hsExt != nil {
			err := p.hsExt.SendQC(qc)
			if err != nil {
				log.Warn("Failed to broadcast QC", "err", err, "peer", p.ID())
			} else {
				sent++
			}
		}
	}
	log.Debug("BroadcastQC sent to peers", "sent", sent)
	return sent
}

// BroadcastTimeout broadcasts Timeout to all hs-capable peers
func (a *hsNetworkAdapter) BroadcastTimeout(to *hs.TimeoutPacket) int {
	peers := a.h.peers.headPeers(uint(a.h.peers.len()))
	sent := 0
	for _, p := range peers {
		if p.hsExt != nil {
			err := p.hsExt.SendTimeout(to)
			if err != nil {
				log.Warn("Failed to broadcast Timeout", "err", err, "peer", p.ID())
			} else {
				sent++
			}
		}
	}
	log.Debug("BroadcastTimeout sent to peers", "sent", sent)
	return sent
}

// ResolveAddress resolves peerID to consensus address if known using peerset's validatorNodeIDsMap
func (a *hsNetworkAdapter) ResolveAddress(peerID string) (common.Address, bool) {
	peer := a.h.peers.peer(peerID)
	if peer == nil || peer.Node() == nil {
		return common.Address{}, false
	}
	nodeID := peer.Node().ID()
	a.h.peers.lock.RLock()
	defer a.h.peers.lock.RUnlock()
	for addr, ids := range a.h.peers.validatorNodeIDsMap {
		for _, id := range ids {
			if id == nodeID {
				return addr, true
			}
		}
	}
	return common.Address{}, false
}

// findHsPeerByValidator tries to pick any connected hs peer that matches the validator's node IDs
func (a *hsNetworkAdapter) findHsPeerByValidator(validator common.Address) *hs.Peer {
	a.h.peers.lock.RLock()
	ids := a.h.peers.validatorNodeIDsMap[validator]
	mapSize := len(a.h.peers.validatorNodeIDsMap)

	// Log all validators in the map for debugging
	if mapSize > 0 && len(ids) == 0 {
		log.Debug("findHsPeerByValidator: validatorNodeIDsMap content:")
		for addr, nodeIDs := range a.h.peers.validatorNodeIDsMap {
			log.Debug("  validator entry", "addr", addr.Hex(), "nodeIDsCount", len(nodeIDs))
		}
	}

	// copy peers snapshot to avoid holding lock during sends
	peers := make([]*ethPeer, 0, len(a.h.peers.peers))
	for _, p := range a.h.peers.peers {
		peers = append(peers, p)
	}
	a.h.peers.lock.RUnlock()

	log.Debug("findHsPeerByValidator", "validator", validator.Hex(), "ids", len(ids), "mapSize", mapSize, "totalPeers", len(peers))

	if mapSize == 0 {
		log.Debug("findHsPeerByValidator: validatorNodeIDsMap is empty, may not be initialized yet")
		return nil
	}

	if len(ids) == 0 {
		log.Debug("findHsPeerByValidator: no node IDs found for validator, node ID may not be registered in StakeHub contract", "validator", validator.Hex())
		return nil
	}

	// Log all node IDs for this validator
	for i, id := range ids {
		log.Debug("findHsPeerByValidator: validator node ID", "validator", validator.Hex(), "index", i, "nodeID", id)
	}

	hsPeersCount := 0
	for _, p := range peers {
		if p == nil || p.Node() == nil {
			continue
		}
		if p.hsExt == nil {
			continue
		}
		hsPeersCount++
		nid := p.Node().ID()
		for i, id := range ids {
			if id == nid {
				log.Debug("findHsPeerByValidator: found matching peer", "validator", validator.Hex(), "nodeID", id, "peerID", p.ID())
				return p.hsExt.Peer
			}
			if i == 0 { // Log first comparison to avoid spam
				log.Debug("findHsPeerByValidator: comparing", "peerNodeID", nid, "validatorNodeID", id, "match", id == nid)
			}
		}
	}

	log.Debug("findHsPeerByValidator: no matching peer found", "validator", validator.Hex(), "hsPeersCount", hsPeersCount, "validatorNodeIDsCount", len(ids))
	return nil
}

// ensure hsNetworkAdapter implements engine interface at compile-time (doc only)
var _ hotstuff.HsNetwork = (*hsNetworkAdapter)(nil)
