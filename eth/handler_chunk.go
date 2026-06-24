package eth

import (
	"encoding/binary"
	"math"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/log"
)

// evnChunkPeers returns the subset of the given peers that (a) have the EVN
// flag set and (b) support the Bsc3 chunk protocol.  The result is sorted
// deterministically using a random seed derived from the block hash so that
// different leaders produce the same ordering (making the fanout assignment
// stable across runs).
func (h *handler) evnChunkPeers(peers []*ethPeer, blockHash common.Hash) []*ethPeer {
	var out []*ethPeer
	for _, p := range peers {
		if !p.EVNPeerFlag.Load() {
			continue
		}
		if p.bscExt == nil || p.bscExt.Version() < bsc.Bsc3 {
			continue
		}
		out = append(out, p)
	}
	if len(out) <= 1 {
		return out
	}
	// Deterministic ordering using a seed from the block hash.  This avoids
	// always favouring the same peers for the first chunk range.
	var seed uint64
	for i := 0; i+8 <= len(blockHash); i += 8 {
		seed ^= binary.BigEndian.Uint64(blockHash[i : i+8])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hashSeed(out[i].ID(), seed) < hashSeed(out[j].ID(), seed)
	})
	return out
}

// hashSeed computes a deterministic pseudo-random uint64 from a peer ID and a
// seed.  It is used only for fanout assignment ordering and is not
// cryptographically relevant.
func hashSeed(id string, seed uint64) uint64 {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], seed)
	h := common.BytesToHash(append([]byte(id), buf[:]...))
	return binary.BigEndian.Uint64(h[:8])
}

// distributeBlockChunks fans out block chunks to EVN peers using a two-level
// tree.  The leader sends different chunk subsets to `ceil(sqrt(N))` first-
// level peers; each first-level peer is responsible for relaying to a small
// subset of second-level peers.  In the MVP, each chunk is sent redundantly to
// enough first-level peers so that any single peer failure still allows
// reconstruction via the missing-chunk request mechanism.
//
// The redundancy factor defaults to 2: each chunk is sent to `fanout` peers
// picked round-robin from the sorted peer list.  When there are fewer peers
// than chunks, every peer receives the full set.
func (h *handler) distributeBlockChunks(peers []*ethPeer, pkts []*bsc.BlockChunkPacket) {
	evnPeers := h.evnChunkPeers(peers, pkts[0].BlockHash)
	if len(evnPeers) == 0 {
		// No EVN peers supporting Bsc3: the caller should fall back to the
		// legacy full-block path, but as a safety net we send to any Bsc3
		// peer we can find.
		for _, p := range peers {
			if p.bscExt != nil && p.bscExt.Version() >= bsc.Bsc3 {
				evnPeers = append(evnPeers, p)
			}
		}
		if len(evnPeers) == 0 {
			log.Warn("chunk path enabled but no Bsc3 peers available", "hash", pkts[0].BlockHash)
			return
		}
	}

	n := len(evnPeers)
	fanout := int(math.Ceil(math.Sqrt(float64(n))))
	if fanout < 1 {
		fanout = 1
	}
	if fanout > n {
		fanout = n
	}
	redundancy := 2
	if redundancy > n {
		redundancy = n
	}

	log.Debug("Distributing block chunks", "hash", pkts[0].BlockHash, "chunks", len(pkts), "evnPeers", n, "fanout", fanout, "redundancy", redundancy)

	for i, pkt := range pkts {
		// Send each chunk to `redundancy` peers chosen round-robin from the
		// fanout subset.  Starting offset is based on chunk index so that
		// different chunks go to different first-level peers.
		for r := 0; r < redundancy; r++ {
			peerIdx := (i*redundancy + r) % fanout
			evnPeers[peerIdx].bscExt.AsyncSendBlockChunk(pkt)
		}
	}
}

// relayBlockChunk is called from the bsc backend Handle path when a chunk is
// received.  It forwards the chunk to a small subset of the node's own EVN
// peers (the second level of the fanout tree).  We avoid sending back to the
// peer that sent us the chunk.
func (h *handler) relayBlockChunk(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket) {
	if h.chunkPool == nil {
		return
	}
	// Collect EVN peers (excluding the sender) that support Bsc3.
	var candidates []*ethPeer
	for _, p := range h.peers.peersWithoutBlock(pkt.BlockHash) {
		if !p.EVNPeerFlag.Load() {
			continue
		}
		if p.bscExt == nil || p.bscExt.Version() < bsc.Bsc3 {
			continue
		}
		if p.bscExt.ID() == fromPeer.ID() {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return
	}
	// Forward to a small subset (sqrt of candidates) to limit amplification.
	count := int(math.Ceil(math.Sqrt(float64(len(candidates)))))
	if count > len(candidates) {
		count = len(candidates)
	}
	// Use a deterministic selection based on the chunk index so that
	// different chunks are relayed to different peers.
	start := int(pkt.ChunkIndex) % len(candidates)
	for i := 0; i < count; i++ {
		candidates[(start+i)%len(candidates)].bscExt.AsyncSendBlockChunk(pkt)
	}
}
