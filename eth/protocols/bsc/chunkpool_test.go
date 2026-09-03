package bsc

import (
	"bytes"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func makeChunkTestBlock() *types.Block {
	header := &types.Header{
		Number:     big.NewInt(1),
		ParentHash: common.HexToHash("0x1234"),
		Extra:      bytes.Repeat([]byte{0x42}, 128),
	}
	txs := make([]*types.Transaction, 0, 16)
	for i := 0; i < 16; i++ {
		to := common.BigToAddress(big.NewInt(int64(i + 1)))
		txs = append(txs, types.NewTransaction(uint64(i), to, big.NewInt(1), 21000, big.NewInt(1), bytes.Repeat([]byte{byte(i)}, 4096)))
	}
	return types.NewBlock(header, &types.Body{Transactions: txs}, nil, nil)
}

func signedTestShards(t *testing.T, block *types.Block, parity int) []*BlockChunkPacket {
	t.Helper()
	pkts, err := SplitBlock(block, ChunkConfig{Enable: true, Threshold: 1, ParityShards: parity})
	if err != nil {
		t.Fatalf("SplitBlock error: %v", err)
	}
	signTestShards(t, pkts)
	return pkts
}

func signTestShards(t *testing.T, pkts []*BlockChunkPacket) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate manifest key: %v", err)
	}
	if err := SignShardManifest(pkts, enode.PubkeyToIDV4(&key.PublicKey), key); err != nil {
		t.Fatalf("failed to sign shard manifest: %v", err)
	}
}

func rebuildTestShardManifest(t *testing.T, pkts []*BlockChunkPacket) {
	t.Helper()
	leaves := make([]common.Hash, len(pkts))
	for i, pkt := range pkts {
		leaves[i] = shardLeaf(pkt)
	}
	root, proofs := buildShardMerkleTree(leaves)
	for i, pkt := range pkts {
		pkt.ShardRoot = root
		pkt.ShardProof = proofs[i]
		pkt.RootSignature = nil
		pkt.OriginNodeID = enode.ID{}
	}
	signTestShards(t, pkts)
}

func TestValidateChunkConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  ChunkConfig
		wantErr bool
	}{
		{
			name:   "defaults",
			config: ChunkConfig{},
		},
		{
			name:    "negative threshold",
			config:  ChunkConfig{Threshold: -1},
			wantErr: true,
		},
		{
			name:    "negative parity",
			config:  ChunkConfig{ParityShards: -1},
			wantErr: true,
		},
		{
			name:    "parity exceeds wire limit",
			config:  ChunkConfig{ParityShards: maxShardCount},
			wantErr: true,
		},
		{
			name:    "default threshold cannot fit with 255 parity shards",
			config:  ChunkConfig{ParityShards: maxShardCount - 1},
			wantErr: true,
		},
		{
			name:   "largest encodable threshold with one parity shard",
			config: ChunkConfig{Threshold: int(maxBlockShardBytes), ParityShards: 1},
		},
		{
			name:    "threshold exceeds actual shard capacity",
			config:  ChunkConfig{Threshold: int(maxBlockShardBytes) + 1, ParityShards: 1},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateChunkConfig(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateChunkConfig(%+v) error = %v, want error: %v", test.config, err, test.wantErr)
			}
		})
	}
}

func TestSplitBlockRejectsUnusableChunkConfig(t *testing.T) {
	if _, err := SplitBlock(makeChunkTestBlock(), ChunkConfig{Enable: true, ParityShards: maxShardCount - 1}); err == nil {
		t.Fatal("SplitBlock accepted a parity configuration that cannot encode the default threshold")
	}
}

func TestChunkPoolHasCompletedEncoding(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, func(*types.Block, string) bool { return true }, nil, nil, nil)
	if pool.HasCompletedEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("unknown encoding was reported complete")
	}
	if !pool.AddChunk(pkts[0], "test-peer") {
		t.Fatal("first shard was not accepted")
	}
	if pool.HasCompletedEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("pending encoding was reported complete")
	}
	for _, pkt := range pkts[1:] {
		pool.AddChunk(pkt, "test-peer")
	}
	if !pool.HasCompletedEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("reconstructed encoding was not reported complete")
	}

	outgoing := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if !outgoing.StoreOutgoing(pkts) {
		t.Fatal("failed to cache outgoing encoding")
	}
	if !outgoing.HasCompletedEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("outgoing encoding was not reported complete")
	}
}

