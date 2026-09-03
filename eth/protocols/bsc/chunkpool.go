package bsc

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/klauspost/reedsolomon"
)

const chunkSize = 64 * 1024

// BlockChunkThreshold is the minimum encoded block size that uses Bsc4.
const BlockChunkThreshold = 512 * 1024

const (
	maxReassemblyAge       = 2 * time.Minute
	completedCacheAge      = 30 * time.Second
	maxReassemblies        = 64
	maxCompletedAssemblies = 8
	maxFailedAssemblies    = 128
	maxEncodingsPerBlock   = 2
	defaultParityShards    = 4
	maxShardCount          = MaxBlockChunkRequestIndexes
	// Every encoding has at least one parity shard, so no more than 255
	// 64 KiB data shards fit in the 256-shard wire limit.
	maxBlockShardBytes                = uint64((maxShardCount - 1) * chunkSize)
	maxPendingShardBytes              = 64 * 1024 * 1024
	maxCompletedShardBytes            = 64 * 1024 * 1024
	maxVerifiedHeaders                = 128
	maxVerifiedManifests              = 256
	maxHeaderVerificationsPerSecond   = 32
	maxManifestVerificationsPerSecond = 64
	maxRepairPeersPerEncoding         = maxShardCount * 2
	maxRepairEgressBytesPerSecond     = 64 * 1024 * 1024
	maxRepairEgressShardsPerSecond    = maxShardCount * 4
	blockChunkPacketFixedSize         = uint64(unsafe.Sizeof(BlockChunkPacket{}))
	blockHeaderFixedSize              = uint64(unsafe.Sizeof(types.Header{}))
)

// ErrDeferredHeaderValidation tells ChunkPool that a structurally valid header
// is near the local head but cannot yet be fully checked (for example because
// its parent has not arrived). Such headers are cached separately from fully
// verified headers and are always revalidated by BlockFetcher before import.
var ErrDeferredHeaderValidation = errors.New("deferred block chunk header validation")

// ChunkConfig carries tunable block chunk propagation parameters.
type ChunkConfig struct {
	Enable       bool
	Threshold    int
	ParityShards int
}

func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{Enable: false, Threshold: BlockChunkThreshold}
}

// ValidateChunkConfig verifies that the configured threshold can be encoded
// with the requested parity within the Bsc4 256-shard wire limit.
func ValidateChunkConfig(cfg ChunkConfig) error {
	_, _, err := resolveChunkConfig(cfg)
	return err
}

func resolveChunkConfig(cfg ChunkConfig) (int, int, error) {
	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = BlockChunkThreshold
	}
	if threshold < 0 {
		return 0, 0, errors.New("negative block chunk threshold")
	}
	parityShards := cfg.ParityShards
	if parityShards == 0 {
		parityShards = defaultParityShards
	}
	if parityShards < 0 || parityShards >= maxShardCount {
		return 0, 0, fmt.Errorf("block chunk parity shard count %d is outside [1, %d)", parityShards, maxShardCount)
	}
	maxBytes := uint64(maxShardCount-parityShards) * chunkSize
	if uint64(threshold) > maxBytes {
		return 0, 0, fmt.Errorf("block chunk threshold %d exceeds maximum encodable size %d for %d parity shards", threshold, maxBytes, parityShards)
	}
	return threshold, parityShards, nil
}

type assemblyKey struct {
	blockHash common.Hash
	shardRoot common.Hash
}

type manifestKey struct {
	assemblyKey
	origin    enode.ID
	signature common.Hash
}

// verificationCall coalesces concurrent validation attempts for the same
// header or manifest. Duplicate shards arriving together must not each reserve
// the global verification budget before the first result reaches the cache.
type verificationCall struct {
	done chan struct{}
	ok   bool
}

type reconstructedEncoding struct {
	shards [][]byte
	hashes map[uint]common.Hash
	proofs [][]common.Hash
}

// Reassembly holds one independently authenticated encoding of a block.
type Reassembly struct {
	blockHash        common.Hash
	number           uint64
	header           *types.Header
	headerHash       common.Hash
	shardRoot        common.Hash
	originNodeID     enode.ID
	rootSignature    []byte
	chunkCount       uint
	dataShardCount   uint
	parityShardCount uint
	originalSize     uint64
	shardSize        int
	skipDelivery     bool
	usable           bool
	chunks           map[uint][]byte
	packets          map[uint]*BlockChunkPacket
	canonicalHashes  map[uint]common.Hash
	sources          map[string]struct{}
	repairPeers      map[string]struct{}
	primarySource    string
	storedBytes      uint64
	createdAt        time.Time
	completedAt      time.Time
	reconstructing   bool
}

// ChunkPool collects shards, caches recent encodings for repair, and delivers
// fully validated block bodies. Header consensus validation is supplied by the
// eth layer and is cached per block hash.
type ChunkPool struct {
	config         ChunkConfig
	deliver        func(block *types.Block, source string) bool
	hasBlock       func(hash common.Hash, number uint64) bool
	validateHeader func(header *types.Header) error
	validateOrigin func(header *types.Header, origin enode.ID) error

	mu                sync.Mutex
	pending           map[assemblyKey]*Reassembly
	completed         map[assemblyKey]*Reassembly
	failed            map[assemblyKey]time.Time
	pendingBytes      uint64
	completedBytes    uint64
	verifiedHeaders   map[common.Hash]time.Time
	deferredHeaders   map[common.Hash]time.Time
	verifiedManifests map[manifestKey]time.Time
	headerChecks      map[common.Hash]*verificationCall
	manifestChecks    map[manifestKey]*verificationCall
	headerRateAt      time.Time
	headerRateCount   int
	manifestRateAt    time.Time
	manifestRateCount int
	repairEgressAt    time.Time
	repairEgressBytes uint64
	repairEgressCount int
}

func NewChunkPool(
	config ChunkConfig,
	deliver func(block *types.Block, source string) bool,
	hasBlock func(hash common.Hash, number uint64) bool,
	validateHeader func(header *types.Header) error,
	validateOrigin func(header *types.Header, origin enode.ID) error,
) *ChunkPool {
	if config.Threshold == 0 {
		config.Threshold = BlockChunkThreshold
	}
	return &ChunkPool{
		config:            config,
		deliver:           deliver,
		hasBlock:          hasBlock,
		validateHeader:    validateHeader,
		validateOrigin:    validateOrigin,
		pending:           make(map[assemblyKey]*Reassembly),
		completed:         make(map[assemblyKey]*Reassembly),
		failed:            make(map[assemblyKey]time.Time),
		verifiedHeaders:   make(map[common.Hash]time.Time),
		deferredHeaders:   make(map[common.Hash]time.Time),
		verifiedManifests: make(map[manifestKey]time.Time),
		headerChecks:      make(map[common.Hash]*verificationCall),
		manifestChecks:    make(map[manifestKey]*verificationCall),
	}
}

