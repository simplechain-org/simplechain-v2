package eth

import (
	"bytes"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	chunkRepairDelay       = 200 * time.Millisecond
	chunkRepairRetryDelay  = 500 * time.Millisecond
	chunkRepairMaxAttempts = 3
	chunkRepairPeerLimit   = 2
)

type chunkRepairKey struct {
	hash common.Hash
	root common.Hash
}

type chunkRepairState struct {
	sources          map[string]struct{}
	origin           string
	number           uint64
	attempts         int
	fallbackAttempts int
	generation       uint64
	timer            *time.Timer
}

// evnChunkPeers returns EVN peers that negotiated the authenticated Bsc4
// protocol. Ranking is block-dependent and uses an actual cryptographic hash,
// avoiding a permanent hot set of first-level relays.
func (h *handler) evnChunkPeers(peers []*ethPeer, blockHash common.Hash) []*ethPeer {
	out := make([]*ethPeer, 0, len(peers))
	for _, peer := range peers {
		if !peer.EVNPeerFlag.Load() || peer.bscExt == nil || peer.bscExt.Version() < bsc.Bsc4 {
			continue
		}
		out = append(out, peer)
	}
	sort.Slice(out, func(i, j int) bool {
		left := crypto.Keccak256Hash(blockHash[:], []byte(out[i].ID()))
		right := crypto.Keccak256Hash(blockHash[:], []byte(out[j].ID()))
		return bytes.Compare(left[:], right[:]) < 0
	})
	return out
}

// evnReceiptChunkPeers selects only Bsc5 peers for producer-originated shard
// propagation. Bsc4 peers cannot acknowledge completion, so they remain on
// the immediate full-block path during a rolling upgrade.
func (h *handler) evnReceiptChunkPeers(peers []*ethPeer, blockHash common.Hash, excluded map[string]struct{}) []*ethPeer {
	out := make([]*ethPeer, 0, len(peers))
	for _, peer := range peers {
		if _, skip := excluded[peer.ID()]; skip {
			continue
		}
		if !peer.EVNPeerFlag.Load() || peer.bscExt == nil || peer.bscExt.Version() < bsc.Bsc5 {
			continue
		}
		out = append(out, peer)
	}
	sort.Slice(out, func(i, j int) bool {
		left := crypto.Keccak256Hash(blockHash[:], []byte(out[i].ID()))
		right := crypto.Keccak256Hash(blockHash[:], []byte(out[j].ID()))
		return bytes.Compare(left[:], right[:]) < 0
	})
	return out
}

