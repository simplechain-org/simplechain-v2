package eth

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/fetcher"
	"github.com/ethereum/go-ethereum/eth/protocols/bsc"
	ethproto "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestChunkFallbackPeerIDs(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		sources []string
		want    []string
	}{
		{name: "origin and distinct source", origin: "origin", sources: []string{"source-b", "source-a"}, want: []string{"origin", "source-a"}},
		{name: "duplicate origin is skipped", origin: "origin", sources: []string{"origin", "origin"}, want: []string{"origin"}},
		{name: "empty origin uses one source", sources: []string{"source-b", "source-a"}, want: []string{"source-a"}},
		{name: "empty entries are skipped", origin: "origin", sources: []string{"", "source"}, want: []string{"origin", "source"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := chunkFallbackPeerIDs(test.origin, test.sources)
			if len(got) != len(test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("got %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestRegisterChunkReceiptsRejectsNilTD(t *testing.T) {
	h := &handler{chunkReceipts: make(map[chunkReceiptKey]*chunkReceiptState)}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	if _, ok := h.registerChunkReceipts(block, nil, common.Hash{1}, map[string]*bsc.Peer{"peer": nil}); ok {
		t.Fatal("nil TD receipt state was accepted")
	}
	if len(h.chunkReceipts) != 0 {
		t.Fatal("nil TD receipt state was registered")
	}
}

func TestChunkReceiptGenerationProtectsReplacement(t *testing.T) {
	key := chunkReceiptKey{hash: common.Hash{1}, root: common.Hash{2}}
	newState := &chunkReceiptState{generation: 2, pending: make(map[string]*bsc.Peer)}
	h := &handler{chunkReceipts: map[chunkReceiptKey]*chunkReceiptState{key: newState}}

	// A timer or fanout cancellation from generation one must not mutate the
	// replacement state registered for the same deterministic encoding root.
	h.runChunkReceiptFallback(key, 1)
	h.cancelChunkReceipts(key.hash, key.root, 1)
	h.chunkReceiptMu.Lock()
	got := h.chunkReceipts[key]
	h.chunkReceiptMu.Unlock()
	if got != newState {
		t.Fatal("stale receipt generation cleared its replacement")
	}
	h.stopChunkReceipts()
}

func TestChunkReceiptRejectsPriorConnection(t *testing.T) {
	nodeID := enode.ID{7}
	localExpected, remoteExpected := p2p.MsgPipe()
	defer localExpected.Close()
	defer remoteExpected.Close()
	expected := bsc.NewPeer(bsc.Bsc5, p2p.NewPeer(nodeID, "expected", nil), remoteExpected)
	defer expected.Close()

	localStale, remoteStale := p2p.MsgPipe()
	defer localStale.Close()
	defer remoteStale.Close()
	stale := bsc.NewPeer(bsc.Bsc5, p2p.NewPeer(nodeID, "stale", nil), remoteStale)
	defer stale.Close()

	receipt := &bsc.BlockChunkReceiptPacket{BlockHash: common.Hash{1}, ShardRoot: common.Hash{2}}
	key := chunkReceiptKey{hash: receipt.BlockHash, root: receipt.ShardRoot}
	state := &chunkReceiptState{
		pending:    map[string]*bsc.Peer{expected.ID(): expected},
		generation: 1,
	}
	h := &handler{chunkReceipts: map[chunkReceiptKey]*chunkReceiptState{key: state}}
	h.acknowledgeChunkReceipt(stale, receipt)
	if _, pending := state.pending[expected.ID()]; !pending {
		t.Fatal("receipt from a prior connection cleared current delivery state")
	}
	h.acknowledgeChunkReceipt(expected, receipt)
	if _, exists := h.chunkReceipts[key]; exists {
		t.Fatal("receipt from selected connection did not clear delivery state")
	}
}

func TestChunkReceiptFallbackRetainsDisconnectedPeer(t *testing.T) {
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	key := chunkReceiptKey{hash: block.Hash(), root: common.Hash{2}}
	state := &chunkReceiptState{
		block:      block,
		td:         big.NewInt(1),
		pending:    map[string]*bsc.Peer{"disconnected": nil},
		generation: 1,
	}
	h := &handler{
		peers:         newPeerSet(),
		chunkReceipts: map[chunkReceiptKey]*chunkReceiptState{key: state},
	}
	h.runChunkReceiptFallback(key, state.generation)

	h.chunkReceiptMu.Lock()
	_, pending := state.pending["disconnected"]
	attempts := state.attempts
	timer := state.timer
	h.chunkReceiptMu.Unlock()
	if !pending || attempts != 1 || timer == nil {
		t.Fatalf("disconnected receipt peer was not retained for retry: pending=%v attempts=%d timer=%v", pending, attempts, timer != nil)
	}
	h.stopChunkReceipts()
}

func TestRelayTargetAssignmentsCoverEverySecondLevelPeer(t *testing.T) {
	for _, peerCount := range []int{4, 16, 64} {
		fanout := 1
		for fanout*fanout < peerCount {
			fanout++
		}
		assignments := relayTargetAssignments(peerCount, fanout, 2)
		seen := make(map[int]int)
		for _, targets := range assignments {
			for _, target := range targets {
				seen[target]++
			}
		}
		for target := fanout; target < peerCount; target++ {
			if seen[target] != min(2, fanout) {
				t.Fatalf("peerCount=%d target=%d assigned %d times", peerCount, target, seen[target])
			}
		}
	}
}

func TestRelayTargetAssignmentsRejectInvalidTopology(t *testing.T) {
	if got := relayTargetAssignments(4, 5, 2); got != nil {
		t.Fatal("invalid fanout was accepted")
	}
	if got := relayTargetAssignments(4, 2, 0); got != nil {
		t.Fatal("zero redundancy was accepted")
	}
}

func TestChunkRepairTimerGenerationAndShutdown(t *testing.T) {
	h := &handler{chunkRepairs: make(map[chunkRepairKey]*chunkRepairState)}
	key := chunkRepairKey{}
	state := &chunkRepairState{sources: make(map[string]struct{})}
	h.chunkRepairs[key] = state

	h.chunkRepairMu.Lock()
	h.scheduleChunkRepairLocked(key, state, time.Hour)
	firstGeneration := state.generation
	h.scheduleChunkRepairLocked(key, state, time.Hour)
	secondGeneration := state.generation
	h.chunkRepairMu.Unlock()
	if secondGeneration <= firstGeneration {
		t.Fatal("repair timer generation did not advance")
	}

	h.clearChunkRepairGeneration(key, firstGeneration)
	h.chunkRepairMu.Lock()
	_, exists := h.chunkRepairs[key]
	h.chunkRepairMu.Unlock()
	if !exists {
		t.Fatal("stale timer generation cleared the active repair")
	}

	h.stopChunkRepairs()
	h.chunkRepairMu.Lock()
	defer h.chunkRepairMu.Unlock()
	if !h.chunkRepairStopped || len(h.chunkRepairs) != 0 {
		t.Fatal("chunk repair timers were not drained on shutdown")
	}
}

func TestChunkRepairUnknownEncodingFallsBackToEthFetcher(t *testing.T) {
	nodeID := enode.ID{1}
	local, remote := p2p.MsgPipe()
	defer local.Close()
	defer remote.Close()
	protocolPeer := ethproto.NewPeer(ethproto.ProtocolVersions[0], p2p.NewPeer(nodeID, "fallback-peer", nil), remote, nil)
	defer protocolPeer.Close()

	peers := newPeerSet()
	peers.peers[protocolPeer.ID()] = &ethPeer{Peer: protocolPeer}
	blockFetcher := fetcher.NewBlockFetcher(
		func(common.Hash) *types.Block { return nil },
		func(*types.Header) error { return nil },
		func(*types.Block, bool) {},
		func() uint64 { return 0 },
		func() uint64 { return 0 },
		func(types.Blocks) (int, error) { return 0, nil },
		func(string) {},
		nil,
	)
	blockFetcher.Start()
	defer blockFetcher.Stop()

	key := chunkRepairKey{hash: common.Hash{1}, root: common.Hash{2}}
	state := &chunkRepairState{
		sources:    map[string]struct{}{protocolPeer.ID(): {}},
		origin:     protocolPeer.ID(),
		number:     1,
		generation: 1,
	}
	h := &handler{
		blockFetcher: blockFetcher,
		peers:        peers,
		chunkPool:    bsc.NewChunkPool(bsc.ChunkConfig{Enable: true}, nil, nil, nil, nil),
		chunkRepairs: map[chunkRepairKey]*chunkRepairState{key: state},
	}

	h.runChunkRepair(key, state.generation)
	type readResult struct {
		msg p2p.Msg
		err error
	}
	result := make(chan readResult, 1)
	go func() {
		msg, err := local.ReadMsg()
		result <- readResult{msg: msg, err: err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("failed to read fallback request: %v", got.err)
		}
		defer got.msg.Discard()
		if got.msg.Code != ethproto.GetBlockHeadersMsg {
			t.Fatalf("fallback message code = %d, want %d", got.msg.Code, ethproto.GetBlockHeadersMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal shard state did not schedule an eth block fetch")
	}

	h.chunkRepairMu.Lock()
	_, exists := h.chunkRepairs[key]
	h.chunkRepairMu.Unlock()
	if exists {
		t.Fatal("terminal shard repair state was not retired")
	}
}

func TestChunkRepairFallbackRetainsDisconnectedSource(t *testing.T) {
	key := chunkRepairKey{hash: common.Hash{1}, root: common.Hash{2}}
	state := &chunkRepairState{
		sources:    map[string]struct{}{"disconnected": {}},
		origin:     "disconnected",
		number:     1,
		generation: 1,
	}
	h := &handler{
		peers:        newPeerSet(),
		chunkPool:    bsc.NewChunkPool(bsc.ChunkConfig{Enable: true}, nil, nil, nil, nil),
		chunkRepairs: map[chunkRepairKey]*chunkRepairState{key: state},
	}

	h.runChunkRepair(key, state.generation)

	h.chunkRepairMu.Lock()
	got := h.chunkRepairs[key]
	fallbackAttempts := state.fallbackAttempts
	timer := state.timer
	h.chunkRepairMu.Unlock()
	if got != state || fallbackAttempts != 1 || timer == nil {
		t.Fatalf("disconnected fallback was not retained for retry: state=%v attempts=%d timer=%v", got == state, fallbackAttempts, timer != nil)
	}
	h.stopChunkRepairs()
}

func TestChunkRepairProgressResetsRetryCounters(t *testing.T) {
	nodeID := enode.ID{1}
	local, remote := p2p.MsgPipe()
	defer local.Close()
	defer remote.Close()
	peer := bsc.NewPeer(bsc.Bsc4, p2p.NewPeer(nodeID, "source", nil), remote)
	defer peer.Close()

	key := chunkRepairKey{hash: common.Hash{1}, root: common.Hash{2}}
	state := &chunkRepairState{
		sources:          make(map[string]struct{}),
		attempts:         chunkRepairMaxAttempts,
		fallbackAttempts: chunkRepairMaxAttempts,
	}
	h := &handler{
		chunkPool:    bsc.NewChunkPool(bsc.ChunkConfig{Enable: true}, nil, nil, nil, nil),
		chunkRepairs: map[chunkRepairKey]*chunkRepairState{key: state},
	}
	h.observeChunkSource(peer, &bsc.BlockChunkPacket{
		BlockHash: key.hash,
		ShardRoot: key.root,
		Number:    1,
	}, true)

	h.chunkRepairMu.Lock()
	attempts := state.attempts
	fallbackAttempts := state.fallbackAttempts
	timer := state.timer
	h.chunkRepairMu.Unlock()
	if attempts != 0 || fallbackAttempts != 0 || timer == nil {
		t.Fatalf("new shard did not reset retry counters: repair=%d fallback=%d timer=%v", attempts, fallbackAttempts, timer != nil)
	}
	h.stopChunkRepairs()
}

func TestApplyValidatorNodesRefreshesPreForkWhitelist(t *testing.T) {
	nodeID := enode.ID{1}
	sender, receiver := p2p.MsgPipe()
	defer sender.Close()
	defer receiver.Close()
	protocolPeer := ethproto.NewPeer(ethproto.ProtocolVersions[0], p2p.NewPeer(nodeID, "test", nil), receiver, nil)
	defer protocolPeer.Close()
	peer := &ethPeer{Peer: protocolPeer}
	peers := newPeerSet()
	peers.peers[peer.ID()] = peer

	h := &handler{
		peers:                  peers,
		enableEVNFeatures:      true,
		evnNodeIdsWhitelistMap: map[enode.ID]struct{}{nodeID: {}},
	}
	h.synced.Store(true)
	h.applyValidatorNodes(nil)
	if !peer.EVNPeerFlag.Load() {
		t.Fatal("pre-fork whitelist peer did not receive EVN features")
	}
}
