package eth

import (
	"fmt"

	"github.com/ethereum/go-ethereum/consensus/hotstuff"
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
	eng, ok := h.chain.Engine().(*hotstuff.Hotstuff)
	if !ok {
		return fmt.Errorf("hotstuff engine not enabled")
	}
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