// distributeBlockChunks builds an explicit two-level tree from the leader's
// peer view. Every first-level relay receives the complete k+m encoding and
// every second-level target is assigned to up to two relays. Consequently, an
// actual recipient receives enough shards to reconstruct instead of receiving
// only a fraction of the encoding.
func (h *handler) distributeBlockChunks(peers []*ethPeer, pkts []*bsc.BlockChunkPacket, block *types.Block, td *big.Int, excluded map[string]struct{}) (map[string]struct{}, bool) {
	if h.chunkPool == nil || len(pkts) == 0 || block == nil || td == nil {
		return nil, false
	}
	if err := h.validateChunkOrigin(pkts[0].Header, h.nodeID); err != nil {
		log.Debug("Local node is not authorized to originate block shard manifest", "hash", pkts[0].BlockHash, "err", err)
		return nil, false
	}
	if err := bsc.SignShardManifest(pkts, h.nodeID, h.nodeKey); err != nil {
		log.Debug("Failed to sign block shard manifest", "hash", pkts[0].BlockHash, "err", err)
		return nil, false
	}
	evnPeers := h.evnReceiptChunkPeers(peers, pkts[0].BlockHash, excluded)
	if len(evnPeers) == 0 || !h.chunkPool.StoreOutgoing(pkts) {
		return nil, false
	}
	n := len(evnPeers)
	fanout := int(math.Ceil(math.Sqrt(float64(n))))
	fanout = max(1, min(fanout, n))
	relays := evnPeers[:fanout]
	redundancy := 1
	if fanout > 1 {
		redundancy = 2
	}
	assignments := relayTargetAssignments(n, fanout, redundancy)
	for _, assignment := range assignments {
		if len(assignment) > bsc.MaxBlockChunkRelayTargets {
			return nil, false
		}
	}

	receiptPeers := make(map[string]*bsc.Peer, len(evnPeers))
	repairPeers := make([]string, 0, len(evnPeers))
	for _, peer := range evnPeers {
		receiptPeers[peer.ID()] = peer.bscExt.Peer
		repairPeers = append(repairPeers, peer.ID())
	}
	if !h.chunkPool.AuthorizeRepairs(pkts[0].BlockHash, pkts[0].ShardRoot, repairPeers) {
		return nil, false
	}
	// Register before the first async send, otherwise a fast receiver can send
	// a valid receipt before the producer begins tracking it.
	receiptGeneration, ok := h.registerChunkReceipts(block, td, pkts[0].ShardRoot, receiptPeers)
	if !ok {
		return nil, false
	}
	covered := make(map[string]struct{}, len(evnPeers))
	for relayIndex, relay := range relays {
		route := make([]enode.ID, len(assignments[relayIndex]))
		for i, targetIndex := range assignments[relayIndex] {
			route[i] = evnPeers[targetIndex].NodeID()
		}
		for _, pkt := range pkts {
			routed := pkt.Clone()
			routed.RelayDepth = 0
			routed.RelayTargets = route
			if !relay.bscExt.AsyncSendBlockChunk(routed) {
				h.cancelChunkReceipts(pkts[0].BlockHash, pkts[0].ShardRoot, receiptGeneration)
				return nil, false
			}
		}
		covered[relay.ID()] = struct{}{}
	}
	// The leader's peer view is not a connectivity graph: a relay may not be
	// connected to all of the targets selected from the leader's view. Give
	// every second-level target one directly delivered seed shard so it can
	// trigger the bounded repair path from the leader's outgoing cache even
	// when both relay-target edges are absent.
	for targetIndex := fanout; targetIndex < n; targetIndex++ {
		target := evnPeers[targetIndex]
		seed := pkts[(targetIndex-fanout)%len(pkts)].Clone()
		seed.RelayDepth = 1
		seed.RelayTargets = nil
		if !target.bscExt.AsyncSendBlockChunk(seed) {
			h.cancelChunkReceipts(pkts[0].BlockHash, pkts[0].ShardRoot, receiptGeneration)
			return nil, false
		}
		covered[target.ID()] = struct{}{}
	}
	bsc.BlockChunkFanoutGauge.Update(int64(fanout))
	bsc.BlockChunkPeerGauge.Update(int64(n))
	bsc.BlockChunkPathMeter.Mark(1)
	log.Debug("Distributed authenticated block shards", "hash", pkts[0].BlockHash, "shards", len(pkts), "peers", n, "relays", fanout)
	return covered, true
}

func relayTargetAssignments(peerCount, fanout, redundancy int) [][]int {
	if peerCount <= 0 || fanout <= 0 || fanout > peerCount || redundancy <= 0 {
		return nil
	}
	redundancy = min(redundancy, fanout)
	assignments := make([][]int, fanout)
	for targetIndex := fanout; targetIndex < peerCount; targetIndex++ {
		offset := targetIndex - fanout
		for replica := 0; replica < redundancy; replica++ {
			relayIndex := (offset + replica) % fanout
			assignments[relayIndex] = append(assignments[relayIndex], targetIndex)
		}
	}
	return assignments
}

