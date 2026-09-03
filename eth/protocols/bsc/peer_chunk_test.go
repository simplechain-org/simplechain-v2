package bsc

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestAsyncSendBlockChunkReportsFullQueue(t *testing.T) {
	peer := &Peer{
		version:        Bsc4,
		chunkBroadcast: make(chan *BlockChunkPacket, 1),
		knownChunks:    newKnownChunkCache(8),
		term:           make(chan struct{}),
		logger:         log.New(),
	}
	first := &BlockChunkPacket{BlockHash: common.HexToHash("0x01"), ShardRoot: common.HexToHash("0x02"), ChunkIndex: 0}
	second := &BlockChunkPacket{BlockHash: first.BlockHash, ShardRoot: first.ShardRoot, ChunkIndex: 1}
	if !peer.AsyncSendBlockChunk(first) {
		t.Fatal("first shard was not queued")
	}
	if peer.AsyncSendBlockChunk(second) {
		t.Fatal("full queue was reported as successful")
	}
}

func TestExplicitRepairBypassesKnownChunkCache(t *testing.T) {
	peer := &Peer{
		version:        Bsc4,
		chunkBroadcast: make(chan *BlockChunkPacket, 2),
		knownChunks:    newKnownChunkCache(8),
		term:           make(chan struct{}),
		logger:         log.New(),
	}
	pkt := &BlockChunkPacket{BlockHash: common.HexToHash("0x01"), ShardRoot: common.HexToHash("0x02"), ChunkIndex: 0}
	if !peer.AsyncSendBlockChunk(pkt) || !peer.AsyncSendBlockChunkForce(pkt) {
		t.Fatal("explicit repair did not requeue a known shard")
	}
	if len(peer.chunkBroadcast) != 2 {
		t.Fatalf("unexpected queue length %d", len(peer.chunkBroadcast))
	}
}

func TestChunkKnownCacheIncludesRouteEnvelope(t *testing.T) {
	base := &BlockChunkPacket{
		BlockHash:    common.HexToHash("0x01"),
		ShardRoot:    common.HexToHash("0x02"),
		ChunkIndex:   0,
		RelayDepth:   0,
		RelayTargets: []enode.ID{{1}},
	}
	changed := base.Clone()
	changed.RelayTargets = []enode.ID{{2}}
	if makeChunkKey(base) == makeChunkKey(changed) {
		t.Fatal("different relay routes shared a known-cache key")
	}
	changed = base.Clone()
	changed.RelayDepth = 1
	changed.RelayTargets = nil
	if makeChunkKey(base) == makeChunkKey(changed) {
		t.Fatal("different relay depths shared a known-cache key")
	}
}

func TestClosedPeerRejectsChunksAndCloseIsIdempotent(t *testing.T) {
	peer := &Peer{
		version:        Bsc4,
		chunkBroadcast: make(chan *BlockChunkPacket, 1),
		knownChunks:    newKnownChunkCache(8),
		term:           make(chan struct{}),
		logger:         log.New(),
	}
	peer.Close()
	peer.Close()
	if peer.AsyncSendBlockChunk(&BlockChunkPacket{BlockHash: common.HexToHash("0x01")}) {
		t.Fatal("closed peer accepted a block shard")
	}
}

func TestRequestMissingChunksUsesBoundedQueue(t *testing.T) {
	peer := &Peer{
		version:       Bsc4,
		chunkRequests: make(chan *GetBlockChunksPacket, 1),
		term:          make(chan struct{}),
		logger:        log.New(),
	}
	hash := common.HexToHash("0x01")
	root := common.HexToHash("0x02")
	indexes := []uint{1, 2}
	if err := peer.RequestMissingChunks(hash, root, indexes); err != nil {
		t.Fatalf("first request was not queued: %v", err)
	}
	indexes[0] = 99
	queued := <-peer.chunkRequests
	if queued.MissingIndexes[0] != 1 {
		t.Fatal("queued request retained the caller's mutable index slice")
	}
	peer.chunkRequests <- queued
	if err := peer.RequestMissingChunks(hash, root, []uint{3}); err == nil {
		t.Fatal("full repair request queue was reported as successful")
	}
}

func TestAsyncSendBlockChunkReceiptIsBsc5OnlyAndDeduplicated(t *testing.T) {
	hash := common.HexToHash("0x01")
	root := common.HexToHash("0x02")
	legacy := &Peer{
		version: Bsc4,
		term:    make(chan struct{}),
		logger:  log.New(),
	}
	if legacy.AsyncSendBlockChunkReceipt(hash, root) {
		t.Fatal("Bsc4 peer accepted a Bsc5 receipt")
	}

	peer := &Peer{
		version:       Bsc5,
		chunkReceipts: make(chan *BlockChunkReceiptPacket, 1),
		knownReceipts: newKnownChunkReceiptCache(8),
		term:          make(chan struct{}),
		logger:        log.New(),
	}
	if !peer.AsyncSendBlockChunkReceipt(hash, root) {
		t.Fatal("Bsc5 peer did not queue a receipt")
	}
	if !peer.AsyncSendBlockChunkReceipt(hash, root) {
		t.Fatal("duplicate receipt was not treated as already queued")
	}
	if got := len(peer.chunkReceipts); got != 1 {
		t.Fatalf("duplicate receipt filled the queue: have %d want 1", got)
	}
	queued := <-peer.chunkReceipts
	if queued.BlockHash != hash || queued.ShardRoot != root {
		t.Fatalf("unexpected queued receipt: %#v", queued)
	}
	peer.Close()
	if peer.AsyncSendBlockChunkReceipt(hash, common.HexToHash("0x03")) {
		t.Fatal("closed peer accepted a receipt")
	}
}
