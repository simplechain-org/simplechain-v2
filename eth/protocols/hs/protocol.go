package hs

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// Protocol versions and parameters for HotStuff subprotocol over devp2p
const (
	HS1 = 1
)

// ProtocolName is the advertised devp2p capability name
const ProtocolName = "hs"

// ProtocolVersions supported (first is primary)
var ProtocolVersions = []uint{HS1}

// protocolLengths maps version to number of implemented messages
var protocolLengths = map[uint]uint64{HS1: 6}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 10 * 1024 * 1024

// HotStuff message codes
const (
	CapMsg      = 0x00 // capability handshake
	ProposalMsg = 0x01
	VoteMsg     = 0x02
	NewViewMsg  = 0x03
	TimeoutMsg  = 0x04
	QCMsg       = 0x05
)

var (
	errNoCap                   = errors.New("no hs capability message")
	errMsgTooLarge             = errors.New("message too long")
	errDecode                  = errors.New("invalid message")
	errInvalidMsgCode          = errors.New("invalid message code")
	errProtocolVersionMismatch = errors.New("protocol version mismatch")
)

// Packet represents a p2p message in the `hs` protocol.
type Packet interface {
	Name() string
	Kind() byte
}

// CapPacket is the network packet for capability message.
type CapPacket struct {
	ProtocolVersion uint
	Extra           rlp.RawValue
}

func (*CapPacket) Name() string { return "CapPacket" }
func (*CapPacket) Kind() byte   { return CapMsg }

// ProposalPacket carries a proposed block header/bytes and embedded sync info.
type ProposalPacket struct {
	ParentHash common.Hash
	BlockHash  common.Hash
	HighQC     HsQC
	View       uint64
	HeaderRLP  rlp.RawValue // RLP encoded header for verification
	BodyRLP    rlp.RawValue // RLP encoded block body (transactions)
}

// HsQC is the minimal QC payload carried inside Proposal
type HsQC struct {
	BlockHash  common.Hash
	View       uint64
	SignersSet types.ValidatorsBitSet
	Sig        []byte
}

func (*ProposalPacket) Name() string { return "ProposalPacket" }
func (*ProposalPacket) Kind() byte   { return ProposalMsg }

// VotePacket wraps a BLS-signed vote for a specific proposal/view.
type VotePacket struct {
	BlockHash  common.Hash
	ViewNumber uint64
	VotePubKey types.BLSPublicKey
	Signature  types.BLSSignature // BLS signature over rlpHash(BlockHash, ViewNumber)
}

func (*VotePacket) Name() string { return "VotePacket" }
func (*VotePacket) Kind() byte   { return VoteMsg }

// NewViewPacket notifies leader of the highest QC the replica knows.
type NewViewPacket struct {
	HighQCView        uint64
	HighQCHash        common.Hash
	HighTCView        uint64
	TimeoutSignersSet types.ValidatorsBitSet
	TimeoutAggSig     types.BLSSignature
}

func (*NewViewPacket) Name() string { return "NewViewPacket" }
func (*NewViewPacket) Kind() byte   { return NewViewMsg }

// TimeoutPacket carries timeout proof for a view.
type TimeoutPacket struct {
	ViewNumber uint64
	HighQCView uint64
	HighQCHash common.Hash
	VotePubKey types.BLSPublicKey
	Signature  types.BLSSignature
}

func (*TimeoutPacket) Name() string { return "TimeoutPacket" }
func (*TimeoutPacket) Kind() byte   { return TimeoutMsg }

// QuorumCertPacket transports a QC/TC when needed (e.g., recovery/new leader sync).
type QuorumCertPacket struct {
	TargetHash   common.Hash
	ViewNumber   uint64
	SignersSet   types.ValidatorsBitSet
	AggregateSig []byte
}

func (*QuorumCertPacket) Name() string { return "QuorumCertPacket" }
func (*QuorumCertPacket) Kind() byte   { return QCMsg }