// relayBlockChunk follows the targets selected by the block producer. A relay
// forwards every shard it receives to the same targets, and second-level or
// repair packets are never forwarded again.
func (h *handler) relayBlockChunk(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket) {
	if h.chunkPool == nil || pkt == nil || pkt.RelayDepth != 0 {
		return
	}
	for _, targetID := range pkt.RelayTargets {
		target := h.peers.peer(targetID.String())
		if target == nil || target.ID() == fromPeer.ID() || !target.EVNPeerFlag.Load() ||
			target.bscExt == nil || target.bscExt.Version() < bsc.Bsc4 {
			continue
		}
		relayed := pkt.Clone()
		relayed.RelayDepth = 1
		relayed.RelayTargets = nil
		target.bscExt.AsyncSendBlockChunk(relayed)
	}
}

// requestMissingChunks schedules a debounced repair. The first request is sent
// to peers that actually supplied this encoding, which in the Bsc4 tree are the
// leader or a relay holding the full shard set.
func (h *handler) requestMissingChunks(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket) {
	h.observeChunkSource(fromPeer, pkt, true)
}

func (h *handler) observeChunkSource(fromPeer *bsc.Peer, pkt *bsc.BlockChunkPacket, progress bool) {
	if h.chunkPool == nil || fromPeer == nil || pkt == nil || pkt.ShardRoot == (common.Hash{}) {
		return
	}
	key := chunkRepairKey{hash: pkt.BlockHash, root: pkt.ShardRoot}
	h.chunkRepairMu.Lock()
	if h.chunkRepairStopped {
		h.chunkRepairMu.Unlock()
		return
	}
	state := h.chunkRepairs[key]
	if state == nil {
		if !progress {
			h.chunkRepairMu.Unlock()
			return
		}
		state = &chunkRepairState{sources: make(map[string]struct{}), number: pkt.Number}
		h.chunkRepairs[key] = state
	}
	state.sources[fromPeer.ID()] = struct{}{}
	if pkt.OriginNodeID != (enode.ID{}) {
		state.origin = pkt.OriginNodeID.String()
		state.sources[state.origin] = struct{}{}
	}
	if !progress {
		h.chunkRepairMu.Unlock()
		return
	}
	// Treat every newly received shard as progress. The idle timer is reset so
	// a large but healthy relay stream is not raced by an origin repair that
	// would duplicate the entire encoding.
	state.attempts = 0
	state.fallbackAttempts = 0
	h.scheduleChunkRepairLocked(key, state, chunkRepairDelay)
	h.chunkRepairMu.Unlock()
}

func (h *handler) scheduleChunkRepairLocked(key chunkRepairKey, state *chunkRepairState, delay time.Duration) {
	if state.timer != nil {
		if state.timer.Stop() {
			h.chunkRepairWG.Done()
		}
		state.timer = nil
	}
	state.generation++
	generation := state.generation
	h.chunkRepairWG.Add(1)
	state.timer = time.AfterFunc(delay, func() {
		defer h.chunkRepairWG.Done()
		h.runChunkRepair(key, generation)
	})
}