func TestChunkPoolMissingChunksReportsRepairStatus(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, func(*types.Block, string) bool { return true }, nil, nil, nil)
	hash, root := pkts[0].BlockHash, pkts[0].ShardRoot
	if _, _, status := pool.MissingChunks(hash, root); status != ChunkRepairUnknown {
		t.Fatalf("unknown encoding status = %d, want unknown", status)
	}

	dataCount := int(pkts[0].DataShardCount)
	if dataCount <= 1 {
		t.Skip("test block produced a single data shard")
	}
	for i := 0; i < dataCount-1; i++ {
		if !pool.AddChunk(pkts[i], "test-peer") {
			t.Fatalf("failed to add shard %d", i)
		}
	}
	if _, needed, status := pool.MissingChunks(hash, root); status != ChunkRepairPending || needed <= 0 {
		t.Fatalf("pending status = %d with needed=%d", status, needed)
	}

	key := assemblyKey{blockHash: hash, shardRoot: root}
	pool.mu.Lock()
	pool.pending[key].reconstructing = true
	pool.mu.Unlock()
	if _, _, status := pool.MissingChunks(hash, root); status != ChunkRepairReconstructing {
		t.Fatalf("reconstructing encoding was not reported as reconstructing")
	}
	pool.mu.Lock()
	pool.pending[key].reconstructing = false
	pool.mu.Unlock()

	for i := dataCount - 1; i < len(pkts); i++ {
		pool.AddChunk(pkts[i], "test-peer")
	}
	if _, _, status := pool.MissingChunks(hash, root); status != ChunkRepairCompleted {
		t.Fatalf("completed encoding status = %d, want completed", status)
	}
}

func TestChunkPoolReassemblesWithMissingDataShard(t *testing.T) {
	block := makeChunkTestBlock()
	pkts := signedTestShards(t, block, 3)

	var delivered *types.Block
	var deliveredBy string
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1, ParityShards: 3}, func(block *types.Block, source string) bool {
		delivered, deliveredBy = block, source
		return true
	}, nil, nil, nil)

	skippedData := false
	var skippedIndex uint
	for _, pkt := range pkts {
		if !skippedData && pkt.ChunkIndex < pkt.DataShardCount {
			skippedData = true
			skippedIndex = pkt.ChunkIndex
			continue
		}
		pool.AddChunk(pkt, "test-peer")
		if delivered != nil {
			break
		}
	}
	if delivered == nil || delivered.Hash() != block.Hash() {
		t.Fatal("expected the original block to be reassembled")
	}
	if deliveredBy != "test-peer" {
		t.Fatalf("unexpected delivery source %q", deliveredBy)
	}
	if !pool.HasUsableEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("accepted reassembled block was not reported usable")
	}
	if !pool.AuthorizeRepairs(pkts[0].BlockHash, pkts[0].ShardRoot, []string{"repair-peer"}) {
		t.Fatal("failed to authorize reconstructed repair cache")
	}
	reconstructed := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, []uint{skippedIndex}, "repair-peer")
	if len(reconstructed) != 1 || reconstructed[0].ChunkIndex != skippedIndex || !verifyShardProof(reconstructed[0]) {
		t.Fatal("reconstructed shard was not retained with a valid Merkle proof")
	}
}

func TestChunkPoolDoesNotConfirmRejectedDelivery(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	deliveries := 0
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, func(*types.Block, string) bool {
		deliveries++
		return false
	}, nil, nil, nil)
	for _, pkt := range pkts {
		pool.AddChunk(pkt, "test-peer")
	}
	hash, root := pkts[0].BlockHash, pkts[0].ShardRoot
	if deliveries != 1 || !pool.HasCompletedEncoding(hash, root) {
		t.Fatal("encoding did not complete before delivery rejection")
	}
	if pool.HasUsableEncoding(hash, root) {
		t.Fatal("rejected block delivery was reported usable")
	}
	if _, _, status := pool.MissingChunks(hash, root); status != ChunkRepairUnusable {
		t.Fatalf("rejected delivery status = %d, want unusable", status)
	}
}

