package bsc

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// handleBlockChunk ingests an incoming Bsc3 block chunk.  The chunk is fed to
// the shared ChunkPool which reassembles the block once all chunks arrive.
func handleBlockChunk(backend Backend, msg Decoder, peer *Peer) error {
	pkt := new(BlockChunkPacket)
	if err := msg.Decode(pkt); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	pool := backend.ChunkPool()
	if pool == nil {
		return nil // peer supports Bsc3 but local node does not use chunk path
	}
	// Remember the chunk for this peer to avoid forwarding it back.
	peer.knownChunks.add(chunkKey{hash: pkt.BlockHash, index: pkt.ChunkIndex})
	pool.AddChunk(pkt)
	// Forward the chunk to the backend so it can relay it further down the
	// fanout tree (best-effort).
	return backend.Handle(peer, pkt)
}

// handleGetBlockChunks serves a request for missing chunks from a remote peer.
// If the local node has the full block (and therefore can reconstruct the
// chunks), it replies by re-splitting the block and sending the requested
// chunk indexes back.
func handleGetBlockChunks(backend Backend, msg Decoder, peer *Peer) error {
	req := new(GetBlockChunksPacket)
	if err := msg.Decode(req); err != nil {
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}
	if req.BlockHash == (common.Hash{}) {
		return nil
	}
	// Look up the local block.  If we don't have it, there's nothing we can do.
	block := backend.Chain().GetBlockByHash(req.BlockHash)
	if block == nil {
		return nil
	}
	// Re-split the block to recover the original chunks.  Use a config that
	// always splits (threshold=0 means "always chunk") so that the indexes
	// line up with what the requester expects.
	pkts, err := SplitBlock(block, ChunkConfig{Enable: true, Threshold: 0})
	if err != nil {
		return err
	}
	if pkts == nil {
		return nil
	}
	want := make(map[uint]struct{}, len(req.MissingIndexes))
	for _, idx := range req.MissingIndexes {
		want[idx] = struct{}{}
	}
	for _, pkt := range pkts {
		if _, ok := want[pkt.ChunkIndex]; !ok {
			continue
		}
		peer.AsyncSendBlockChunk(pkt)
	}
	return nil
}
