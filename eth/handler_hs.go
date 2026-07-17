package eth

import (
	"fmt"

	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// hsHandler implements the hs.Backend interface to bridge p2p messages to the HotStuff engine.
type hsHandler handler

// RunPeer is invoked when a peer joins on the `hs` protocol.
func (h *hsHandler) RunPeer(peer *hs.Peer, hand func(*hs.Peer) error) error {
	// No custom handshake yet beyond devp2p; proceed to run loop
	return (*handler)(h).runHsExtension(peer, hand)
}

// PeerInfo retrieves protocol-specific info about a peer.
func (h *hsHandler) PeerInfo(id enode.ID) interface{} {
	if p := h.peers.peer(id.String()); p != nil && p.hsExt != nil {
		return p.hsExt.info()
	}
	return nil
}

// Handle routes decoded packets to the HotStuff engine.
func (h *hsHandler) Handle(peer *hs.Peer, packet hs.Packet) error {
	if eng, ok := h.chain.Engine().(interface {
		OnHsProposal(string, *hs.ProposalPacket) error
		OnHsVote(string, *hs.VotePacket) error
		OnHsNewView(string, *hs.NewViewPacket) error
		OnHsTimeout(string, *hs.TimeoutPacket) error
		OnHsQuorumCert(string, *hs.QuorumCertPacket) error
	}); ok {
		switch pkt := packet.(type) {
		case *hs.ProposalPacket:
			return eng.OnHsProposal(peer.ID(), pkt)
		case *hs.VotePacket:
			return eng.OnHsVote(peer.ID(), pkt)
		case *hs.NewViewPacket:
			return eng.OnHsNewView(peer.ID(), pkt)
		case *hs.TimeoutPacket:
			return eng.OnHsTimeout(peer.ID(), pkt)
		case *hs.QuorumCertPacket:
			return eng.OnHsQuorumCert(peer.ID(), pkt)
		default:
			return fmt.Errorf("unexpected hs packet type: %T", packet)
		}
	}
	// Standalone HotStuff currently exposes generic packet arguments, while the
	// transition engine exposes strongly typed methods. Support both until the
	// engine interface is unified.
	if eng, ok := h.chain.Engine().(interface {
		OnHsProposal(string, interface{}) error
		OnHsVote(string, interface{}) error
		OnHsNewView(string, interface{}) error
		OnHsTimeout(string, interface{}) error
		OnHsQuorumCert(string, interface{}) error
	}); ok {
		switch packet.(type) {
		case *hs.ProposalPacket:
			return eng.OnHsProposal(peer.ID(), packet)
		case *hs.VotePacket:
			return eng.OnHsVote(peer.ID(), packet)
		case *hs.NewViewPacket:
			return eng.OnHsNewView(peer.ID(), packet)
		case *hs.TimeoutPacket:
			return eng.OnHsTimeout(peer.ID(), packet)
		case *hs.QuorumCertPacket:
			return eng.OnHsQuorumCert(peer.ID(), packet)
		default:
			return fmt.Errorf("unexpected hs packet type: %T", packet)
		}
	}
	return fmt.Errorf("hotstuff protocol not enabled")
}