func TestChunkPoolRejectsTamperedShardProof(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	bad := pkts[0].Clone()
	bad.Payload = append([]byte(nil), bad.Payload...)
	bad.Payload[0] ^= 0xff
	bad.PayloadHash = crypto.Keccak256Hash(bad.Payload)

	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if pool.AddChunk(bad, "test-peer") {
		t.Fatal("tampered shard was accepted")
	}
}

func TestChunkPoolRejectsTamperedManifestSignature(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	bad := pkts[0].Clone()
	bad.RootSignature[0] ^= 0xff

	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if pool.AddChunk(bad, "test-peer") {
		t.Fatal("shard with a tampered manifest signature was accepted")
	}
}

func TestChunkPoolRejectsUnauthorizedManifestOrigin(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil,
		func(header *types.Header, origin enode.ID) error { return errors.New("unauthorized") })
	if pool.AddChunk(pkts[0], "test-peer") {
		t.Fatal("unauthorized manifest origin was accepted")
	}
}

func TestChunkPoolRejectsUnauthorizedBeforeVerificationBudgets(t *testing.T) {
	block := makeChunkTestBlock()
	attack := signedTestShards(t, block, 3)
	legitimate := signedTestShards(t, block, 3)
	authorizedOrigin := legitimate[0].OriginNodeID
	headerValidations := 0
	originValidations := 0
	p := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil,
		func(header *types.Header) error {
			headerValidations++
			return nil
		},
		func(header *types.Header, origin enode.ID) error {
			originValidations++
			if origin != authorizedOrigin {
				return errors.New("unauthorized")
			}
			return nil
		})

	for i := 0; i < maxHeaderVerificationsPerSecond+maxManifestVerificationsPerSecond+1; i++ {
		if p.AddChunk(attack[0], "attacker") {
			t.Fatal("unauthorized shard was accepted")
		}
	}
	p.mu.Lock()
	headerBudget, manifestBudget := p.headerRateCount, p.manifestRateCount
	p.mu.Unlock()
	if headerBudget != 0 || manifestBudget != 0 {
		t.Fatalf("unauthorized traffic consumed verification budgets: headers=%d manifests=%d", headerBudget, manifestBudget)
	}
	if originValidations == 0 || headerValidations != 0 {
		t.Fatalf("unexpected validation order: origins=%d headers=%d", originValidations, headerValidations)
	}
	if !p.AddChunk(legitimate[0], "authorized") {
		t.Fatal("legitimate shard was rejected after unauthorized traffic")
	}
	p.mu.Lock()
	headerBudget, manifestBudget = p.headerRateCount, p.manifestRateCount
	p.mu.Unlock()
	if headerBudget != 1 || manifestBudget != 1 {
		t.Fatalf("legitimate validation did not reserve one budget each: headers=%d manifests=%d", headerBudget, manifestBudget)
	}
}

func TestChunkPoolRejectsKnownManifestMutationBeforeBudget(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	p := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil,
		func(*types.Header) error { return nil },
		func(*types.Header, enode.ID) error { return nil })
	if !p.AddChunk(pkts[0], "origin") {
		t.Fatal("failed to establish encoding")
	}
	p.mu.Lock()
	headerBudget, manifestBudget := p.headerRateCount, p.manifestRateCount
	p.mu.Unlock()

	mutated := pkts[1].Clone()
	mutated.RootSignature[0] ^= 0xff
	if p.AddChunk(mutated, "attacker") {
		t.Fatal("known encoding accepted a mutated manifest")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.headerRateCount != headerBudget || p.manifestRateCount != manifestBudget {
		t.Fatalf("known manifest mutation consumed verification budget: headers=%d manifests=%d", p.headerRateCount-headerBudget, p.manifestRateCount-manifestBudget)
	}
}

