package bsc

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// chunkSize is the target size (in bytes) of each block chunk payload.  When a
// serialized block body exceeds `BlockChunkThreshold`, it is split into chunks
// of roughly this size.
const chunkSize = 64 * 1024 // 64KB

// BlockChunkThreshold is the minimum RLP-encoded block body size (in bytes)
// above which the chunk propagation path kicks in.  Below this threshold the
// regular full-block broadcast is kept.
const BlockChunkThreshold = 512 * 1024 // 512KB

// maxReassemblyAge is how long a partial/complete block assembly is kept in
// the chunk pool before being garbage-collected.
const maxReassemblyAge = 2 * time.Minute

// maxReassemblies is the maximum number of concurrent reassemblies tracked in
// the pool (a safety valve against memory blowup under attack).
const maxReassemblies = 64

// ChunkConfig carries the tunable parameters for the block chunk propagation.
// It is plumbed in from the eth layer so that tests and the node config can
// override the defaults.
type ChunkConfig struct {
	// Enable toggles the whole chunk propagation path.
	Enable bool
	// Threshold is the minimum serialized block body size to use the chunk
	// path.  Blocks smaller than this are broadcast via the legacy full-block
	// path.  If zero, BlockChunkThreshold is used.
	Threshold int
}

// DefaultChunkConfig returns a conservative default chunk config (disabled).
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{Enable: false, Threshold: BlockChunkThreshold}
}

// Reassembly holds the collected chunks for a single block together with the
// metadata needed to reconstruct it once all chunks arrive.
type Reassembly struct {
	blockHash  common.Hash
	number     uint64
	header     *types.Header
	chunkCount uint
	chunks     map[uint][]byte
	createdAt  time.Time
}

// ChunkPool collects incoming block chunks and, once a full set is available,
// reconstructs the original block and hands it to the registered delivery
// callback.  It is concurrency safe.
type ChunkPool struct {
	config ChunkConfig

	deliver  func(block *types.Block)                   // invoked when a block is fully reassembled
	hasBlock func(hash common.Hash, number uint64) bool // fast-path: skip if we already have it

	mu      sync.Mutex
	pending map[common.Hash]*Reassembly
}

// NewChunkPool creates a new chunk pool.  `deliver` is called (in the caller's
// goroutine) when a block has been fully reassembled.  `hasBlock` is an
// optional fast-path check that lets the pool drop chunks for blocks that are
// already known locally.
func NewChunkPool(config ChunkConfig, deliver func(block *types.Block), hasBlock func(hash common.Hash, number uint64) bool) *ChunkPool {
	if config.Threshold == 0 {
		config.Threshold = BlockChunkThreshold
	}
	return &ChunkPool{
		config:   config,
		deliver:  deliver,
		hasBlock: hasBlock,
		pending:  make(map[common.Hash]*Reassembly),
	}
}

