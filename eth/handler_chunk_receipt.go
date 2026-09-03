package eth

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// A usable Bsc5 encoding is expected to be acknowledged well before the
	// repair loop exhausts its retries. Keep the delay long enough for a healthy
	// relay path, then fall back to normal eth full-block propagation.
	chunkReceiptFallbackDelay       = 2 * time.Second
	chunkReceiptFallbackRetryDelay  = time.Second
	chunkReceiptFallbackMaxAttempts = 3
)

type chunkReceiptKey struct {
	hash common.Hash
	root common.Hash
}

type chunkReceiptState struct {
	block      *types.Block
	td         *big.Int
	pending    map[string]*bsc.Peer
	attempts   int
	generation uint64
	timer      *time.Timer
}

// registerChunkReceipts starts producer-side delivery tracking before any
// shard is queued. This ordering is important: a fast Bsc5 receiver can
// reconstruct and acknowledge while the producer is still fanning out shards.
// The returned generation must be supplied when canceling this registration so
// an older fanout cannot clear a newer state for the same deterministic root.
func (h *handler) registerChunkReceipts(block *types.Block, td *big.Int, root common.Hash, peers map[string]*bsc.Peer) (uint64, bool) {
	if h == nil || block == nil || td == nil || root == (common.Hash{}) || len(peers) == 0 {
		return 0, false
	}
	key := chunkReceiptKey{hash: block.Hash(), root: root}
	pending := make(map[string]*bsc.Peer, len(peers))
	for id, peer := range peers {
		if id == "" || peer == nil || peer.Version() < bsc.Bsc5 || peer.ID() != id {
			continue
		}
		pending[id] = peer
	}
	if len(pending) == 0 {
		return 0, false
	}

	h.chunkReceiptMu.Lock()
	defer h.chunkReceiptMu.Unlock()
	if h.chunkReceiptStopped {
		return 0, false
	}
	if h.chunkReceipts == nil {
		h.chunkReceipts = make(map[chunkReceiptKey]*chunkReceiptState)
	}
	if previous := h.chunkReceipts[key]; previous != nil {
		h.clearChunkReceiptLocked(key)
	}
	h.chunkReceiptSeq++
	if h.chunkReceiptSeq == 0 {
		h.chunkReceiptSeq++
	}
	state := &chunkReceiptState{
		block:      block,
		td:         new(big.Int).Set(td),
		pending:    pending,
		generation: h.chunkReceiptSeq,
	}
	h.chunkReceipts[key] = state
	h.scheduleChunkReceiptFallbackLocked(key, state, chunkReceiptFallbackDelay)
	return state.generation, true
}

// acknowledgeChunkReceipt records a Bsc5 peer that accepted the reconstructed
// block into its normal processing path. The authenticated connection instance
// must match the one selected for this fanout.
func (h *handler) acknowledgeChunkReceipt(peer *bsc.Peer, receipt *bsc.BlockChunkReceiptPacket) {
	if h == nil || peer == nil || receipt == nil || peer.Version() < bsc.Bsc5 {
		return
	}
	key := chunkReceiptKey{hash: receipt.BlockHash, root: receipt.ShardRoot}
	h.chunkReceiptMu.Lock()
	defer h.chunkReceiptMu.Unlock()
	state := h.chunkReceipts[key]
	if state == nil {
		return
	}
	expected, ok := state.pending[peer.ID()]
	if !ok || expected != peer {
		return
	}
	delete(state.pending, peer.ID())
	if len(state.pending) == 0 {
		h.clearChunkReceiptLocked(key)
	}
}

// cancelChunkReceipts removes a delivery state if fanout could not be fully
// queued. The caller then uses the immediate full-block fallback path instead.
func (h *handler) cancelChunkReceipts(hash, root common.Hash, generation uint64) {
	key := chunkReceiptKey{hash: hash, root: root}
	h.chunkReceiptMu.Lock()
	if state := h.chunkReceipts[key]; state != nil && state.generation == generation {
		h.clearChunkReceiptLocked(key)
	}
	h.chunkReceiptMu.Unlock()
}

func (h *handler) scheduleChunkReceiptFallbackLocked(key chunkReceiptKey, state *chunkReceiptState, delay time.Duration) {
	if state.timer != nil {
		if state.timer.Stop() {
			h.chunkReceiptWG.Done()
		}
		state.timer = nil
	}
	h.chunkReceiptWG.Add(1)
	generation := state.generation
	state.timer = time.AfterFunc(delay, func() {
		defer h.chunkReceiptWG.Done()
		h.runChunkReceiptFallback(key, generation)
	})
}

func (h *handler) runChunkReceiptFallback(key chunkReceiptKey, generation uint64) {
	h.chunkReceiptMu.Lock()
	state := h.chunkReceipts[key]
	if h.chunkReceiptStopped || state == nil || state.generation != generation {
		h.chunkReceiptMu.Unlock()
		return
	}
	state.timer = nil // This callback owns the active timer generation.
	if state.block == nil || state.td == nil {
		log.Warn("Discarding invalid block shard receipt state", "hash", key.hash)
		h.clearChunkReceiptLocked(key)
		h.chunkReceiptMu.Unlock()
		return
	}
	pending := make([]string, 0, len(state.pending))
	for id := range state.pending {
		pending = append(pending, id)
	}
	block, td := state.block, new(big.Int).Set(state.td)
	h.chunkReceiptMu.Unlock()

	retry := make(map[string]struct{})
	for _, id := range pending {
		peer := h.peers.peer(id)
		if peer == nil {
			retry[id] = struct{}{}
			continue
		}
		if peer.AsyncSendNewBlock(block, td) {
			log.Debug("Fell back to full block after missing shard receipt", "hash", block.Hash(), "peer", id)
			continue
		}
		retry[id] = struct{}{}
		// An announcement is only a last-resort pull hint. The full-block queue
		// is retried below before the state is finally retired.
		peer.AsyncSendNewBlockHash(block)
	}

	h.chunkReceiptMu.Lock()
	defer h.chunkReceiptMu.Unlock()
	state = h.chunkReceipts[key]
	if h.chunkReceiptStopped || state == nil || state.generation != generation {
		return
	}
	for _, id := range pending {
		if _, stillPending := state.pending[id]; !stillPending {
			continue // Receipt arrived while the fallback was being queued.
		}
		if _, shouldRetry := retry[id]; shouldRetry {
			continue
		}
		delete(state.pending, id)
	}
	if len(state.pending) == 0 {
		h.clearChunkReceiptLocked(key)
		return
	}
	state.attempts++
	if state.attempts >= chunkReceiptFallbackMaxAttempts {
		log.Warn("Unable to enqueue full block fallback after shard receipt timeout", "hash", key.hash, "peers", len(state.pending))
		h.clearChunkReceiptLocked(key)
		return
	}
	h.scheduleChunkReceiptFallbackLocked(key, state, chunkReceiptFallbackRetryDelay)
}

func (h *handler) clearChunkReceiptLocked(key chunkReceiptKey) {
	state := h.chunkReceipts[key]
	if state == nil {
		return
	}
	if state.timer != nil && state.timer.Stop() {
		h.chunkReceiptWG.Done()
	}
	delete(h.chunkReceipts, key)
}

func (h *handler) stopChunkReceipts() {
	h.chunkReceiptMu.Lock()
	h.chunkReceiptStopped = true
	for key := range h.chunkReceipts {
		h.clearChunkReceiptLocked(key)
	}
	h.chunkReceiptMu.Unlock()
	h.chunkReceiptWG.Wait()
}