func TestChunkPoolCoalescesConcurrentHeaderVerification(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	var headerCalls atomic.Int32
	var originCalls atomic.Int32
	headerStarted := make(chan struct{})
	secondAuthorized := make(chan struct{})
	releaseHeader := make(chan struct{})
	p := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil,
		func(*types.Header) error {
			if headerCalls.Add(1) == 1 {
				close(headerStarted)
			}
			<-releaseHeader
			return nil
		},
		func(*types.Header, enode.ID) error {
			if originCalls.Add(1) == 2 {
				close(secondAuthorized)
			}
			return nil
		})

	results := make(chan bool, 2)
	go func() { results <- p.AddChunk(pkts[0], "peer-a") }()
	select {
	case <-headerStarted:
	case <-time.After(time.Second):
		t.Fatal("first header validation did not start")
	}
	go func() { results <- p.AddChunk(pkts[0].Clone(), "peer-b") }()
	select {
	case <-secondAuthorized:
	case <-time.After(time.Second):
		t.Fatal("second shard did not reach origin validation")
	}
	time.Sleep(20 * time.Millisecond)
	if calls := headerCalls.Load(); calls != 1 {
		t.Fatalf("concurrent duplicate started %d header validations", calls)
	}
	close(releaseHeader)
	first, second := <-results, <-results
	if !first && !second {
		t.Fatal("neither concurrent shard established the encoding")
	}
	p.mu.Lock()
	headerBudget := p.headerRateCount
	p.mu.Unlock()
	if headerBudget != 1 {
		t.Fatalf("concurrent duplicate consumed %d header verification budgets", headerBudget)
	}
}

func TestChunkPoolSeparatesEncodingRoots(t *testing.T) {
	block := makeChunkTestBlock()
	first := signedTestShards(t, block, 2)
	second := signedTestShards(t, block, 4)
	if first[0].ShardRoot == second[0].ShardRoot {
		t.Fatal("expected parity settings to produce different roots")
	}

	var delivered *types.Block
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, func(block *types.Block, source string) bool {
		delivered = block
		return true
	}, nil, nil, nil)
	if !pool.AddChunk(first[0], "first-peer") {
		t.Fatal("failed to start first encoding")
	}
	for _, pkt := range second {
		pool.AddChunk(pkt, "second-peer")
		if delivered != nil {
			break
		}
	}
	if delivered == nil || delivered.Hash() != block.Hash() {
		t.Fatal("a partial encoding blocked an independent valid encoding")
	}
}

func TestChunkPoolRepairCache(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if !pool.StoreOutgoing(pkts) {
		t.Fatal("failed to cache outgoing encoding")
	}
	want := []uint{0, pkts[0].ChunkCount - 1}
	if got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, want, "intruder"); len(got) != 0 {
		t.Fatal("unauthorized peer read the repair cache")
	}
	if !pool.AuthorizeRepairs(pkts[0].BlockHash, pkts[0].ShardRoot, []string{"repair-peer"}) {
		t.Fatal("failed to authorize repair peer")
	}
	got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, want, "repair-peer")
	if len(got) != len(want) {
		t.Fatalf("unexpected repair shard count: have %d want %d", len(got), len(want))
	}
	for i, pkt := range got {
		if pkt.ChunkIndex != want[i] || pkt.RelayDepth != 1 || len(pkt.RelayTargets) != 0 {
			t.Fatalf("invalid repair packet at %d", i)
		}
	}
	pool.mu.Lock()
	pool.repairEgressAt = time.Now()
	pool.repairEgressBytes = maxRepairEgressBytesPerSecond
	pool.repairEgressCount = 0
	pool.mu.Unlock()
	if got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, []uint{0}, "repair-peer"); len(got) != 0 {
		t.Fatal("node-wide repair egress budget was bypassed")
	}
}

func TestChunkPoolAuthorizesSignedRelayTargets(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	target := enode.ID{9}
	pkts[0].RelayTargets = []enode.ID{target}
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if !pool.AddChunk(pkts[0], "origin") {
		t.Fatal("failed to establish routed encoding")
	}
	got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, []uint{pkts[0].ChunkIndex}, target.String())
	if len(got) != 1 || got[0].ChunkIndex != pkts[0].ChunkIndex {
		t.Fatal("producer-selected relay target was not authorized for repair")
	}
}

