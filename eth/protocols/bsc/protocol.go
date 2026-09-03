package bsc

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

// Constants to match up protocol versions and messages
const (
	Bsc1 = 1
	Bsc2 = 2
	Bsc3 = 3 // legacy block chunk propagation
	Bsc4 = 4 // authenticated block shards and explicit two-level routing
	Bsc5 = 5 // authenticated shard receipts and producer-side fallback
)

// ProtocolName is the official short name of the `bsc` protocol used during
// devp2p capability negotiation.
const ProtocolName = "bsc"

// ProtocolVersions are the versions advertised by this node. Bsc3 is kept as
// a decoder constant for historical fixtures, but is deliberately not
// advertised: its chunk wire format is unauthenticated and Bsc4 nodes must
// make old peers negotiate Bsc2 so they use the full-block path.
var ProtocolVersions = []uint{Bsc1, Bsc2, Bsc4, Bsc5}

// protocolLengths are the number of implemented messages corresponding to
// different protocol versions.
var protocolLengths = map[uint]uint64{Bsc1: 2, Bsc2: 4, Bsc3: 6, Bsc4: 6, Bsc5: 7}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 10 * 1024 * 1024

const (
	MaxBlockChunkRelayTargets   = 64
	MaxBlockChunkRequestIndexes = 256
	// Chunk messages contain one 64 KiB shard plus a bounded header, proof and
	// route. Keep them below the generic 10 MiB bsc cap before RLP decoding.
	maxBlockChunkMessageSize = 256 * 1024
	maxBlockChunkRequestSize = 16 * 1024
	maxBlockChunkReceiptSize = 4 * 1024
)

const (
	BscCapMsg            = 0x00 // bsc capability msg used upon handshake
	VotesMsg             = 0x01
	GetBlocksByRangeMsg  = 0x02 // it can request (StartBlockHeight-Count, StartBlockHeight] range blocks from remote peer
	BlocksByRangeMsg     = 0x03 // the replied blocks from remote peer
	BlockChunkMsg        = 0x04 // a single chunk of a sharded block (Bsc4)
	GetBlockChunksMsg    = 0x05 // request missing chunks of a sharded block (Bsc4)
	BlockChunkReceiptMsg = 0x06 // receiver completed an authenticated shard encoding (Bsc5)
)

var defaultExtra = []byte{0x00}

var (
	errNoBscCapMsg             = errors.New("no bsc capability message")
	errMsgTooLarge             = errors.New("message too long")
	errDecode                  = errors.New("invalid message")
	errInvalidMsgCode          = errors.New("invalid message code")
	errProtocolVersionMismatch = errors.New("protocol version mismatch")
)

// Packet represents a p2p message in the `bsc` protocol.
type Packet interface {
	Name() string // Name returns a string corresponding to the message type.
	Kind() byte   // Kind returns the message type.
}

// BscCapPacket is the network packet for bsc capability message.
type BscCapPacket struct {
	ProtocolVersion uint
	Extra           rlp.RawValue // for extension
}

// VotesPacket is the network packet for votes record.
type VotesPacket struct {
	Votes []*types.VoteEnvelope
}

func (*BscCapPacket) Name() string { return "BscCap" }
func (*BscCapPacket) Kind() byte   { return BscCapMsg }

func (*VotesPacket) Name() string { return "Votes" }
func (*VotesPacket) Kind() byte   { return VotesMsg }

type GetBlocksByRangePacket struct {
	RequestId        uint64
	StartBlockHeight uint64      // The start block height expected to be obtained from
	StartBlockHash   common.Hash // The start block hash expected to be obtained from
	Count            uint64      // Get the number of blocks from the start
}

func (*GetBlocksByRangePacket) Name() string { return "GetBlocksByRange" }
func (*GetBlocksByRangePacket) Kind() byte   { return GetBlocksByRangeMsg }

// BlockData contains types.extblock + sidecars
type BlockData struct {
	Header      *types.Header
	Txs         []*types.Transaction
	Uncles      []*types.Header
	Withdrawals []*types.Withdrawal `rlp:"optional"`
	Sidecars    types.BlobSidecars  `rlp:"optional"`
}

// NewBlockData creates a new BlockData object from a block
func NewBlockData(block *types.Block) *BlockData {
	return &BlockData{
		Header:      block.Header(),
		Txs:         block.Transactions(),
		Uncles:      block.Uncles(),
		Withdrawals: block.Withdrawals(),
		Sidecars:    block.Sidecars(),
	}
}

