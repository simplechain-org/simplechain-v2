package bsc

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestChunkPoolReassemblesWithMissingDataShard(t *testing.T) {
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
	block := types.NewBlock(header, &types.Body{Transactions: txs}, nil, nil)

	pkts, err := SplitBlock(block, ChunkConfig{Enable: true, Threshold: 1, ParityShards: 3})
	if err != nil {
		t.Fatalf("SplitBlock error: %v", err)
	}
	if len(pkts) == 0 {
		t.Fatal("expected shards")
	}

	var delivered *types.Block
	pool := NewChunkPool(ChunkConfig{Enable: true, Threshold: 1, ParityShards: 3}, func(block *types.Block) {
		delivered = block
	}, nil)

	skippedData := false
	for _, pkt := range pkts {
		if !skippedData && pkt.ChunkIndex < pkt.DataShardCount {
			skippedData = true
			continue
		}
		pool.AddChunk(pkt)
		if delivered != nil {
			break
		}
	}
	if !skippedData {
		t.Fatal("test did not skip a data shard")
	}
	if delivered == nil {
		t.Fatal("expected reassembled block")
	}
	if delivered.Hash() != block.Hash() {
		t.Fatalf("reassembled hash mismatch, want %s got %s", block.Hash(), delivered.Hash())
	}
}