func TestChunkPoolBoundsRepairAuthorization(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if !pool.StoreOutgoing(pkts) {
		t.Fatal("failed to cache outgoing encoding")
	}
	peerIDs := make([]string, maxRepairPeersPerEncoding+1)
	for i := range peerIDs {
		peerIDs[i] = common.BigToHash(big.NewInt(int64(i + 1))).Hex()
	}
	if pool.AuthorizeRepairs(pkts[0].BlockHash, pkts[0].ShardRoot, peerIDs) {
		t.Fatal("oversized repair allowlist was accepted")
	}
	if got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, []uint{0}, peerIDs[0]); len(got) != 0 {
		t.Fatal("rejected repair allowlist was partially applied")
	}
}

func TestChunkPoolCachesCompleteCodewordAndHandlesLateParity(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	deliveries := 0
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, func(block *types.Block, source string) bool {
		deliveries++
		return true
	}, nil, nil, nil)
	for i, pkt := range pkts {
		accepted := pool.AddChunk(pkt, "test-peer")
		if i < int(pkt.DataShardCount) && !accepted {
			t.Fatalf("data shard %d was not accepted", pkt.ChunkIndex)
		}
	}
	if deliveries != 1 {
		t.Fatalf("unexpected delivery count %d", deliveries)
	}
	indexes := make([]uint, len(pkts))
	for i := range indexes {
		indexes[i] = uint(i)
	}
	if !pool.AuthorizeRepairs(pkts[0].BlockHash, pkts[0].ShardRoot, []string{"test-peer"}) {
		t.Fatal("failed to authorize repair peer")
	}
	if got := pool.GetChunks(pkts[0].BlockHash, pkts[0].ShardRoot, indexes, "test-peer"); len(got) != len(pkts) {
		t.Fatalf("late parity was not retained: have %d want %d", len(got), len(pkts))
	}
}

func TestChunkPoolRejectsLateParityOutsideCanonicalCodeword(t *testing.T) {
	pkts, err := SplitBlock(makeChunkTestBlock(), ChunkConfig{Enable: true, Threshold: 1, ParityShards: 3})
	if err != nil {
		t.Fatal(err)
	}
	badParity := pkts[len(pkts)-1]
	badParity.Payload = append([]byte(nil), badParity.Payload...)
	badParity.Payload[0] ^= 0xff
	badParity.PayloadHash = crypto.Keccak256Hash(badParity.Payload)
	rebuildTestShardManifest(t, pkts)

	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	for i := uint(0); i < pkts[0].DataShardCount; i++ {
		if !pool.AddChunk(pkts[i], "test-peer") {
			t.Fatalf("data shard %d was not accepted", i)
		}
	}
	if pool.AddChunk(badParity, "test-peer") {
		t.Fatal("parity outside the reconstructed codeword was accepted")
	}
	if pool.HasEncoding(pkts[0].BlockHash, pkts[0].ShardRoot) {
		t.Fatal("poisoned encoding remained available for repair")
	}
}

func TestChunkPoolTombstonesFailedEncoding(t *testing.T) {
	pkts, err := SplitBlock(makeChunkTestBlock(), ChunkConfig{Enable: true, Threshold: 1, ParityShards: 3})
	if err != nil {
		t.Fatal(err)
	}
	pkts[0].Payload = append([]byte(nil), pkts[0].Payload...)
	pkts[0].Payload[0] ^= 0xff
	pkts[0].PayloadHash = crypto.Keccak256Hash(pkts[0].Payload)
	rebuildTestShardManifest(t, pkts)

	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	for i := uint(0); i < pkts[0].DataShardCount; i++ {
		pool.AddChunk(pkts[i], "test-peer")
	}
	key := assemblyKey{blockHash: pkts[0].BlockHash, shardRoot: pkts[0].ShardRoot}
	pool.mu.Lock()
	_, failed := pool.failed[key]
	pool.mu.Unlock()
	if !failed {
		t.Fatal("failed encoding was not tombstoned")
	}
	if _, _, status := pool.MissingChunks(pkts[0].BlockHash, pkts[0].ShardRoot); status != ChunkRepairFailed {
		t.Fatalf("failed encoding status = %d, want failed", status)
	}
	if pool.AddChunk(pkts[0], "test-peer") {
		t.Fatal("failed encoding was admitted again")
	}
}