func (h *handler) runChunkRepair(key chunkRepairKey, generation uint64) {
	h.chunkRepairMu.Lock()
	state := h.chunkRepairs[key]
	if h.chunkRepairStopped || state == nil || state.generation != generation {
		h.chunkRepairMu.Unlock()
		return
	}
	state.timer = nil // This callback now owns the scheduled generation.
	h.chunkRepairMu.Unlock()

	missing, needed, status := h.chunkPool.MissingChunks(key.hash, key.root)
	switch status {
	case bsc.ChunkRepairCompleted:
		h.clearChunkRepairGeneration(key, generation)
		return
	case bsc.ChunkRepairReconstructing:
		// Reed-Solomon reconstruction runs outside the pool lock. Keep the
		// recovery state alive until that attempt reports completed or failed.
		h.rescheduleChunkRepairGeneration(key, generation, chunkRepairRetryDelay)
		return
	case bsc.ChunkRepairFailed, bsc.ChunkRepairUnknown, bsc.ChunkRepairUnusable:
		// A repair state is created only after a shard was accepted. Losing the
		// corresponding pool entry therefore means reassembly failed, was evicted,
		// or aged out and must fall back to the ordinary eth fetcher.
		h.fallbackChunkRepairGeneration(key, generation)
		return
	case bsc.ChunkRepairPending:
		if needed <= 0 || len(missing) == 0 {
			h.rescheduleChunkRepairGeneration(key, generation, chunkRepairRetryDelay)
			return
		}
	default:
		h.fallbackChunkRepairGeneration(key, generation)
		return
	}
	h.chunkRepairMu.Lock()
	state = h.chunkRepairs[key]
	if h.chunkRepairStopped || state == nil || state.generation != generation {
		h.chunkRepairMu.Unlock()
		return
	}
	if state.attempts >= chunkRepairMaxAttempts {
		h.chunkRepairMu.Unlock()
		h.fallbackChunkRepairGeneration(key, generation)
		return
	}
	state.attempts++
	sourceIDs := make([]string, 0, len(state.sources))
	for id := range state.sources {
		if id == state.origin {
			continue
		}
		sourceIDs = append(sourceIDs, id)
	}
	sort.Strings(sourceIDs)
	if state.origin != "" {
		sourceIDs = append([]string{state.origin}, sourceIDs...)
	}
	h.scheduleChunkRepairLocked(key, state, chunkRepairRetryDelay)
	h.chunkRepairMu.Unlock()

	candidates := make([]*ethPeer, 0, chunkRepairPeerLimit)
	selected := make(map[string]struct{}, chunkRepairPeerLimit)
	for _, id := range sourceIDs {
		// Every recorded source supplied a packet that passed the producer
		// manifest checks. Do not depend on the asynchronously refreshed local
		// EVN flag when asking that source for repair.
		if peer := h.peers.peer(id); peer != nil && peer.bscExt != nil && peer.bscExt.Version() >= bsc.Bsc4 {
			candidates = append(candidates, peer)
			selected[peer.ID()] = struct{}{}
			if len(candidates) == chunkRepairPeerLimit {
				break
			}
		}
	}
	if len(candidates) < chunkRepairPeerLimit {
		all := h.peers.peersWithoutBlock(key.hash)
		for _, peer := range h.evnChunkPeers(all, key.hash) {
			if _, ok := selected[peer.ID()]; ok {
				continue
			}
			candidates = append(candidates, peer)
			selected[peer.ID()] = struct{}{}
			if len(candidates) == chunkRepairPeerLimit {
				break
			}
		}
	}
	for _, peer := range candidates {
		if err := peer.bscExt.RequestMissingChunks(key.hash, key.root, missing); err != nil {
			log.Debug("Failed to request missing block shards", "hash", key.hash, "peer", peer.ID(), "err", err)
		}
	}
}

func (h *handler) rescheduleChunkRepairGeneration(key chunkRepairKey, generation uint64, delay time.Duration) {
	h.chunkRepairMu.Lock()
	if state := h.chunkRepairs[key]; !h.chunkRepairStopped && state != nil && state.generation == generation {
		h.scheduleChunkRepairLocked(key, state, delay)
	}
	h.chunkRepairMu.Unlock()
}