type BlocksByRangePacket struct {
	RequestId uint64
	Blocks    []*BlockData
}

func (*BlocksByRangePacket) Name() string { return "BlocksByRange" }
func (*BlocksByRangePacket) Kind() byte   { return BlocksByRangeMsg }

// BlockChunkPacket is a single Reed-Solomon shard in the Bsc4 propagation
// protocol. ShardRoot and ShardProof bind the payload and encoding metadata to
// one encoding batch. RelayTargets is populated only on leader-to-relay traffic
// and RelayDepth prevents packets from being forwarded beyond the second level.
type BlockChunkPacket struct {
	BlockHash        common.Hash // hash of the full block this shard belongs to
	Number           uint64      // block number, used for quick filtering
	Header           *types.Header
	ChunkIndex       uint   // 0-based shard index
	ChunkCount       uint   // total shard count, data + parity
	DataShardCount   uint   // number of shards required to reconstruct
	ParityShardCount uint   // number of parity shards
	OriginalSize     uint64 // original RLP-encoded block data size
	Payload          []byte // Reed-Solomon shard payload
	PayloadHash      common.Hash
	ShardRoot        common.Hash
	ShardProof       []common.Hash
	OriginNodeID     enode.ID
	RootSignature    []byte
	RelayDepth       uint8
	RelayTargets     []enode.ID
}

func (*BlockChunkPacket) Name() string { return "BlockChunk" }
func (*BlockChunkPacket) Kind() byte   { return BlockChunkMsg }

// Clone returns a packet copy whose slice fields can be changed independently.
// The payload itself is immutable and intentionally shared between copies.
func (p *BlockChunkPacket) Clone() *BlockChunkPacket {
	if p == nil {
		return nil
	}
	clone := *p
	clone.ShardProof = append([]common.Hash(nil), p.ShardProof...)
	clone.RootSignature = append([]byte(nil), p.RootSignature...)
	clone.RelayTargets = append([]enode.ID(nil), p.RelayTargets...)
	return &clone
}

// GetBlockChunksPacket requests the missing chunks of a sharded block from a
// peer that is known to have (or is expected to have) them.
type GetBlockChunksPacket struct {
	BlockHash      common.Hash
	ShardRoot      common.Hash
	MissingIndexes []uint // 0-based indexes of the chunks the sender still needs
}

func (*GetBlockChunksPacket) Name() string { return "GetBlockChunks" }
func (*GetBlockChunksPacket) Kind() byte   { return GetBlockChunksMsg }

// BlockChunkReceiptPacket confirms that a peer accepted the reconstructed block
// identified by BlockHash and ShardRoot into its normal processing path. It is
// available only in Bsc5 and lets the producer avoid an unnecessary full-block
// fallback.
type BlockChunkReceiptPacket struct {
	BlockHash common.Hash
	ShardRoot common.Hash
}

func (*BlockChunkReceiptPacket) Name() string { return "BlockChunkReceipt" }
func (*BlockChunkReceiptPacket) Kind() byte   { return BlockChunkReceiptMsg }

// blockChunkPacketV3 preserves the exact Bsc3 wire layout. Bsc4 nodes decode
// it for rolling-upgrade compatibility but never originate Bsc3 chunk traffic.
type blockChunkPacketV3 struct {
	BlockHash        common.Hash
	Number           uint64
	Header           *types.Header
	ChunkIndex       uint
	ChunkCount       uint
	DataShardCount   uint
	ParityShardCount uint
	OriginalSize     uint64
	Payload          []byte
	PayloadHash      common.Hash
}

func (p *blockChunkPacketV3) toBlockChunkPacket() *BlockChunkPacket {
	return &BlockChunkPacket{
		BlockHash:        p.BlockHash,
		Number:           p.Number,
		Header:           p.Header,
		ChunkIndex:       p.ChunkIndex,
		ChunkCount:       p.ChunkCount,
		DataShardCount:   p.DataShardCount,
		ParityShardCount: p.ParityShardCount,
		OriginalSize:     p.OriginalSize,
		Payload:          p.Payload,
		PayloadHash:      p.PayloadHash,
	}
}

type getBlockChunksPacketV3 struct {
	BlockHash      common.Hash
	MissingIndexes []uint
}