func TestChunkPoolCachesDeferredHeaderSeparately(t *testing.T) {
	pkts := signedTestShards(t, makeChunkTestBlock(), 3)
	validations := 0
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil,
		func(header *types.Header) error {
			validations++
			return ErrDeferredHeaderValidation
		}, nil)
	for _, pkt := range pkts {
		pool.AddChunk(pkt, "test-peer")
	}
	if validations != 1 {
		t.Fatalf("deferred header was validated %d times", validations)
	}
	if _, ok := pool.verifiedHeaders[pkts[0].BlockHash]; ok {
		t.Fatal("deferred header was cached as fully verified")
	}
}

func TestSignShardManifestRejectsNilPacket(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := SignShardManifest([]*BlockChunkPacket{nil}, enode.PubkeyToIDV4(&key.PublicKey), key); err == nil {
		t.Fatal("nil manifest packet was accepted")
	}
}

func TestValidateBlockBodyRejectsWrongTransactions(t *testing.T) {
	block := makeChunkTestBlock()
	bad := types.NewBlockWithHeader(block.Header()).WithBody(types.Body{
		Transactions: []*types.Transaction{},
		Uncles:       block.Uncles(),
		Withdrawals:  block.Withdrawals(),
	})
	if err := validateBlockBody(bad); err == nil {
		t.Fatal("block with mismatched transaction root was accepted")
	}
}

func TestCanonicalPacketsRejectShardAddedDuringReconstruction(t *testing.T) {
	good := crypto.Keccak256Hash([]byte("good"))
	bad := crypto.Keccak256Hash([]byte("bad"))
	r := &Reassembly{packets: map[uint]*BlockChunkPacket{
		0: {ChunkIndex: 0, PayloadHash: good},
		1: {ChunkIndex: 1, PayloadHash: bad},
	}}
	if index, ok := canonicalPacketsMatch(r, map[uint]common.Hash{0: good, 1: good}); ok || index != 1 {
		t.Fatalf("late non-canonical shard was accepted: index=%d ok=%v", index, ok)
	}
}

func TestReassemblyAccountsAndSharesManifestStorage(t *testing.T) {
	base := makeChunkTestBlock()
	header := base.Header()
	header.Difficulty = big.NewInt(1)
	header.Extra = bytes.Repeat([]byte{0x44}, 100000)
	block := types.NewBlock(header, &types.Body{Transactions: base.Transactions()}, nil, nil)
	pkts := signedTestShards(t, block, 2)
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1}, nil, nil, nil, nil)
	if !pool.AddChunk(pkts[0], "peer-a") || !pool.AddChunk(pkts[1], "peer-a") {
		t.Fatal("valid shards were rejected")
	}
	key := assemblyKey{blockHash: pkts[0].BlockHash, shardRoot: pkts[0].ShardRoot}
	pool.mu.Lock()
	r := pool.pending[key]
	if r == nil {
		pool.mu.Unlock()
		t.Fatal("pending reassembly not found")
	}
	first, second := r.packets[0], r.packets[1]
	wantMinimum := retainedManifestSize(pkts[0]) + retainedShardSize(pkts[0]) + retainedShardSize(pkts[1])
	stored, pending := r.storedBytes, pool.pendingBytes
	pool.mu.Unlock()
	if first.Header != r.header || second.Header != r.header {
		t.Fatal("shards retained duplicate header objects")
	}
	if len(first.RootSignature) == 0 || &first.RootSignature[0] != &second.RootSignature[0] {
		t.Fatal("shards retained duplicate manifest signatures")
	}
	if stored < wantMinimum || pending < wantMinimum {
		t.Fatalf("retained memory was under-accounted: stored=%d pending=%d want>=%d", stored, pending, wantMinimum)
	}
}