// AddChunk ingests a single block chunk.  If the chunk completes a reassembly,
// the reconstructed block is delivered via the registered callback.  The
// returned bool reports whether the chunk was accepted (true) or dropped
// (false, e.g. duplicate / stale / pool full).
func (p *ChunkPool) AddChunk(pkt *BlockChunkPacket) bool {
	if p == nil || !p.config.Enable {
		return false
	}
	// Defensive validation.
	if pkt == nil || pkt.Header == nil || pkt.ChunkCount == 0 {
		return false
	}
	if pkt.ChunkIndex >= pkt.ChunkCount {
		return false
	}
	// Hash sanity: the advertised block hash must match the header hash.
	if pkt.BlockHash == (common.Hash{}) || pkt.BlockHash != pkt.Header.Hash() {
		log.Debug("Drop chunk with mismatched block hash", "hash", pkt.BlockHash, "headerHash", pkt.Header.Hash())
		return false
	}
	// Payload hash sanity.
	if crypto.Keccak256Hash(pkt.Payload) != pkt.PayloadHash {
		log.Debug("Drop chunk with bad payload hash", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
		return false
	}
	// Already-known block: skip silently.
	if p.hasBlock != nil && p.hasBlock(pkt.BlockHash, pkt.Number) {
		return false
	}

	p.mu.Lock()
	// GC opportunistically to bound memory.
	if len(p.pending) >= maxReassemblies {
		p.gcLocked()
	}
	// After GC, if we are still at capacity and this block is not tracked, drop.
	r, ok := p.pending[pkt.BlockHash]
	if !ok {
		if len(p.pending) >= maxReassemblies {
			p.mu.Unlock()
			log.Debug("Chunk pool full, dropping chunk", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
			return false
		}
		r = &Reassembly{
			blockHash:  pkt.BlockHash,
			number:     pkt.Number,
			header:     pkt.Header,
			chunkCount: pkt.ChunkCount,
			chunks:     make(map[uint][]byte),
			createdAt:  time.Now(),
		}
		p.pending[pkt.BlockHash] = r
	}
	// If we already have all chunks, ignore.
	if _, exists := r.chunks[pkt.ChunkIndex]; exists {
		p.mu.Unlock()
		return false
	}
	// If the total/chunkCount changed mid-stream (shouldn't happen for a valid
	// producer), reset the collection.
	if r.chunkCount != pkt.ChunkCount {
		r.chunkCount = pkt.ChunkCount
		r.chunks = make(map[uint][]byte)
	}
	// Copy the payload to avoid aliasing the caller's buffer.
	r.chunks[pkt.ChunkIndex] = append([]byte(nil), pkt.Payload...)

	complete := len(r.chunks) == int(r.chunkCount)
	if !complete {
		p.mu.Unlock()
		return true
	}
	// Complete: remove from pending and deliver outside the lock.
	delete(p.pending, pkt.BlockHash)
	p.mu.Unlock()

	block, err := reassemble(r)
	if err != nil {
		log.Warn("Failed to reassemble block from chunks", "hash", pkt.BlockHash, "err", err)
		return false
	}
	log.Debug("Reassembled block from chunks", "hash", pkt.BlockHash, "number", pkt.Number, "chunks", r.chunkCount)
	if p.deliver != nil {
		p.deliver(block)
	}
	return true
}

// gcLocked removes stale reassemblies.  Caller must hold p.mu.
func (p *ChunkPool) gcLocked() {
	now := time.Now()
	for h, r := range p.pending {
		if now.Sub(r.createdAt) > maxReassemblyAge {
			delete(p.pending, h)
		}
	}
}

// SplitBlock serializes `block` into a slice of BlockChunkPacket suitable for
// fanout distribution.  Only the body (transactions, uncles, withdrawals,
// sidecars) is chunked; the header is replicated in every packet so that
// receivers can validate the seal immediately and reconstruct the block hash.
//
// If the serialized body is smaller than the configured threshold, nil is
// returned to signal the caller that the legacy full-block path should be
// used instead.
func SplitBlock(block *types.Block, cfg ChunkConfig) ([]*BlockChunkPacket, error) {
	if block == nil {
		return nil, errors.New("nil block")
	}
	body := &BlockData{
		Header:      block.Header(),
		Txs:         block.Transactions(),
		Uncles:      block.Uncles(),
		Withdrawals: block.Withdrawals(),
		Sidecars:    block.Sidecars(),
	}
	enc, err := rlp.EncodeToBytes(body)
	if err != nil {
		return nil, err
	}
	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = BlockChunkThreshold
	}
	if len(enc) < threshold {
		return nil, nil
	}

	count := uint(math.Ceil(float64(len(enc)) / float64(chunkSize)))
	if count == 0 {
		count = 1
	}
	hash := block.Hash()
	number := block.NumberU64()
	pkts := make([]*BlockChunkPacket, count)
	for i := uint(0); i < count; i++ {
		start := int(i) * chunkSize
		end := start + chunkSize
		if end > len(enc) {
			end = len(enc)
		}
		payload := enc[start:end]
		pkts[i] = &BlockChunkPacket{
			BlockHash:   hash,
			Number:      number,
			Header:      block.Header(),
			ChunkIndex:  i,
			ChunkCount:  count,
			Payload:     payload,
			PayloadHash: crypto.Keccak256Hash(payload),
		}
	}
	return pkts, nil
}

// reassemble stitches the chunks back together and decodes the block.
func reassemble(r *Reassembly) (*types.Block, error) {
	// Concatenate chunks in order.
	var total int
	for _, c := range r.chunks {
		total += len(c)
	}
	enc := make([]byte, 0, total)
	for i := uint(0); i < r.chunkCount; i++ {
		c, ok := r.chunks[i]
		if !ok {
			return nil, errors.New("missing chunk during reassembly")
		}
		enc = append(enc, c...)
	}
	var body BlockData
	if err := rlp.DecodeBytes(enc, &body); err != nil {
		return nil, err
	}
	block := types.NewBlockWithHeader(body.Header).WithBody(types.Body{
		Transactions: body.Txs,
		Uncles:       body.Uncles,
		Withdrawals:  body.Withdrawals,
	})
	block = block.WithSidecars(body.Sidecars)
	if block.Hash() != r.blockHash {
		return nil, errors.New("reassembled block hash mismatch")
	}
	return block, nil
}
