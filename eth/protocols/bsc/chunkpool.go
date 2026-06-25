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
	"github.com/klauspost/reedsolomon"
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

const (
	defaultParityShards = 4
	maxShardCount       = 256
	maxBlockShardBytes  = 32 * 1024 * 1024
)

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
	// ParityShards is the number of Reed-Solomon parity shards produced for a
	// chunked block. If zero, a conservative default is used.
	ParityShards int
}

// DefaultChunkConfig returns a conservative default chunk config (disabled).
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{Enable: false, Threshold: BlockChunkThreshold}
}

// Reassembly holds the collected chunks for a single block together with the
// metadata needed to reconstruct it once all chunks arrive.
type Reassembly struct {
	blockHash        common.Hash
	number           uint64
	headerHash       common.Hash
	chunkCount       uint
	dataShardCount   uint
	parityShardCount uint
	originalSize     uint64
	chunks           map[uint][]byte
	createdAt        time.Time
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
	BlockChunkShardInMeter.Mark(1)
	// Defensive validation.
	if pkt == nil || pkt.Header == nil || pkt.ChunkCount == 0 {
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	if pkt.ChunkIndex >= pkt.ChunkCount || pkt.DataShardCount == 0 || pkt.ParityShardCount == 0 {
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	if pkt.ChunkCount != pkt.DataShardCount+pkt.ParityShardCount {
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	if pkt.ChunkCount > maxShardCount || len(pkt.Payload) == 0 || len(pkt.Payload) > chunkSize {
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	if pkt.OriginalSize == 0 || pkt.OriginalSize > maxBlockShardBytes {
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	// Hash sanity: the advertised block hash must match the header hash.
	if pkt.BlockHash == (common.Hash{}) || pkt.BlockHash != pkt.Header.Hash() {
		log.Debug("Drop chunk with mismatched block hash", "hash", pkt.BlockHash, "headerHash", pkt.Header.Hash())
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	if pkt.Number != pkt.Header.Number.Uint64() {
		log.Debug("Drop chunk with mismatched block number", "hash", pkt.BlockHash, "number", pkt.Number, "headerNumber", pkt.Header.Number)
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	// Payload hash sanity.
	if crypto.Keccak256Hash(pkt.Payload) != pkt.PayloadHash {
		log.Debug("Drop chunk with bad payload hash", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
		BlockChunkShardDropMeter.Mark(1)
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
			BlockChunkShardDropMeter.Mark(1)
			return false
		}
		r = &Reassembly{
			blockHash:        pkt.BlockHash,
			number:           pkt.Number,
			headerHash:       pkt.Header.Hash(),
			chunkCount:       pkt.ChunkCount,
			dataShardCount:   pkt.DataShardCount,
			parityShardCount: pkt.ParityShardCount,
			originalSize:     pkt.OriginalSize,
			chunks:           make(map[uint][]byte),
			createdAt:        time.Now(),
		}
		p.pending[pkt.BlockHash] = r
	}
	if !r.matches(pkt) {
		p.mu.Unlock()
		log.Debug("Drop chunk with inconsistent metadata", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
	// If we already have all chunks, ignore.
	if _, exists := r.chunks[pkt.ChunkIndex]; exists {
		p.mu.Unlock()
		return false
	}
	// Copy the payload to avoid aliasing the caller's buffer.
	r.chunks[pkt.ChunkIndex] = append([]byte(nil), pkt.Payload...)

	complete := len(r.chunks) >= int(r.dataShardCount)
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
		BlockChunkReassembleErrors.Mark(1)
		return false
	}
	BlockChunkReassembleTimer.UpdateSince(r.createdAt)
	BlockChunkReassembleMeter.Mark(1)
	log.Debug("Reassembled block from chunks", "hash", pkt.BlockHash, "number", pkt.Number, "chunks", r.chunkCount)
	if p.deliver != nil {
		p.deliver(block)
	}
	return true
}

func (r *Reassembly) matches(pkt *BlockChunkPacket) bool {
	return r.number == pkt.Number &&
		r.headerHash == pkt.Header.Hash() &&
		r.chunkCount == pkt.ChunkCount &&
		r.dataShardCount == pkt.DataShardCount &&
		r.parityShardCount == pkt.ParityShardCount &&
		r.originalSize == pkt.OriginalSize
}

// MissingChunks returns missing shard indexes for a partially reassembled block.
func (p *ChunkPool) MissingChunks(hash common.Hash) []uint {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.pending[hash]
	if r == nil {
		return nil
	}
	missing := make([]uint, 0, r.chunkCount-uint(len(r.chunks)))
	for i := uint(0); i < r.chunkCount; i++ {
		if _, ok := r.chunks[i]; !ok {
			missing = append(missing, i)
		}
	}
	return missing
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

// SplitBlock serializes `block` into Reed-Solomon shards suitable for fanout
// distribution. Only the body (transactions, uncles, withdrawals, sidecars) is
// encoded; the header is replicated in every packet so receivers can validate
// the block hash early.
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

	dataShards := uint(math.Ceil(float64(len(enc)) / float64(chunkSize)))
	if dataShards == 0 {
		dataShards = 1
	}
	parityShards := cfg.ParityShards
	if parityShards == 0 {
		parityShards = defaultParityShards
	}
	if parityShards < 0 {
		return nil, errors.New("negative parity shard count")
	}
	count := dataShards + uint(parityShards)
	if count > maxShardCount || uint64(dataShards)*chunkSize > maxBlockShardBytes {
		return nil, errors.New("block shard count exceeds limit")
	}
	encShards, err := reedsolomon.New(int(dataShards), parityShards)
	if err != nil {
		return nil, err
	}
	shards, err := encShards.Split(enc)
	if err != nil {
		return nil, err
	}
	if err := encShards.Encode(shards); err != nil {
		return nil, err
	}
	hash := block.Hash()
	number := block.NumberU64()
	pkts := make([]*BlockChunkPacket, count)
	for i := uint(0); i < count; i++ {
		payload := shards[i]
		pkts[i] = &BlockChunkPacket{
			BlockHash:        hash,
			Number:           number,
			Header:           block.Header(),
			ChunkIndex:       i,
			ChunkCount:       count,
			DataShardCount:   dataShards,
			ParityShardCount: uint(parityShards),
			OriginalSize:     uint64(len(enc)),
			Payload:          payload,
			PayloadHash:      crypto.Keccak256Hash(payload),
		}
	}
	return pkts, nil
}

// reassemble reconstructs the encoded data from any dataShardCount shards and
// decodes the block.
func reassemble(r *Reassembly) (*types.Block, error) {
	shards := make([][]byte, r.chunkCount)
	for i := uint(0); i < r.chunkCount; i++ {
		shards[i] = r.chunks[i]
	}
	encShards, err := reedsolomon.New(int(r.dataShardCount), int(r.parityShardCount))
	if err != nil {
		return nil, err
	}
	if err := encShards.Reconstruct(shards); err != nil {
		return nil, err
	}
	var enc []byte
	for i := uint(0); i < r.dataShardCount; i++ {
		enc = append(enc, shards[i]...)
	}
	if uint64(len(enc)) < r.originalSize {
		return nil, errors.New("reconstructed data shorter than original size")
	}
	enc = enc[:r.originalSize]
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