type chunkAddStatus uint8

const (
	chunkRejected chunkAddStatus = iota
	chunkDuplicate
	chunkAccepted
)

// AddChunk validates and stores a shard. source is the peer ID used to
// attribute a reconstructed block to the block fetcher. It returns true only
// for a new shard that is eligible for relay; callers that need to observe a
// valid duplicate can use addChunkStatus inside this package.
func (p *ChunkPool) AddChunk(pkt *BlockChunkPacket, source string) bool {
	return p.addChunkStatus(pkt, source) == chunkAccepted
}

func (p *ChunkPool) addChunkStatus(pkt *BlockChunkPacket, source string) chunkAddStatus {
	if p == nil || !p.config.Enable {
		return chunkRejected
	}
	BlockChunkShardInMeter.Mark(1)
	if !validChunkPacket(pkt) {
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	key := assemblyKey{blockHash: pkt.BlockHash, shardRoot: pkt.ShardRoot}
	// Do the cheap per-encoding admission check before consensus/origin
	// verification. This prevents rejected roots from populating verification
	// caches or repeatedly consuming consensus CPU once the block budget is
	// exhausted.
	p.mu.Lock()
	p.gcLocked()
	if _, ok := p.failed[key]; ok {
		p.mu.Unlock()
		return chunkRejected
	}
	knownReassembly := p.pending[key]
	if knownReassembly == nil {
		knownReassembly = p.completed[key]
	}
	knownEncoding := knownReassembly != nil
	if !knownEncoding && p.encodingCountLocked(pkt.BlockHash) >= maxEncodingsPerBlock {
		p.mu.Unlock()
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	// Once an encoding is established, reject attempts to change its manifest
	// before spending either global verification budget. Relay routing fields
	// may vary, but the codeword metadata, origin and signature may not.
	if knownReassembly != nil && !knownReassembly.manifestMatches(pkt) {
		p.mu.Unlock()
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	p.mu.Unlock()

	knownBlock := p.hasBlock != nil && p.hasBlock(pkt.BlockHash, pkt.Number)
	if !p.manifestAuthorized(pkt) {
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	if !knownBlock && !p.headerVerified(pkt) {
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}

	p.mu.Lock()
	p.gcLocked()
	if _, ok := p.failed[key]; ok {
		p.mu.Unlock()
		return chunkRejected
	}
	r := p.completed[key]
	completed := r != nil
	if r == nil {
		r = p.pending[key]
	}
	newAssembly := false
	shardBytes := retainedShardSize(pkt)
	if r == nil {
		if p.encodingCountLocked(pkt.BlockHash) >= maxEncodingsPerBlock {
			p.mu.Unlock()
			BlockChunkShardDropMeter.Mark(1)
			return chunkRejected
		}
		if !p.makePendingRoomLocked(key, retainedManifestSize(pkt)+shardBytes) {
			p.mu.Unlock()
			BlockChunkShardDropMeter.Mark(1)
			return chunkRejected
		}
		r = newReassembly(pkt)
		r.skipDelivery = knownBlock
		r.usable = knownBlock
		p.pending[key] = r
		p.pendingBytes += r.storedBytes
		newAssembly = true
	}
	if !r.manifestMatches(pkt) {
		p.mu.Unlock()
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	r.authorizeRepairTargets(pkt.RelayTargets)
	if knownBlock {
		r.skipDelivery = true
	}
	if _, exists := r.chunks[pkt.ChunkIndex]; exists {
		r.recordSource(source)
		p.mu.Unlock()
		return chunkDuplicate
	}
	if completed && r.canonicalHashes != nil {
		if expected, ok := r.canonicalHashes[pkt.ChunkIndex]; ok && expected != pkt.PayloadHash {
			p.removeCompletedLocked(key)
			p.failed[key] = time.Now()
			p.trimFailedLocked()
			p.mu.Unlock()
			BlockChunkShardDropMeter.Mark(1)
			return chunkRejected
		}
	}
	if !completed && !newAssembly && !p.makePendingRoomLocked(key, shardBytes) {
		p.mu.Unlock()
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	if completed && !p.makeCompletedRoomLocked(key, shardBytes) {
		p.mu.Unlock()
		BlockChunkShardDropMeter.Mark(1)
		return chunkRejected
	}
	payload := append([]byte(nil), pkt.Payload...)
	cached := r.cachePacket(pkt, payload)
	r.chunks[pkt.ChunkIndex] = payload
	r.packets[pkt.ChunkIndex] = cached
	r.storedBytes += shardBytes
	if completed {
		p.completedBytes += shardBytes
	}
	if !completed {
		p.pendingBytes += shardBytes
	}
	r.recordSource(source)
	// A node that already has the full block still validates the RS codeword so
	// it cannot relay poisoned parity, but it suppresses duplicate delivery.
	if completed {
		r.completedAt = time.Now()
		p.trimCompletedLocked()
		p.mu.Unlock()
		return chunkAccepted
	}
	if len(r.chunks) < int(r.dataShardCount) || r.reconstructing {
		p.mu.Unlock()
		return chunkAccepted
	}
	r.reconstructing = true
	snapshot := r.snapshot()
	p.mu.Unlock()

	block, codeword, err := reassemble(snapshot)
	p.mu.Lock()
	current := p.pending[key]
	if err != nil {
		if current != nil {
			p.removePendingLocked(key)
		}
		p.failed[key] = time.Now()
		p.trimFailedLocked()
		p.mu.Unlock()
		log.Warn("Failed to reassemble block from shards", "hash", pkt.BlockHash, "root", pkt.ShardRoot, "err", err)
		BlockChunkReassembleErrors.Mark(1)
		return chunkRejected
	}
	if current == nil {
		p.mu.Unlock()
		return chunkRejected
	}
	// Shards may arrive while Reed-Solomon reconstruction runs without the pool
	// lock. Validate those late arrivals against the canonical codeword before
	// exposing the completed encoding through the repair cache.
	if index, ok := canonicalPacketsMatch(current, codeword.hashes); !ok {
		p.removePendingLocked(key)
		p.failed[key] = time.Now()
		p.trimFailedLocked()
		p.mu.Unlock()
		log.Warn("Shard received during reconstruction is outside canonical codeword", "hash", pkt.BlockHash, "root", pkt.ShardRoot, "index", index)
		BlockChunkReassembleErrors.Mark(1)
		return chunkRejected
	}
	current.reconstructing = false
	current.completedAt = time.Now()
	current.canonicalHashes = codeword.hashes
	current.usable = current.skipDelivery
	p.movePendingToCompletedLocked(key, current)
	p.completedBytes += current.cacheReconstructedShards(codeword)
	p.trimCompletedLocked()
	source = current.primarySource
	deliver := !current.skipDelivery
	p.mu.Unlock()

	BlockChunkReassembleTimer.UpdateSince(current.createdAt)
	BlockChunkReassembleMeter.Mark(1)
	if deliver && p.deliver != nil && p.deliver(block, source) {
		p.mu.Lock()
		if cached := p.completed[key]; cached == current {
			cached.usable = true
		}
		p.mu.Unlock()
	}
	return chunkAccepted
}

func validChunkPacket(pkt *BlockChunkPacket) bool {
	if pkt == nil || pkt.Header == nil || pkt.Header.Number == nil || pkt.ChunkCount == 0 {
		return false
	}
	if pkt.ChunkIndex >= pkt.ChunkCount || pkt.DataShardCount == 0 || pkt.ParityShardCount == 0 {
		return false
	}
	if pkt.DataShardCount > maxShardCount || pkt.ParityShardCount > maxShardCount ||
		pkt.DataShardCount > maxShardCount-pkt.ParityShardCount ||
		pkt.ChunkCount != pkt.DataShardCount+pkt.ParityShardCount {
		return false
	}
	if len(pkt.Payload) == 0 || len(pkt.Payload) > chunkSize || len(pkt.RelayTargets) > MaxBlockChunkRelayTargets ||
		pkt.RelayDepth > 1 || (pkt.RelayDepth == 1 && len(pkt.RelayTargets) != 0) {
		return false
	}
	if pkt.OriginalSize == 0 || pkt.OriginalSize > maxBlockShardBytes {
		return false
	}
	capacity := uint64(pkt.DataShardCount) * uint64(len(pkt.Payload))
	if pkt.OriginalSize > capacity || (pkt.DataShardCount > 1 && pkt.OriginalSize <= capacity-uint64(len(pkt.Payload))) {
		return false
	}
	if pkt.BlockHash == (common.Hash{}) || pkt.BlockHash != pkt.Header.Hash() || pkt.Number != pkt.Header.Number.Uint64() {
		return false
	}
	if err := pkt.Header.SanityCheck(); err != nil {
		return false
	}
	if crypto.Keccak256Hash(pkt.Payload) != pkt.PayloadHash {
		return false
	}
	if pkt.ShardRoot != (common.Hash{}) {
		if !verifyShardProof(pkt) {
			return false
		}
	}
	return true
}

func (p *ChunkPool) headerVerified(pkt *BlockChunkPacket) bool {
	if p.validateHeader == nil {
		return true
	}
	p.mu.Lock()
	p.gcLocked()
	_, ok := p.verifiedHeaders[pkt.BlockHash]
	if !ok {
		_, ok = p.deferredHeaders[pkt.BlockHash]
	}
	if ok {
		p.mu.Unlock()
		return true
	}
	if call := p.headerChecks[pkt.BlockHash]; call != nil {
		p.mu.Unlock()
		<-call.done
		return call.ok
	}
	if !p.allowHeaderVerificationLocked(time.Now()) {
		p.mu.Unlock()
		return false
	}
	call := &verificationCall{done: make(chan struct{})}
	p.headerChecks[pkt.BlockHash] = call
	p.mu.Unlock()

	valid := true
	deferred := false
	if err := p.validateHeader(pkt.Header); err != nil {
		if errors.Is(err, ErrDeferredHeaderValidation) {
			deferred = true
		} else {
			valid = false
			log.Debug("Drop shard with invalid header", "hash", pkt.BlockHash, "err", err)
		}
	}
	p.mu.Lock()
	if valid {
		p.gcLocked()
		if deferred {
			p.deferredHeaders[pkt.BlockHash] = time.Now()
			p.trimDeferredHeadersLocked()
		} else {
			delete(p.deferredHeaders, pkt.BlockHash)
			p.verifiedHeaders[pkt.BlockHash] = time.Now()
			p.trimVerifiedHeadersLocked()
		}
	}
	call.ok = valid
	delete(p.headerChecks, pkt.BlockHash)
	close(call.done)
	p.mu.Unlock()
	return valid
}

func (p *ChunkPool) manifestAuthorized(pkt *BlockChunkPacket) bool {
	if pkt.ShardRoot == (common.Hash{}) || pkt.OriginNodeID == (enode.ID{}) || len(pkt.RootSignature) != crypto.SignatureLength {
		return false
	}
	// Origin authorization is a cheap policy lookup. Perform it before the
	// shared signature budget so unauthorized roots cannot starve legitimate
	// block producers.
	if p.validateOrigin != nil {
		if err := p.validateOrigin(pkt.Header, pkt.OriginNodeID); err != nil {
			log.Debug("Drop shard from unauthorized encoding origin", "hash", pkt.BlockHash, "origin", pkt.OriginNodeID, "err", err)
			return false
		}
	}
	signatureHash := crypto.Keccak256Hash(pkt.RootSignature)
	key := manifestKey{
		assemblyKey: assemblyKey{blockHash: pkt.BlockHash, shardRoot: pkt.ShardRoot},
		origin:      pkt.OriginNodeID,
		signature:   signatureHash,
	}
	p.mu.Lock()
	_, ok := p.verifiedManifests[key]
	if ok {
		p.mu.Unlock()
		return true
	}
	if call := p.manifestChecks[key]; call != nil {
		p.mu.Unlock()
		<-call.done
		return call.ok
	}
	if !p.allowManifestVerificationLocked(time.Now()) {
		p.mu.Unlock()
		return false
	}
	call := &verificationCall{done: make(chan struct{})}
	p.manifestChecks[key] = call
	p.mu.Unlock()

	valid := verifyManifestSignature(pkt)
	p.mu.Lock()
	if valid {
		p.gcLocked()
		p.verifiedManifests[key] = time.Now()
		p.trimVerifiedManifestsLocked()
	}
	call.ok = valid
	delete(p.manifestChecks, key)
	close(call.done)
	p.mu.Unlock()
	return valid
}

func newReassembly(pkt *BlockChunkPacket) *Reassembly {
	header := types.CopyHeader(pkt.Header)
	rootSignature := append([]byte(nil), pkt.RootSignature...)
	r := &Reassembly{
		blockHash:        pkt.BlockHash,
		number:           pkt.Number,
		header:           header,
		headerHash:       header.Hash(),
		shardRoot:        pkt.ShardRoot,
		originNodeID:     pkt.OriginNodeID,
		rootSignature:    rootSignature,
		chunkCount:       pkt.ChunkCount,
		dataShardCount:   pkt.DataShardCount,
		parityShardCount: pkt.ParityShardCount,
		originalSize:     pkt.OriginalSize,
		shardSize:        len(pkt.Payload),
		chunks:           make(map[uint][]byte),
		packets:          make(map[uint]*BlockChunkPacket),
		sources:          make(map[string]struct{}),
		repairPeers:      make(map[string]struct{}),
		storedBytes:      retainedManifestSize(pkt),
		createdAt:        time.Now(),
	}
	r.authorizeRepairTargets(pkt.RelayTargets)
	return r
}

func retainedManifestSize(pkt *BlockChunkPacket) uint64 {
	if pkt == nil || pkt.Header == nil {
		return 0
	}
	header := pkt.Header
	bits := 0
	for _, value := range []*big.Int{header.Difficulty, header.Number, header.BaseFee} {
		if value != nil {
			bits += value.BitLen()
		}
	}
	return blockHeaderFixedSize + uint64(len(header.Extra)) + uint64((bits+7)/8) + uint64(len(pkt.RootSignature))
}

func retainedShardSize(pkt *BlockChunkPacket) uint64 {
	if pkt == nil {
		return 0
	}
	return blockChunkPacketFixedSize + uint64(len(pkt.Payload)) + uint64(len(pkt.ShardProof))*uint64(len(common.Hash{}))
}

func (r *Reassembly) cachePacket(pkt *BlockChunkPacket, payload []byte) *BlockChunkPacket {
	cached := *pkt
	cached.Header = r.header
	cached.Payload = payload
	cached.ShardProof = append([]common.Hash(nil), pkt.ShardProof...)
	cached.OriginNodeID = r.originNodeID
	cached.RootSignature = r.rootSignature
	cached.RelayDepth = 1
	cached.RelayTargets = nil
	return &cached
}

// cacheReconstructedShards retains packets that were absent from the received
// set but recovered by Reed-Solomon. Rebuilding the full Merkle tree supplies
// valid proofs, allowing a completed receiver to serve any shard during repair.
func (r *Reassembly) cacheReconstructedShards(codeword *reconstructedEncoding) uint64 {
	if r == nil || codeword == nil || len(codeword.shards) != int(r.chunkCount) || len(codeword.proofs) != int(r.chunkCount) {
		return 0
	}
	var additional uint64
	for i, shard := range codeword.shards {
		index := uint(i)
		if r.packets[index] != nil {
			continue
		}
		payload := append([]byte(nil), shard...)
		pkt := &BlockChunkPacket{
			BlockHash:        r.blockHash,
			Number:           r.number,
			Header:           r.header,
			ChunkIndex:       index,
			ChunkCount:       r.chunkCount,
			DataShardCount:   r.dataShardCount,
			ParityShardCount: r.parityShardCount,
			OriginalSize:     r.originalSize,
			Payload:          payload,
			PayloadHash:      codeword.hashes[index],
			ShardRoot:        r.shardRoot,
			ShardProof:       codeword.proofs[i],
			OriginNodeID:     r.originNodeID,
			RootSignature:    r.rootSignature,
			RelayDepth:       1,
		}
		cached := r.cachePacket(pkt, payload)
		r.chunks[index] = payload
		r.packets[index] = cached
		size := retainedShardSize(pkt)
		r.storedBytes += size
		additional += size
	}
	return additional
}

func canonicalPacketsMatch(r *Reassembly, canonicalHashes map[uint]common.Hash) (uint, bool) {
	if r == nil {
		return 0, false
	}
	for index, packet := range r.packets {
		expected, ok := canonicalHashes[index]
		if !ok || packet == nil || packet.PayloadHash != expected {
			return index, false
		}
	}
	return 0, true
}

func (r *Reassembly) matches(pkt *BlockChunkPacket) bool {
	return r.number == pkt.Number &&
		r.headerHash == pkt.Header.Hash() &&
		r.shardRoot == pkt.ShardRoot &&
		r.chunkCount == pkt.ChunkCount &&
		r.dataShardCount == pkt.DataShardCount &&
		r.parityShardCount == pkt.ParityShardCount &&
		r.originalSize == pkt.OriginalSize &&
		r.shardSize == len(pkt.Payload)
}

func (r *Reassembly) manifestMatches(pkt *BlockChunkPacket) bool {
	return r != nil && pkt != nil && r.matches(pkt) &&
		r.originNodeID == pkt.OriginNodeID &&
		bytes.Equal(r.rootSignature, pkt.RootSignature)
}

func (r *Reassembly) recordSource(source string) {
	if source == "" {
		return
	}
	r.sources[source] = struct{}{}
	if r.primarySource == "" {
		r.primarySource = source
	}
}

func (r *Reassembly) authorizeRepairTargets(targets []enode.ID) {
	if r == nil {
		return
	}
	peerIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		if target != (enode.ID{}) {
			peerIDs = append(peerIDs, target.String())
		}
	}
	r.authorizeRepairPeers(peerIDs)
}

func (r *Reassembly) authorizeRepairPeers(peerIDs []string) bool {
	if r == nil {
		return false
	}
	if r.repairPeers == nil {
		r.repairPeers = make(map[string]struct{})
	}
	additional := make(map[string]struct{}, len(peerIDs))
	for _, id := range peerIDs {
		if id == "" {
			continue
		}
		if _, exists := r.repairPeers[id]; !exists {
			additional[id] = struct{}{}
		}
	}
	if len(r.repairPeers)+len(additional) > maxRepairPeersPerEncoding {
		return false
	}
	for id := range additional {
		r.repairPeers[id] = struct{}{}
	}
	return true
}

func (r *Reassembly) snapshot() *Reassembly {
	copy := *r
	copy.chunks = make(map[uint][]byte, len(r.chunks))
	for index, payload := range r.chunks {
		copy.chunks[index] = append([]byte(nil), payload...)
	}
	return &copy
}

// StoreOutgoing caches an encoding generated locally so relays can request
// dropped shards without forcing another full-block encode.
func (p *ChunkPool) StoreOutgoing(pkts []*BlockChunkPacket) bool {
	if p == nil || !p.config.Enable || len(pkts) == 0 || !validChunkPacket(pkts[0]) ||
		pkts[0].ShardRoot == (common.Hash{}) || !p.manifestAuthorized(pkts[0]) {
		return false
	}
	r := newReassembly(pkts[0])
	for _, pkt := range pkts {
		if !validChunkPacket(pkt) || !r.matches(pkt) || pkt.OriginNodeID != pkts[0].OriginNodeID ||
			!bytes.Equal(pkt.RootSignature, pkts[0].RootSignature) {
			return false
		}
		payload := append([]byte(nil), pkt.Payload...)
		cached := r.cachePacket(pkt, payload)
		r.chunks[pkt.ChunkIndex] = payload
		r.packets[pkt.ChunkIndex] = cached
		r.storedBytes += retainedShardSize(pkt)
	}
	if len(r.chunks) != int(r.chunkCount) {
		return false
	}
	r.completedAt = time.Now()
	r.usable = true
	key := assemblyKey{blockHash: r.blockHash, shardRoot: r.shardRoot}
	p.mu.Lock()
	p.gcLocked()
	delete(p.failed, key)
	p.removePendingLocked(key)
	p.storeCompletedLocked(key, r)
	p.trimCompletedLocked()
	p.mu.Unlock()
	return true
}

// AuthorizeRepairs records authenticated peers selected by the producer for an
// outgoing encoding. Relays learn their authorized repair targets directly from
// the depth-zero route carried by the producer.
func (p *ChunkPool) AuthorizeRepairs(hash, root common.Hash, peerIDs []string) bool {
	if p == nil || len(peerIDs) == 0 {
		return false
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	r := p.pending[key]
	if r == nil {
		r = p.completed[key]
	}
	if r == nil {
		return false
	}
	return r.authorizeRepairPeers(peerIDs)
}

// GetChunks returns cached shards for an authorized repair requester. Returned
// packets are marked as second-level traffic and cannot be relayed further. A
// node-wide egress budget bounds aggregate service across all peer connections.
func (p *ChunkPool) GetChunks(hash, root common.Hash, indexes []uint, peerID string) []*BlockChunkPacket {
	if p == nil || peerID == "" || len(indexes) == 0 || len(indexes) > MaxBlockChunkRequestIndexes {
		return nil
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	r := p.pending[key]
	if r == nil {
		r = p.completed[key]
	}
	if r == nil {
		return nil
	}
	if _, authorized := r.repairPeers[peerID]; !authorized {
		return nil
	}
	result := make([]*BlockChunkPacket, 0, len(indexes))
	seen := make(map[uint]struct{}, len(indexes))
	now := time.Now()
	for _, index := range indexes {
		if index >= r.chunkCount {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		if pkt := r.packets[index]; pkt != nil {
			if !p.allowRepairEgressLocked(now, repairPacketWireSize(pkt)) {
				break
			}
			clone := pkt.Clone()
			clone.RelayDepth = 1
			clone.RelayTargets = nil
			result = append(result, clone)
		}
	}
	return result
}

func repairPacketWireSize(pkt *BlockChunkPacket) uint64 {
	return retainedManifestSize(pkt) + retainedShardSize(pkt)
}

// HasEncoding reports whether an authenticated origin packet has already
// established this encoding locally. Relayed depth-one shards for an unknown
// root are held back until the direct origin seed arrives, which prevents an
// arbitrary public Bsc4 peer from spending verification CPU on new roots.
func (p *ChunkPool) HasEncoding(hash, root common.Hash) bool {
	if p == nil {
		return false
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	return p.pending[key] != nil || p.completed[key] != nil
}

// HasCompletedEncoding reports whether this exact authenticated encoding has
// been fully reconstructed (or cached in full by its producer). Completion is
// intentionally distinct from usability; Bsc5 receipts use HasUsableEncoding.
func (p *ChunkPool) HasCompletedEncoding(hash, root common.Hash) bool {
	if p == nil {
		return false
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	return p.completed[key] != nil
}

// HasUsableEncoding reports whether reconstruction completed and the block is
// either already present locally or was accepted by the normal block fetcher.
// Receipt senders use this stricter state so RS completion alone cannot suppress
// the producer's full-block fallback.
func (p *ChunkPool) HasUsableEncoding(hash, root common.Hash) bool {
	if p == nil {
		return false
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	r := p.completed[key]
	return r != nil && r.usable
}

// ChunkRepairStatus describes the state of an authenticated encoding from the
// repair scheduler's perspective.
type ChunkRepairStatus uint8

const (
	ChunkRepairUnknown ChunkRepairStatus = iota
	ChunkRepairPending
	ChunkRepairReconstructing
	ChunkRepairCompleted
	ChunkRepairUnusable
	ChunkRepairFailed
)

// MissingChunks returns the absent indexes, the minimum number still needed,
// and an explicit status. An absent pending entry is terminal for the current
// repair attempt: the pool evicted, garbage-collected, or rejected the encoding,
// so callers must fall back to ordinary block fetching.
func (p *ChunkPool) MissingChunks(hash, root common.Hash) (missing []uint, needed int, status ChunkRepairStatus) {
	if p == nil {
		return nil, 0, ChunkRepairUnknown
	}
	key := assemblyKey{blockHash: hash, shardRoot: root}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	if _, ok := p.failed[key]; ok {
		return nil, 0, ChunkRepairFailed
	}
	if r := p.completed[key]; r != nil {
		if r.usable {
			return nil, 0, ChunkRepairCompleted
		}
		return nil, 0, ChunkRepairUnusable
	}
	r := p.pending[key]
	if r == nil {
		return nil, 0, ChunkRepairUnknown
	}
	if r.reconstructing {
		return nil, 0, ChunkRepairReconstructing
	}
	needed = int(r.dataShardCount) - len(r.chunks)
	if needed <= 0 {
		return nil, 0, ChunkRepairReconstructing
	}
	missing = make([]uint, 0, r.chunkCount-uint(len(r.chunks)))
	for i := uint(0); i < r.chunkCount; i++ {
		if _, ok := r.chunks[i]; !ok {
			missing = append(missing, i)
		}
	}
	return missing, needed, ChunkRepairPending
}

func (p *ChunkPool) encodingCountLocked(hash common.Hash) int {
	encodings := make(map[assemblyKey]struct{}, maxEncodingsPerBlock)
	for key := range p.pending {
		if key.blockHash == hash {
			encodings[key] = struct{}{}
		}
	}
	for key := range p.completed {
		if key.blockHash == hash {
			encodings[key] = struct{}{}
		}
	}
	for key := range p.failed {
		if key.blockHash == hash {
			encodings[key] = struct{}{}
		}
	}
	return len(encodings)
}

func (p *ChunkPool) makePendingRoomLocked(key assemblyKey, additional uint64) bool {
	newAssembly := p.pending[key] == nil
	for (newAssembly && len(p.pending) >= maxReassemblies) || p.pendingBytes+additional > maxPendingShardBytes {
		var (
			oldestKey assemblyKey
			oldest    time.Time
			found     bool
		)
		for candidate, r := range p.pending {
			if candidate == key || r.reconstructing {
				continue
			}
			if !found || r.createdAt.Before(oldest) {
				oldestKey, oldest, found = candidate, r.createdAt, true
			}
		}
		if !found {
			return false
		}
		p.removePendingLocked(oldestKey)
		p.failed[oldestKey] = time.Now()
		p.trimFailedLocked()
		newAssembly = p.pending[key] == nil
	}
	return true
}

func (p *ChunkPool) makeCompletedRoomLocked(key assemblyKey, additional uint64) bool {
	for p.completedBytes+additional > maxCompletedShardBytes {
		var (
			oldestKey assemblyKey
			oldest    time.Time
			found     bool
		)
		for candidate, r := range p.completed {
			if candidate == key {
				continue
			}
			if !found || r.completedAt.Before(oldest) {
				oldestKey, oldest, found = candidate, r.completedAt, true
			}
		}
		if !found {
			return false
		}
		p.removeCompletedLocked(oldestKey)
	}
	return true
}

func (p *ChunkPool) removePendingLocked(key assemblyKey) *Reassembly {
	r := p.pending[key]
	if r == nil {
		return nil
	}
	delete(p.pending, key)
	if r.storedBytes <= p.pendingBytes {
		p.pendingBytes -= r.storedBytes
	} else {
		p.pendingBytes = 0
	}
	return r
}

func (p *ChunkPool) removeCompletedLocked(key assemblyKey) *Reassembly {
	r := p.completed[key]
	if r == nil {
		return nil
	}
	delete(p.completed, key)
	if r.storedBytes <= p.completedBytes {
		p.completedBytes -= r.storedBytes
	} else {
		p.completedBytes = 0
	}
	return r
}

func (p *ChunkPool) storeCompletedLocked(key assemblyKey, r *Reassembly) {
	if r == nil {
		return
	}
	p.removeCompletedLocked(key)
	if r.completedAt.IsZero() {
		r.completedAt = time.Now()
	}
	p.completed[key] = r
	p.completedBytes += r.storedBytes
}

func (p *ChunkPool) movePendingToCompletedLocked(key assemblyKey, r *Reassembly) {
	if p.pending[key] == r {
		p.removePendingLocked(key)
	}
	p.storeCompletedLocked(key, r)
}

func (p *ChunkPool) trimFailedLocked() {
	for len(p.failed) > maxFailedAssemblies {
		var (
			oldestKey assemblyKey
			oldest    time.Time
		)
		for key, failedAt := range p.failed {
			if oldest.IsZero() || failedAt.Before(oldest) {
				oldestKey, oldest = key, failedAt
			}
		}
		delete(p.failed, oldestKey)
	}
}

func (p *ChunkPool) trimVerifiedHeadersLocked() {
	for len(p.verifiedHeaders) > maxVerifiedHeaders {
		var (
			oldestHash common.Hash
			oldest     time.Time
		)
		for hash, verifiedAt := range p.verifiedHeaders {
			if oldest.IsZero() || verifiedAt.Before(oldest) {
				oldestHash, oldest = hash, verifiedAt
			}
		}
		delete(p.verifiedHeaders, oldestHash)
	}
}

func (p *ChunkPool) trimDeferredHeadersLocked() {
	for len(p.deferredHeaders) > maxVerifiedHeaders {
		var (
			oldestHash common.Hash
			oldest     time.Time
		)
		for hash, deferredAt := range p.deferredHeaders {
			if oldest.IsZero() || deferredAt.Before(oldest) {
				oldestHash, oldest = hash, deferredAt
			}
		}
		delete(p.deferredHeaders, oldestHash)
	}
}

func (p *ChunkPool) trimVerifiedManifestsLocked() {
	for len(p.verifiedManifests) > maxVerifiedManifests {
		var (
			oldestKey manifestKey
			oldest    time.Time
		)
		for key, verifiedAt := range p.verifiedManifests {
			if oldest.IsZero() || verifiedAt.Before(oldest) {
				oldestKey, oldest = key, verifiedAt
			}
		}
		delete(p.verifiedManifests, oldestKey)
	}
}

func (p *ChunkPool) allowHeaderVerificationLocked(now time.Time) bool {
	if p.headerRateAt.IsZero() || now.Sub(p.headerRateAt) >= time.Second {
		p.headerRateAt = now
		p.headerRateCount = 0
	}
	if p.headerRateCount >= maxHeaderVerificationsPerSecond {
		return false
	}
	p.headerRateCount++
	return true
}

func (p *ChunkPool) allowManifestVerificationLocked(now time.Time) bool {
	if p.manifestRateAt.IsZero() || now.Sub(p.manifestRateAt) >= time.Second {
		p.manifestRateAt = now
		p.manifestRateCount = 0
	}
	if p.manifestRateCount >= maxManifestVerificationsPerSecond {
		return false
	}
	p.manifestRateCount++
	return true
}

func (p *ChunkPool) allowRepairEgressLocked(now time.Time, bytes uint64) bool {
	if bytes == 0 || bytes > maxRepairEgressBytesPerSecond {
		return false
	}
	if p.repairEgressAt.IsZero() || now.Sub(p.repairEgressAt) >= time.Second {
		p.repairEgressAt = now
		p.repairEgressBytes = 0
		p.repairEgressCount = 0
	}
	if p.repairEgressCount >= maxRepairEgressShardsPerSecond ||
		p.repairEgressBytes > maxRepairEgressBytesPerSecond-bytes {
		return false
	}
	p.repairEgressCount++
	p.repairEgressBytes += bytes
	return true
}

// refundRepairEgress returns a reservation for a packet that could not enter
// the peer's asynchronous send queue. Reservations from a prior rate window
// have already expired and must not affect the current window.
func (p *ChunkPool) refundRepairEgress(pkt *BlockChunkPacket) {
	if p == nil || pkt == nil {
		return
	}
	bytes := repairPacketWireSize(pkt)
	if bytes == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repairEgressAt.IsZero() || time.Since(p.repairEgressAt) >= time.Second {
		return
	}
	if p.repairEgressCount > 0 {
		p.repairEgressCount--
	}
	if bytes <= p.repairEgressBytes {
		p.repairEgressBytes -= bytes
	} else {
		p.repairEgressBytes = 0
	}
}

func (p *ChunkPool) gcLocked() {
	now := time.Now()
	for key, r := range p.pending {
		if now.Sub(r.createdAt) > maxReassemblyAge {
			p.removePendingLocked(key)
		}
	}
	for key, r := range p.completed {
		if now.Sub(r.completedAt) > completedCacheAge {
			p.removeCompletedLocked(key)
		}
	}
	for key, failedAt := range p.failed {
		if now.Sub(failedAt) > maxReassemblyAge {
			delete(p.failed, key)
		}
	}
	for hash, verifiedAt := range p.verifiedHeaders {
		if now.Sub(verifiedAt) > maxReassemblyAge {
			delete(p.verifiedHeaders, hash)
		}
	}
	for hash, deferredAt := range p.deferredHeaders {
		if now.Sub(deferredAt) > maxReassemblyAge {
			delete(p.deferredHeaders, hash)
		}
	}
	for key, verifiedAt := range p.verifiedManifests {
		if now.Sub(verifiedAt) > maxReassemblyAge {
			delete(p.verifiedManifests, key)
		}
	}
}

func (p *ChunkPool) trimCompletedLocked() {
	for len(p.completed) > maxCompletedAssemblies || p.completedBytes > maxCompletedShardBytes {
		var oldestKey assemblyKey
		var oldest time.Time
		for key, r := range p.completed {
			if oldest.IsZero() || r.completedAt.Before(oldest) {
				oldestKey, oldest = key, r.completedAt
			}
		}
		p.removeCompletedLocked(oldestKey)
	}
}

// SplitBlock serializes a block and creates an authenticated Reed-Solomon
// encoding. The Merkle root covers block hash, encoding metadata, shard index,
// and payload hash.
func SplitBlock(block *types.Block, cfg ChunkConfig) ([]*BlockChunkPacket, error) {
	if block == nil {
		return nil, errors.New("nil block")
	}
	threshold, parityShards, err := resolveChunkConfig(cfg)
	if err != nil {
		return nil, err
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
	if len(enc) < threshold {
		return nil, nil
	}
	dataShards := uint(math.Ceil(float64(len(enc)) / float64(chunkSize)))
	if dataShards == 0 {
		dataShards = 1
	}
	if dataShards > maxShardCount || uint(parityShards) > maxShardCount-dataShards {
		return nil, errors.New("block shard count exceeds limit")
	}
	count := dataShards + uint(parityShards)
	maxBytes := uint64(maxShardCount-parityShards) * chunkSize
	if uint64(len(enc)) > maxBytes {
		return nil, fmt.Errorf("block data %d exceeds chunk capacity %d for %d parity shards", len(enc), maxBytes, parityShards)
	}
	encoder, err := reedsolomon.New(int(dataShards), parityShards)
	if err != nil {
		return nil, err
	}
	shards, err := encoder.Split(enc)
	if err != nil {
		return nil, err
	}
	if err := encoder.Encode(shards); err != nil {
		return nil, err
	}
	hash := block.Hash()
	pkts := make([]*BlockChunkPacket, count)
	leaves := make([]common.Hash, count)
	for i := uint(0); i < count; i++ {
		pkts[i] = &BlockChunkPacket{
			BlockHash:        hash,
			Number:           block.NumberU64(),
			Header:           block.Header(),
			ChunkIndex:       i,
			ChunkCount:       count,
			DataShardCount:   dataShards,
			ParityShardCount: uint(parityShards),
			OriginalSize:     uint64(len(enc)),
			Payload:          shards[i],
			PayloadHash:      crypto.Keccak256Hash(shards[i]),
		}
		leaves[i] = shardLeaf(pkts[i])
	}
	root, proofs := buildShardMerkleTree(leaves)
	for i := range pkts {
		pkts[i].ShardRoot = root
		pkts[i].ShardProof = proofs[i]
	}
	return pkts, nil
}

func shardLeaf(pkt *BlockChunkPacket) common.Hash {
	var metadata [6 * 8]byte
	binary.BigEndian.PutUint64(metadata[0:8], uint64(pkt.ChunkIndex))
	binary.BigEndian.PutUint64(metadata[8:16], uint64(pkt.ChunkCount))
	binary.BigEndian.PutUint64(metadata[16:24], uint64(pkt.DataShardCount))
	binary.BigEndian.PutUint64(metadata[24:32], uint64(pkt.ParityShardCount))
	binary.BigEndian.PutUint64(metadata[32:40], pkt.OriginalSize)
	binary.BigEndian.PutUint64(metadata[40:48], uint64(len(pkt.Payload)))
	return crypto.Keccak256Hash(pkt.BlockHash[:], metadata[:], pkt.PayloadHash[:])
}

func buildShardMerkleTree(leaves []common.Hash) (common.Hash, [][]common.Hash) {
	if len(leaves) == 0 {
		return common.Hash{}, nil
	}
	width := 1
	for width < len(leaves) {
		width <<= 1
	}
	base := make([]common.Hash, width)
	copy(base, leaves)
	for i := len(leaves); i < width; i++ {
		base[i] = leaves[len(leaves)-1]
	}
	levels := [][]common.Hash{base}
	for len(levels[len(levels)-1]) > 1 {
		current := levels[len(levels)-1]
		next := make([]common.Hash, len(current)/2)
		for i := range next {
			next[i] = crypto.Keccak256Hash(current[2*i][:], current[2*i+1][:])
		}
		levels = append(levels, next)
	}
	proofs := make([][]common.Hash, len(leaves))
	for i := range leaves {
		index := i
		proofs[i] = make([]common.Hash, 0, len(levels)-1)
		for level := 0; level < len(levels)-1; level++ {
			proofs[i] = append(proofs[i], levels[level][index^1])
			index >>= 1
		}
	}
	return levels[len(levels)-1][0], proofs
}

func verifyShardProof(pkt *BlockChunkPacket) bool {
	depth := 0
	for width := 1; width < int(pkt.ChunkCount); width <<= 1 {
		depth++
	}
	if len(pkt.ShardProof) != depth {
		return false
	}
	hash := shardLeaf(pkt)
	index := int(pkt.ChunkIndex)
	for _, sibling := range pkt.ShardProof {
		if index&1 == 0 {
			hash = crypto.Keccak256Hash(hash[:], sibling[:])
		} else {
			hash = crypto.Keccak256Hash(sibling[:], hash[:])
		}
		index >>= 1
	}
	return hash == pkt.ShardRoot
}

func manifestDigest(pkt *BlockChunkPacket) common.Hash {
	return crypto.Keccak256Hash(
		[]byte("simplechain-bsc4-block-shards-v1"),
		pkt.BlockHash[:],
		pkt.ShardRoot[:],
		pkt.OriginNodeID[:],
	)
}

// SignShardManifest binds an encoding root to the node that originated it.
// Relay nodes forward the signature unchanged.
func SignShardManifest(pkts []*BlockChunkPacket, origin enode.ID, privateKey *ecdsa.PrivateKey) error {
	if len(pkts) == 0 || privateKey == nil || origin == (enode.ID{}) {
		return errors.New("missing shard manifest signer")
	}
	if enode.PubkeyToIDV4(&privateKey.PublicKey) != origin {
		return errors.New("shard manifest signer does not match origin")
	}
	first := pkts[0]
	if first == nil {
		return errors.New("nil shard manifest packet")
	}
	for _, pkt := range pkts {
		if pkt == nil {
			return errors.New("nil shard manifest packet")
		}
	}
	first.OriginNodeID = origin
	digest := manifestDigest(first)
	signature, err := crypto.Sign(digest[:], privateKey)
	if err != nil {
		return err
	}
	for _, pkt := range pkts {
		if pkt == nil || pkt.BlockHash != first.BlockHash || pkt.ShardRoot != first.ShardRoot {
			return errors.New("inconsistent shard manifest")
		}
		pkt.OriginNodeID = origin
		pkt.RootSignature = append([]byte(nil), signature...)
	}
	return nil
}

func verifyManifestSignature(pkt *BlockChunkPacket) bool {
	if pkt.OriginNodeID == (enode.ID{}) || len(pkt.RootSignature) != crypto.SignatureLength {
		return false
	}
	digest := manifestDigest(pkt)
	publicKey, err := crypto.SigToPub(digest[:], pkt.RootSignature)
	if err != nil {
		return false
	}
	return enode.PubkeyToIDV4(publicKey) == pkt.OriginNodeID
}

func reassemble(r *Reassembly) (*types.Block, *reconstructedEncoding, error) {
	shards := make([][]byte, r.chunkCount)
	for i := uint(0); i < r.chunkCount; i++ {
		shards[i] = r.chunks[i]
	}
	encoder, err := reedsolomon.New(int(r.dataShardCount), int(r.parityShardCount))
	if err != nil {
		return nil, nil, err
	}
	if err := encoder.Reconstruct(shards); err != nil {
		return nil, nil, err
	}
	valid, err := encoder.Verify(shards)
	if err != nil {
		return nil, nil, err
	}
	if !valid {
		return nil, nil, errors.New("reconstructed shards are not a valid Reed-Solomon codeword")
	}
	canonicalHashes := make(map[uint]common.Hash, len(shards))
	leaves := make([]common.Hash, len(shards))
	for i, shard := range shards {
		index := uint(i)
		payloadHash := crypto.Keccak256Hash(shard)
		canonicalHashes[index] = payloadHash
		leaves[i] = shardLeaf(&BlockChunkPacket{
			BlockHash:        r.blockHash,
			ChunkIndex:       index,
			ChunkCount:       r.chunkCount,
			DataShardCount:   r.dataShardCount,
			ParityShardCount: r.parityShardCount,
			OriginalSize:     r.originalSize,
			Payload:          shard,
			PayloadHash:      payloadHash,
		})
	}
	root, proofs := buildShardMerkleTree(leaves)
	if root != r.shardRoot {
		return nil, nil, errors.New("reconstructed codeword does not match shard root")
	}
	codeword := &reconstructedEncoding{shards: shards, hashes: canonicalHashes, proofs: proofs}
	enc := make([]byte, 0, int(r.dataShardCount)*r.shardSize)
	for i := uint(0); i < r.dataShardCount; i++ {
		enc = append(enc, shards[i]...)
	}
	if uint64(len(enc)) < r.originalSize {
		return nil, nil, errors.New("reconstructed data shorter than original size")
	}
	enc = enc[:r.originalSize]
	var body BlockData
	if err := rlp.DecodeBytes(enc, &body); err != nil {
		return nil, nil, err
	}
	if body.Header == nil || body.Header.Hash() != r.headerHash {
		return nil, nil, errors.New("reassembled header mismatch")
	}
	if body.Header.EmptyWithdrawalsHash() && body.Withdrawals == nil {
		body.Withdrawals = make([]*types.Withdrawal, 0)
	}
	block := types.NewBlockWithHeader(body.Header).WithBody(types.Body{
		Transactions: body.Txs,
		Uncles:       body.Uncles,
		Withdrawals:  body.Withdrawals,
	}).WithSidecars(body.Sidecars)
	if block.Hash() != r.blockHash {
		return nil, nil, errors.New("reassembled block hash mismatch")
	}
	if err := validateBlockBody(block); err != nil {
		return nil, nil, err
	}
	return block, codeword, nil
}

func validateBlockBody(block *types.Block) error {
	if types.CalcUncleHash(block.Uncles()) != block.UncleHash() {
		return errors.New("reassembled uncle hash mismatch")
	}
	if types.DeriveSha(block.Transactions(), trie.NewStackTrie(nil)) != block.TxHash() {
		return errors.New("reassembled transaction root mismatch")
	}
	withdrawals := block.Withdrawals()
	if withdrawals == nil {
		if block.Header().WithdrawalsHash != nil {
			return errors.New("reassembled withdrawals missing")
		}
		return nil
	}
	root := types.DeriveSha(types.Withdrawals(withdrawals), trie.NewStackTrie(nil))
	if block.Header().WithdrawalsHash == nil || *block.Header().WithdrawalsHash != root {
		return errors.New("reassembled withdrawals root mismatch")
	}
	return nil
}
