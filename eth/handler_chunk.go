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

// distributeBlockChunks fans out block shards to EVN peers using a deterministic
// two-level tree. The leader sends every shard to the first-level peers in a
// round-robin pattern with redundancy. Each first-level peer is then responsible
// for relaying shards to its deterministic child group.
func (h *handler) distributeBlockChunks(peers []*ethPeer, pkts []*bsc.BlockChunkPacket) bool {
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
			return false
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
	firstLevel := evnPeers[:fanout]

	bsc.BlockChunkFanoutGauge.Update(int64(fanout))
	bsc.BlockChunkPeerGauge.Update(int64(n))
	log.Debug("Distributing block chunks", "hash", pkts[0].BlockHash, "shards", len(pkts), "evnPeers", n, "fanout", fanout, "redundancy", redundancy)

	for i, pkt := range pkts {
		// Send each shard to first-level peers. Redundancy over RS parity keeps
		// the leader path robust without full-block EVN broadcast.
		for r := 0; r < redundancy; r++ {
			peerIdx := (i*redundancy + r) % fanout
			firstLevel[peerIdx].bscExt.AsyncSendBlockChunk(pkt)
		}
	}
	bsc.BlockChunkPathMeter.Mark(1)
	return true
}

// relayBlockChunk is called from the bsc backend Handle path when a chunk is
// received.  It forwards the chunk to a small subset of the node's own EVN
// peers (the second level of the fanout tree).  We avoid sending back to the
// peer that sent us the chunk.
func (h *handler) relayBlockChunk(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket) {
	if h.chunkPool == nil {
		return
	}
	tree := h.chunkFanoutTree(pkt.BlockHash)
	if len(tree.peers) == 0 {
		return
	}
	children := tree.childrenOf(fromPeer.ID())
	if len(children) == 0 {
		return
	}
	for _, child := range children {
		child.bscExt.AsyncSendBlockChunk(pkt)
	}
}

type chunkFanoutTree struct {
	peers  []*ethPeer
	fanout int
}

func (h *handler) chunkFanoutTree(blockHash common.Hash) chunkFanoutTree {
	h.peers.lock.RLock()
	allPeers := make([]*ethPeer, 0, len(h.peers.peers))
	for _, p := range h.peers.peers {
		allPeers = append(allPeers, p)
	}
	h.peers.lock.RUnlock()

	peers := h.evnChunkPeers(allPeers, blockHash)
	fanout := int(math.Ceil(math.Sqrt(float64(len(peers)))))
	if fanout < 1 {
		fanout = 1
	}
	if fanout > len(peers) {
		fanout = len(peers)
	}
	return chunkFanoutTree{peers: peers, fanout: fanout}
}

func (t chunkFanoutTree) childrenOf(peerID string) []*ethPeer {
	if len(t.peers) == 0 || t.fanout == 0 {
		return nil
	}
	parent := -1
	for i := 0; i < t.fanout; i++ {
		if t.peers[i].ID() == peerID {
			parent = i
			break
		}
	}
	if parent < 0 {
		return nil
	}
	children := make([]*ethPeer, 0)
	for i := t.fanout + parent; i < len(t.peers); i += t.fanout {
		children = append(children, t.peers[i])
	}
	return children
}

// requestMissingChunks asks a small set of Bsc3 EVN peers for shards still
// missing from the local reassembly. This closes the reliability loop for the
// best-effort fanout path while the legacy full-block path remains as fallback.
func (h *handler) requestMissingChunks(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket) {
	if h.chunkPool == nil {
		return
	}
	missing := h.chunkPool.MissingChunks(pkt.BlockHash)
	if len(missing) == 0 {
		return
	}
	var candidates []*ethPeer
	for _, p := range h.peers.peersWithoutBlock(pkt.BlockHash) {
		if !p.EVNPeerFlag.Load() || p.bscExt == nil || p.bscExt.Version() < bsc.Bsc3 {
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
	count := int(math.Ceil(math.Sqrt(float64(len(candidates)))))
	if count > len(candidates) {
		count = len(candidates)
	}
	start := int(pkt.ChunkIndex) % len(candidates)
	for i := 0; i < count; i++ {
		if err := candidates[(start+i)%len(candidates)].bscExt.RequestMissingChunks(pkt.BlockHash, missing); err != nil {
			log.Debug("Failed to request missing block shards", "hash", pkt.BlockHash, "peer", candidates[(start+i)%len(candidates)].ID(), "err", err)
		}
	}
}
