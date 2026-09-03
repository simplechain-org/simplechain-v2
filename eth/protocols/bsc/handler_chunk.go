package bsc

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// handleBlockChunk handles authenticated Bsc4 shards. Ingress does not depend
// on the asynchronously refreshed EVNPeerFlag: the producer manifest and the
// depth-zero source binding provide stable authentication, while every Bsc4
// connection is subject to the same wire-rate and pool budgets.
func handleBlockChunk(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(BlockChunkPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	if pkt.ShardRoot == (common.Hash{}) || pkt.RelayDepth > 1 || len(pkt.RelayTargets) > MaxBlockChunkRelayTargets {
		return nil
	}
	pool := backend.ChunkPool()
	if pool == nil || peer.Peer == nil {
		return nil
	}
	sourceIsOrigin := pkt.OriginNodeID == peer.Peer.ID()
	// Depth-zero traffic is leader-to-relay traffic. The manifest signature
	// binds OriginNodeID to the leader's devp2p key, so a relay cannot rewrite
	// the route and re-amplify a packet as depth zero.
	if pkt.RelayDepth == 0 && !sourceIsOrigin {
		return nil
	}
	// A depth-one relay may only contribute to a root already established by
	// a direct producer seed. Relay traffic can race ahead and be dropped; the
	// seed's repair path then recovers any missing early shards.
	if pkt.RelayDepth == 1 && !sourceIsOrigin && !pool.HasEncoding(pkt.BlockHash, pkt.ShardRoot) {
		return nil
	}
	// Invalid shards are not eligible for any backend work. Valid duplicates
	// are observed as additional repair sources, but are not relayed again.
	switch pool.addChunkStatus(pkt, peer.ID()) {
	case chunkRejected:
		return nil
	case chunkDuplicate:
		if observer, ok := backend.(ChunkObserver); ok {
			observer.ObserveBlockChunk(peer, pkt)
		}
		return nil
	}
	return backend.Handle(peer, pkt)
}

// Bsc3 chunk packets were never authenticated and are no longer accepted as
// data. The message is discarded without decoding its potentially large body;
// peers can still use Bsc2 range requests and the regular eth protocol.
func handleBlockChunkV3(backend Backend, msg Decoder, peer *Peer) error {
	return nil
}

func handleGetBlockChunks(backend Backend, msg Decoder, peer *Peer) error {
	// EVNPeerFlag is refreshed asynchronously and is not an authorization source.
	// The pool instead binds repair access to the producer-selected route (or the
	// producer's explicit recipient set) and applies a node-wide egress budget in
	// addition to this connection's request rate limit.
	req := new(GetBlockChunksPacket)
	if err := msg.Decode(req); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	if req.BlockHash == (common.Hash{}) || req.ShardRoot == (common.Hash{}) ||
		len(req.MissingIndexes) == 0 || len(req.MissingIndexes) > MaxBlockChunkRequestIndexes {
		return fmt.Errorf("%w: invalid chunk repair request", errDecode)
	}
	if !peer.allowChunkRepairRequest(len(req.MissingIndexes)) {
		return nil
	}
	pool := backend.ChunkPool()
	if pool == nil {
		return nil
	}
	for _, pkt := range pool.GetChunks(req.BlockHash, req.ShardRoot, req.MissingIndexes, peer.ID()) {
		if !peer.AsyncSendBlockChunkForce(pkt) {
			// GetChunks reserves the node-wide egress budget before returning a
			// packet. A full or closed async queue did not accept that packet, so
			// do not let one slow authorized peer consume capacity for traffic
			// that can never leave this node.
			pool.refundRepairEgress(pkt)
		}
	}
	return nil
}

// Bsc3 repair requests are discarded for the same reason as legacy shards.
func handleGetBlockChunksV3(backend Backend, msg Decoder, peer *Peer) error {
	return nil
}