func (h *handler) fallbackChunkRepairGeneration(key chunkRepairKey, generation uint64) {
	h.chunkRepairMu.Lock()
	state := h.chunkRepairs[key]
	if h.chunkRepairStopped || state == nil || state.generation != generation {
		h.chunkRepairMu.Unlock()
		return
	}
	origin := state.origin
	number := state.number
	sources := make([]string, 0, len(state.sources))
	for id := range state.sources {
		sources = append(sources, id)
	}
	h.chunkRepairMu.Unlock()

	log.Debug("Falling back to eth fetch after block shard state became terminal", "hash", key.hash, "root", key.root)
	scheduled := h.fallbackChunkToEthFetch(key.hash, number, origin, sources)

	h.chunkRepairMu.Lock()
	state = h.chunkRepairs[key]
	if h.chunkRepairStopped || state == nil || state.generation != generation {
		h.chunkRepairMu.Unlock()
		return
	}
	if scheduled > 0 {
		delete(h.chunkRepairs, key)
		h.chunkRepairMu.Unlock()
		return
	}
	state.fallbackAttempts++
	if state.fallbackAttempts >= chunkRepairMaxAttempts {
		delete(h.chunkRepairs, key)
		h.chunkRepairMu.Unlock()
		log.Debug("Giving up eth fallback after block shard sources remained unavailable", "hash", key.hash, "root", key.root, "attempts", state.fallbackAttempts)
		return
	}
	h.scheduleChunkRepairLocked(key, state, chunkRepairRetryDelay)
	h.chunkRepairMu.Unlock()
}

// fallbackChunkToEthFetch hands a failed shard repair to the ordinary eth
// block fetcher. This is deliberately a pull fallback: it works even when the
// producer-side receipt timer is unavailable (for example with an older Bsc4
// producer) and does not require the shard sender to know our queue state.
func (h *handler) fallbackChunkToEthFetch(hash common.Hash, number uint64, origin string, sources []string) int {
	if h == nil || h.blockFetcher == nil || h.peers == nil {
		return 0
	}
	scheduled := 0
	for _, id := range chunkFallbackPeerIDs(origin, sources) {
		peer := h.peers.peer(id)
		if peer == nil {
			continue
		}
		if err := h.blockFetcher.Notify(peer.ID(), hash, number, time.Now(), peer.RequestOneHeader, peer.RequestBodies); err != nil {
			log.Debug("Failed to schedule eth fallback for block shards", "hash", hash, "peer", peer.ID(), "err", err)
			continue
		}
		scheduled++
		log.Debug("Scheduled eth fallback after shard repair exhaustion", "hash", hash, "peer", peer.ID())
	}
	return scheduled
}

// chunkFallbackPeerIDs bounds the normal ETH fallback to the shard origin and
// one distinct shard source. Both are notified when available: the block
// fetcher deduplicates competing announcements, while a second source avoids
// making recovery depend on the origin remaining reachable.
func chunkFallbackPeerIDs(origin string, sources []string) []string {
	ids := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		return true
	}
	add(origin)

	// Sources originate from a map in the repair state. Sort a private copy so
	// recovery behavior is reproducible and never mutates the caller's slice.
	candidates := append([]string(nil), sources...)
	sort.Strings(candidates)
	for _, source := range candidates {
		if add(source) {
			break
		}
	}
	return ids
}

func (h *handler) clearChunkRepair(key chunkRepairKey) {
	h.chunkRepairMu.Lock()
	h.clearChunkRepairLocked(key)
	h.chunkRepairMu.Unlock()
}

func (h *handler) clearChunkRepairGeneration(key chunkRepairKey, generation uint64) {
	h.chunkRepairMu.Lock()
	if state := h.chunkRepairs[key]; state != nil && state.generation == generation {
		h.clearChunkRepairLocked(key)
	}
	h.chunkRepairMu.Unlock()
}

func (h *handler) clearChunkRepairLocked(key chunkRepairKey) {
	if state := h.chunkRepairs[key]; state != nil && state.timer != nil {
		if state.timer.Stop() {
			h.chunkRepairWG.Done()
		}
	}
	delete(h.chunkRepairs, key)
}

func (h *handler) stopChunkRepairs() {
	h.chunkRepairMu.Lock()
	h.chunkRepairStopped = true
	for key := range h.chunkRepairs {
		h.clearChunkRepairLocked(key)
	}
	h.chunkRepairMu.Unlock()
	h.chunkRepairWG.Wait()
}
