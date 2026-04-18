package hs

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

// Backend defines the consensus backend hooks the `hs` protocol calls into.
type Backend interface {
	// RunPeer is invoked when a peer joins on the `hs` protocol.
	RunPeer(peer *Peer, handler func(*Peer) error) error

	// PeerInfo retrieves protocol-specific info to be exposed over the API.
	PeerInfo(id enode.ID) interface{}

	// Handle is invoked when a decoded packet is received from remote.
	Handle(peer *Peer, packet Packet) error
}

// MakeProtocols constructs the p2p protocol definitions for `hs`.
func MakeProtocols(backend Backend) []p2p.Protocol {
	protocols := make([]p2p.Protocol, len(ProtocolVersions))
	for i, version := range ProtocolVersions {
		ver := version
		protocols[i] = p2p.Protocol{
			Name:    ProtocolName,
			Version: ver,
			Length:  protocolLengths[ver],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				peer := NewPeer(ver, p, rw)
				defer peer.Close()
				return backend.RunPeer(peer, func(peer *Peer) error {
					return Handle(backend, peer)
				})
			},
			NodeInfo: func() interface{} {
				// no chain-specific node info for hs
				return nil
			},
			PeerInfo:   func(id enode.ID) interface{} { return backend.PeerInfo(id) },
			Attributes: []enr.Entry{},
		}
	}
	return protocols
}

// Handle drives the lifecycle of an `hs` peer, dispatching inbound messages.
func Handle(backend Backend, peer *Peer) error {
	for {
		if err := handleMessage(backend, peer); err != nil {
			peer.Log().Debug("hs message handling failed", "err", err)
			return err
		}
	}
}

type Decoder interface{ Decode(val interface{}) error }

type msgHandler func(backend Backend, msg Decoder, peer *Peer) error

var hs1 = map[uint64]msgHandler{
	ProposalMsg: handleProposal,
	VoteMsg:     handleVote,
	NewViewMsg:  handleNewView,
	TimeoutMsg:  handleTimeout,
	QCMsg:       handleQC,
}

func handleMessage(backend Backend, peer *Peer) error {
	msg, err := peer.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > maxMessageSize {
		return fmt.Errorf("%w: %v > %v", errMsgTooLarge, msg.Size, maxMessageSize)
	}
	defer msg.Discard()

	handlers := hs1

	if metrics.Enabled() {
		h := fmt.Sprintf("%s/%s/%d/%#02x", p2p.HandleHistName, ProtocolName, peer.Version(), msg.Code)
		defer func(start time.Time) {
			sampler := func() metrics.Sample { return metrics.ResettingSample(metrics.NewExpDecaySample(1028, 0.015)) }
			metrics.GetOrRegisterHistogramLazy(h, nil, sampler).Update(time.Since(start).Microseconds())
		}(time.Now())
	}
	if handler := handlers[msg.Code]; handler != nil {
		return handler(backend, msg, peer)
	}
	return fmt.Errorf("%w: %v", errInvalidMsgCode, msg.Code)
}

func handleProposal(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(ProposalPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, pkt)
}
func handleVote(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(VotePacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	peer.markVotes([]*VotePacket{pkt})
	return backend.Handle(peer, pkt)
}
func handleNewView(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(NewViewPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, pkt)
}
func handleTimeout(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(TimeoutPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, pkt)
}
func handleQC(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(QuorumCertPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	return backend.Handle(peer, pkt)
}
