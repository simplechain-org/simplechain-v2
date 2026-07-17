package hotstuff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/holiman/uint256"
	"github.com/willf/bitset"
	"golang.org/x/crypto/sha3"

	"encoding/binary"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gopool"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

const (
	inMemorySnapshots  = 1280  // Number of recent snapshots to keep in memory; a buffer exceeding the EpochLength
	inMemorySignatures = 4096  // Number of recent block signatures to keep in memory
	inMemoryHeaders    = 86400 // Number of recent headers to keep in memory for double sign detection,

	checkpointInterval = 1024 // Number of blocks after which to save the snapshot to the database

	defaultEpochLength   uint64 = 200   // Default number of blocks of checkpoint to update validatorSet from contract
	defaultBlockInterval uint64 = 10000 // Default block interval in milliseconds (10 seconds)

	extraVanity      = 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal        = 65 // Fixed number of extra-data suffix bytes reserved for signer seal
	nextForkHashSize = 4  // Fixed number of extra-data suffix bytes reserved for nextForkHash.

	validatorBytesLength = common.AddressLength + types.BLSPublicKeyLength
	validatorNumberSize  = 1 // Fixed number of extra prefix bytes reserved for validator number after Luban
	turnLengthSize       = 1 // Fixed number of extra-data suffix bytes reserved for turnLength

	wiggleTime                uint64 = 1000 // milliseconds, Random delay (per signer) to allow concurrent signers
	defaultInitialBackOffTime uint64 = 1000 // milliseconds, Default backoff time for the second validator permitted to produce blocks
	lorentzInitialBackOffTime uint64 = 2000 // milliseconds, Backoff time for the second validator permitted to produce blocks from the Lorentz hard fork

	systemRewardPercent = 4 // it means 1/2^4 = 1/16 percentage of gas fee incoming will be distributed to system

	collectAdditionalVotesRewardRatio = 100 // ratio of additional reward for collecting more votes than needed, the denominator is 100

	gasLimitBoundDivisorBeforeLorentz uint64 = 256 // The bound divisor of the gas limit, used in update calculations before lorentz hard fork.

	// `finalityRewardInterval` should be smaller than `inMemorySnapshots`, otherwise, it will result in excessive computation.
	finalityRewardInterval = 200
	millisecondsUnit       = 250

	// Reuse Parlia fork constants locally for HotStuff integration
	lorentzEpochLength              uint64 = 500
	maxwellEpochLength              uint64 = 1000
	lorentzBlockInterval            uint64 = 1500
	maxwellBlockInterval            uint64 = 750
	defaultTurnLength               uint8  = 1
	validatorBytesLengthBeforeLuban        = common.AddressLength

	// HotStuff SyncInfo encoding constants
	viewSize                    = 8                              // uint64 size in bytes
	hashSize                    = 32                             // common.Hash size in bytes
	countSize                   = 8                              // uint64 validator bitset size in bytes
	addressSize                 = 20                             // common.Address size in bytes
	syncInfoTotalSize           = viewSize + hashSize + viewSize // hqcView + hqcHash + htcView
	syncInfoProofTotalSize      = syncInfoTotalSize + countSize + types.BLSSignatureLength
	tcHeaderSize                = viewSize + viewSize + hashSize + countSize // view + highQC view + highQC hash + signer count
	hsFlag                 byte = 0xA5
	hsProofFlag            byte = 0xA6
)

var (
	// 100 native token
	maxSystemBalance                  = new(uint256.Int).Mul(uint256.NewInt(100), uint256.NewInt(params.Ether))
	verifyVoteAttestationErrorCounter = metrics.NewRegisteredCounter("parlia/verifyVoteAttestation/error", nil)
	updateAttestationErrorCounter     = metrics.NewRegisteredCounter("parlia/updateAttestation/error", nil)
	validVotesfromSelfCounter         = metrics.NewRegisteredCounter("parlia/VerifyVote/self", nil)
	doubleSignCounter                 = metrics.NewRegisteredCounter("parlia/doublesign", nil)
	intentionalDelayMiningCounter     = metrics.NewRegisteredCounter("parlia/intentionalDelayMining", nil)
	hotstuffProposalExecuteTimer      = metrics.NewRegisteredTimer("hotstuff/proposal/execute", nil)
	hotstuffProposalExecuteErrors     = metrics.NewRegisteredCounter("hotstuff/proposal/execute/error", nil)
	hotstuffPrewriteBlocksCounter     = metrics.NewRegisteredCounter("hotstuff/prewrite/blocks", nil)
	hotstuffPrewriteMissCounter       = metrics.NewRegisteredCounter("hotstuff/prewrite/miss", nil)
	hotstuffCommitFastPathCounter     = metrics.NewRegisteredCounter("hotstuff/commit/insert/fastpath", nil)
	hotstuffCommitReexecuteCounter    = metrics.NewRegisteredCounter("hotstuff/commit/insert/reexecute", nil)
	hotstuffCommitInsertErrorCounter  = metrics.NewRegisteredCounter("hotstuff/commit/insert/error", nil)

	// Difficulty markers (aligned with Parlia)
	diffInTurn = big.NewInt(2)
	diffNoTurn = big.NewInt(1)

	systemContracts = map[common.Address]bool{
		common.HexToAddress(systemcontracts.ValidatorContract):          true,
		common.HexToAddress(systemcontracts.SlashContract):              true,
		common.HexToAddress(systemcontracts.SystemRewardContract):       true,
		common.HexToAddress(systemcontracts.LightClientContract):        true,
		common.HexToAddress(systemcontracts.RelayerHubContract):         true,
		common.HexToAddress(systemcontracts.GovHubContract):             true,
		common.HexToAddress(systemcontracts.TokenHubContract):           true,
		common.HexToAddress(systemcontracts.RelayerIncentivizeContract): true,
		common.HexToAddress(systemcontracts.CrossChainContract):         true,
		common.HexToAddress(systemcontracts.StakeHubContract):           true,
		common.HexToAddress(systemcontracts.GovernorContract):           true,
		common.HexToAddress(systemcontracts.GovTokenContract):           true,
		common.HexToAddress(systemcontracts.TimelockContract):           true,
		common.HexToAddress(systemcontracts.TokenRecoverPortalContract): true,
	}
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of validators is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte signature suffix missing")

	// errExtraValidators is returned if non-sprint-end block contain validator data in
	// their extra-data fields.
	errExtraValidators = errors.New("non-sprint-end block contains extra validator list")

	// errInvalidSpanValidators is returned if a block contains an
	// invalid list of validators (i.e. non divisible by 20 bytes).
	errInvalidSpanValidators = errors.New("invalid validator list on sprint end block")

	// errInvalidTurnLength is returned if a block contains an
	// invalid length of turn (i.e. no data left after parsing validators).
	errInvalidTurnLength = errors.New("invalid turnLength")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errMismatchingEpochValidators is returned if a sprint block contains a
	// list of validators different than the one the local node calculated.
	errMismatchingEpochValidators = errors.New("mismatching validator list on epoch block")

	// errMismatchingEpochTurnLength is returned if a sprint block contains a
	// turn length different than the one the local node calculated.
	errMismatchingEpochTurnLength = errors.New("mismatching turn length on epoch block")

	// errInvalidDifficulty is returned if the difficulty of a block is missing.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// errWrongDifficulty is returned if the difficulty of a block doesn't match the
	// turn of the signer.
	errWrongDifficulty = errors.New("wrong difficulty")

	// errOutOfRangeChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errOutOfRangeChain = errors.New("out of range or non-contiguous chain")

	// errBlockHashInconsistent is returned if an authorization list is attempted to
	// insert an inconsistent block.
	errBlockHashInconsistent = errors.New("the block hash is inconsistent")

	// errUnauthorizedValidator is returned if a header is signed by a non-authorized entity.
	errUnauthorizedValidator = func(val string) error {
		return errors.New("unauthorized validator: " + val)
	}

	// errCoinBaseMisMatch is returned if a header's coinbase do not match with signature
	errCoinBaseMisMatch = errors.New("coinbase do not match with signature")

	// errRecentlySigned is returned if a header is signed by an authorized entity
	// that already signed a header recently, thus is temporarily not allowed to.
	errRecentlySigned = errors.New("recently signed")
)

// SignerFn is a signer callback function to request a header to be signed by a
// backing account.
type SignerFn func(accounts.Account, string, []byte) ([]byte, error)
type SignerTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)

type authorizedSigner struct {
	val      common.Address
	signFn   SignerFn
	signTxFn SignerTxFn
}

func isToSystemContract(to common.Address) bool {
	return systemContracts[to]
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigCache *lru.Cache[common.Hash, common.Address], chainId *big.Int) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigCache.Get(hash); known {
		return address, nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(types.SealHash(header, chainId).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigCache.Add(hash, signer)
	return signer, nil
}

// ParliaRLP returns the rlp bytes which needs to be signed for the parlia
// sealing. The RLP to sign consists of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func HotstuffRLP(header *types.Header, chainId *big.Int) []byte {
	b := new(bytes.Buffer)
	types.EncodeSigHeader(b, header, chainId)
	return b.Bytes()
}

// NumFaulty calculates 'f', which is the number of replicas that can be faulty for a configuration of size 'n'.
func NumFaulty(n int) int {
	return (n - 1) / 3
}

// QuorumSize moved to hotstuff_utils.go

type Hotstuff struct {
	chainConfig *params.ChainConfig  // Chain config
	config      *params.ParliaConfig // Consensus engine configuration parameters for parlia consensus
	genesisHash common.Hash
	db          ethdb.Database // Database to store and retrieve snapshot checkpoints

	recentSnaps   *lru.Cache[common.Hash, *Snapshot]      // Snapshots for recent block to speed up
	signatures    *lru.Cache[common.Hash, common.Address] // Signatures of recent blocks to speed up mining
	recentHeaders *lru.Cache[string, common.Hash]
	// Recent headers to check for double signing: key includes block number and miner. value is the block header
	// If same key's value already exists for different block header roots then double sign is detected

	signer types.Signer

	authorized atomic.Pointer[authorizedSigner]

	lock sync.RWMutex // Protects HotStuff runtime state

	// stateLock serializes speculative metadata prewrites with canonical
	// InsertChain. Speculative StateDBs are never committed to the shared trie.
	stateLock sync.Mutex

	ethAPI                     *ethapi.BlockChainAPI
	VotePool                   consensus.VotePool
	validatorSetABIBeforeLuban abi.ABI
	validatorSetABI            abi.ABI
	slashABI                   abi.ABI
	stakeHubABI                abi.ABI
	hotstuffConfig             interface{}

	// HotStuff core state (non-final, for hs subprotocol integration)
	_hs *hsState
	// Runtime dependencies are atomically published so no consensus lock is
	// held across wallet or network calls.
	hsNet atomic.Pointer[hsNetworkRef]

	// Chain reader for hs runtime paths
	chain consensus.ChainHeaderReader

	// BLS vote signer (optional). When set, HotStuff votes will be BLS-signed.
	blsSigner atomic.Pointer[blsSignerRef]

	// View timeout management
	hsTimer         *time.Timer
	hsBaseTimeoutMS uint64 // base timeout in milliseconds; 0 => derive from snapshot
	lastTimeoutView uint64
	hsWALError      error
	closed          bool

	// proposalBlocksCache is a lock-free cache for proposal blocks.
	// Used by executeBlocks/Finalize to access parent blocks without acquiring h.lock,
	// which prevents deadlock when another goroutine is waiting for h.lock.Lock().
	// Key: common.Hash (block hash), Value: *types.Block
	proposalBlocksCache sync.Map
	lastTimeoutPacket   *hs.TimeoutPacket

	commitMu      sync.Mutex
	commitWaiters map[common.Hash]map[chan *types.Block]struct{}

	// notifyMinerCh is used to notify miner to immediately produce a block
	// when view changes and this node becomes the leader
	notifyMinerCh chan struct{}
}

// HsNetwork is provided by the p2p层来发送/广播 hs 报文
type HsNetwork interface {
	BroadcastProposal(*hs.ProposalPacket) error
	SendVoteToLeader(common.Address, *hs.VotePacket) error
	BroadcastNewView(*hs.NewViewPacket) int
	BroadcastQC(*hs.QuorumCertPacket) int
	BroadcastTimeout(*hs.TimeoutPacket) int
	ResolveAddress(peerID string) (common.Address, bool)
}

type hsNetworkRef struct{ network HsNetwork }
type blsSignerRef struct{ signer HsBlsSigner }

// SetHsNetwork sets the hs network adapter from p2p side
// Accept interface{} to match backend type assertion, then cast to HsNetwork.
func (h *Hotstuff) SetHsNetwork(n interface{}) {
	if net, ok := n.(HsNetwork); ok {
		h.hsNet.Store(&hsNetworkRef{network: net})
	}
}

func (h *Hotstuff) hsNetwork() HsNetwork {
	if ref := h.hsNet.Load(); ref != nil {
		return ref.network
	}
	return nil
}

// relab engine integration removed

// SetChainReader injects the chain reader into the engine for hs runtime paths
func (h *Hotstuff) SetChainReader(chain consensus.ChainHeaderReader) {
	h.chain = chain
	// Initialize hsState from persisted snapshot if available
	if chain != nil {
		head := chain.CurrentHeader()
		if head != nil {
			if snap, err := h.snapshot(chain, head.Number.Uint64(), head.Hash(), nil); err == nil && snap != nil {
				h.lock.Lock()
				st := h.getHsState()
				if st != nil {
					nextView := snap.CurrentView
					if nextView < math.MaxUint64 {
						nextView++
					}
					if nextView > st.currentView {
						st.currentView = nextView
					}
					if st.hasLastVote && st.lastVotedView < math.MaxUint64 && st.lastVotedView+1 > st.currentView {
						st.currentView = st.lastVotedView + 1
					}
					if snap.HighQCView > 0 && (snap.HighQCHash != (common.Hash{})) {
						// Preserve a proof-bearing WAL HighQC. Snapshot checkpoints only
						// persist the hash/view and cannot replace its aggregate proof.
						if st.highQC == nil {
							st.highQC = &HsQC{BlockHash: snap.HighQCHash, View: snap.HighQCView}
						}
						log.Debug("initHsState: initialized highQC from snapshot",
							"view", snap.HighQCView,
							"blockHash", snap.HighQCHash.Hex()[:8],
							"note", "SignersSet/Sig will be updated from QC messages")
					}
					if st.highQC == nil && h.chainConfig != nil && h.chainConfig.HotstuffBlock != nil {
						next := new(big.Int).Add(head.Number, common.Big1)
						bootstrap := h.chainConfig.IsOnHotstuff(next) ||
							(h.chainConfig.HotstuffBlock.Sign() == 0 && head.Number.Sign() == 0)
						if bootstrap {
							st.highQC = &HsQC{BlockHash: head.Hash(), View: getViewFromHeader(head, h.chainConfig)}
							log.Info("Initialized HotStuff bootstrap HighQC", "number", head.Number, "hash", head.Hash(), "view", st.highQC.View)
						}
					}
				}
				h.lock.Unlock()
			}
		}
		if err := h.recoverSpeculativeTail(); err != nil {
			log.Error("Failed to recover HotStuff speculative tail; voting remains disabled", "err", err)
		}
	}
	h.restartViewTimeout()
}

// SetBLSVoteSigner injects a BLS vote signer for HotStuff votes
// Accept interface{} for backend-friendly dynamic injection
func (h *Hotstuff) SetBLSVoteSigner(vs interface{}) {
	if s, ok := vs.(HsBlsSigner); ok {
		h.blsSigner.Store(&blsSignerRef{signer: s})
	}
}

func (h *Hotstuff) blsVoteSigner() HsBlsSigner {
	if ref := h.blsSigner.Load(); ref != nil {
		return ref.signer
	}
	return nil
}

// HsBlsSigner is the minimal interface required from a BLS signer
type HsBlsSigner interface {
	SignRoot(root [32]byte) (types.BLSPublicKey, types.BLSSignature, error)
}

// New creates a Hotstuff consensus engine.
func New(
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	ethAPI *ethapi.BlockChainAPI,
	genesisHash common.Hash,
	_ interface{},
	hotstuffConfig interface{},
) *Hotstuff {
	// get parlia config
	parliaConfig := chainConfig.Parlia
	log.Info("Hotstuff", "chainConfig", chainConfig)

	vABIBeforeLuban, err := abi.JSON(strings.NewReader(validatorSetABIBeforeLuban))
	if err != nil {
		panic(err)
	}
	vABI, err := abi.JSON(strings.NewReader(validatorSetABI))
	if err != nil {
		panic(err)
	}
	sABI, err := abi.JSON(strings.NewReader(slashABI))
	if err != nil {
		panic(err)
	}
	stABI, err := abi.JSON(strings.NewReader(stakeABI))
	if err != nil {
		panic(err)
	}

	c := &Hotstuff{
		chainConfig:                chainConfig,
		config:                     parliaConfig,
		genesisHash:                genesisHash,
		db:                         db,
		ethAPI:                     ethAPI,
		recentSnaps:                lru.NewCache[common.Hash, *Snapshot](inMemorySnapshots),
		recentHeaders:              lru.NewCache[string, common.Hash](inMemoryHeaders),
		signatures:                 lru.NewCache[common.Hash, common.Address](inMemorySignatures),
		validatorSetABIBeforeLuban: vABIBeforeLuban,
		validatorSetABI:            vABI,
		slashABI:                   sABI,
		stakeHubABI:                stABI,
		signer:                     types.LatestSigner(chainConfig),
		hotstuffConfig:             hotstuffConfig,
		commitWaiters:              make(map[common.Hash]map[chan *types.Block]struct{}),
		notifyMinerCh:              make(chan struct{}, 1),
	}
	// init hs runtime state
	c.initHsState()
	if err := c.loadHsWAL(); err != nil {
		c.hsWALError = err
		log.Error("Failed to restore HotStuff safety WAL; voting is disabled", "err", err)
	}
	// initialize timeout parameters
	// Base timeout = block interval + 5s buffer for network delay, execution, etc.
	c.hsBaseTimeoutMS = defaultBlockInterval + 5000 // base timeout: block interval + buffer
	return c
}

// GetNotifyMinerCh returns the channel for notifying miner to immediately produce a block.
// This is used by the miner worker to listen for view change events when this node becomes leader.
func (h *Hotstuff) GetNotifyMinerCh() <-chan struct{} {
	return h.notifyMinerCh
}

func (h *Hotstuff) registerCommitWaiter(hash common.Hash) chan *types.Block {
	waiter := make(chan *types.Block, 1)
	h.commitMu.Lock()
	if h.commitWaiters == nil {
		h.commitWaiters = make(map[common.Hash]map[chan *types.Block]struct{})
	}
	if h.commitWaiters[hash] == nil {
		h.commitWaiters[hash] = make(map[chan *types.Block]struct{})
	}
	h.commitWaiters[hash][waiter] = struct{}{}
	h.commitMu.Unlock()
	return waiter
}

func (h *Hotstuff) unregisterCommitWaiter(hash common.Hash, waiter chan *types.Block) {
	h.commitMu.Lock()
	if waiters := h.commitWaiters[hash]; waiters != nil {
		delete(waiters, waiter)
		if len(waiters) == 0 {
			delete(h.commitWaiters, hash)
		}
	}
	h.commitMu.Unlock()
}

func (h *Hotstuff) notifyCommittedBlock(block *types.Block) {
	if block == nil {
		return
	}
	hash := block.Hash()
	h.commitMu.Lock()
	waiterSet := h.commitWaiters[hash]
	waiters := make([]chan *types.Block, 0, len(waiterSet))
	for waiter := range waiterSet {
		waiters = append(waiters, waiter)
	}
	delete(h.commitWaiters, hash)
	h.commitMu.Unlock()
	for _, waiter := range waiters {
		waiter <- block
	}
}

func (h *Hotstuff) IsHotstuffMining(number *big.Int) bool {
	active := h.chainConfig != nil && h.chainConfig.IsHotstuff(number)
	if active {
		h.ensureBootstrapAtHead()
	}
	return active
}

func (h *Hotstuff) HasPendingProposal() bool {
	h.ensureBootstrapAtHead()
	h.lock.RLock()
	defer h.lock.RUnlock()
	st := h.getHsState()
	if st == nil {
		return false
	}
	_, exists := st.proposalsByView[st.currentView]
	return exists
}

// ConsensusAddress returns the consensus address of the validator
func (h *Hotstuff) ConsensusAddress() common.Address {
	val, _, _ := h.signerCredentials()
	return val
}

func (h *Hotstuff) signerCredentials() (common.Address, SignerFn, SignerTxFn) {
	if signer := h.authorized.Load(); signer != nil {
		return signer.val, signer.signFn, signer.signTxFn
	}
	return common.Address{}, nil, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (h *Hotstuff) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return h.verifyHeader(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (h *Hotstuff) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	gopool.Submit(func() {
		for i, header := range headers {
			err := h.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	})
	return abort, results
}

// getValidatorBytesFromHeader returns the validators bytes extracted from the header's extra field if exists.
// The validators bytes would be contained only in the epoch block's header, and its each validator bytes length is fixed.
// On luban fork, we introduce vote attestation into the header's extra field, so extra format is different from before.
// Before luban fork: |---Extra Vanity---|---Validators Bytes (or Empty)---|---Extra Seal---|
// After luban fork:  |---Extra Vanity---|---Validators Number and Validators Bytes (or Empty)---|---Vote Attestation (or Empty)---|---Extra Seal---|
// After bohr fork:   |---Extra Vanity---|---Validators Number, Validators Bytes and Turn Length (or Empty)---|---Vote Attestation (or Empty)---|---Extra Seal---|
// For hotstuff: |---Extra Vanity---|---Validators Number, Validators Bytes and Turn Length (or Empty)---|HSFLAG(1) | HQC_VIEW(8) | HQC_HASH(32) | HTC_VIEW(8) ]|---Extra Seal
func getValidatorBytesFromHeader(header *types.Header, chainConfig *params.ChainConfig, epochLength uint64) []byte {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil
	}
	hsExtraSize := hotstuffExtraSize(header, chainConfig)
	if !chainConfig.IsLuban(header.Number) {
		if header.Number.Uint64()%epochLength == 0 && (len(header.Extra)-extraSeal-extraVanity-hsExtraSize)%validatorBytesLengthBeforeLuban != 0 {
			return nil
		}
		return header.Extra[extraVanity : len(header.Extra)-extraSeal-hsExtraSize]
	}
	if header.Number.Uint64()%epochLength != 0 {
		return nil
	}
	num := int(header.Extra[extraVanity])
	start := extraVanity + validatorNumberSize
	end := start + num*validatorBytesLength
	extraMinLen := end + extraSeal + hsExtraSize
	if num == 0 || len(header.Extra) < extraMinLen {
		return nil
	}
	return header.Extra[start:end]
}

// getParent returns the parent of a given block.
func (h *Hotstuff) getParent(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) (*types.Header, error) {
	var parent *types.Header
	number := header.Number.Uint64()
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
		// CRITICAL FIX: If parent not in canonical chain, try HotStuff state
		if parent == nil {
			log.Debug("getParent: parent not in canonical chain, trying HotStuff state",
				"parentHash", header.ParentHash.Hex()[:10],
				"parentNumber", number-1)

			parentBlock := h.GetBlockFromState(header.ParentHash)
			if parentBlock != nil {
				parent = parentBlock.Header()
				log.Debug("getParent: got parent from HotStuff state",
					"parentHash", header.ParentHash.Hex()[:10],
					"parentNumber", parent.Number.Uint64(),
					"parentGasLimit", parent.GasLimit)
			}
		}
	}

	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return nil, consensus.ErrUnknownAncestor
	}
	return parent, nil
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (h *Hotstuff) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}
	// Check that the extra-data contains the vanity, validators and signature.
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// check extra data
	number := header.Number.Uint64()
	epochLength, err := h.epochLength(chain, header, parents)
	if err != nil {
		return err
	}
	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise.
	// Luban changed the checkpoint validator encoding to include vote addresses,
	// but it did not make non-checkpoint blocks carry validator lists.
	signersBytes := getValidatorBytesFromHeader(header, h.chainConfig, epochLength)
	isEpoch := number%epochLength == 0
	// Allow validator list on Plato fork activation block even if not at epoch boundary.
	isPlatoForkBlock := h.chainConfig.IsOnPlato(header.Number)
	if !isEpoch && len(signersBytes) != 0 && !isPlatoForkBlock {
		return errExtraValidators
	}
	if isEpoch && len(signersBytes) == 0 {
		return errInvalidSpanValidators
	}

	lorentz := chain.Config().IsLorentz(header.Number, header.Time)
	if !lorentz {
		if header.MixDigest != (common.Hash{}) {
			return errInvalidMixDigest
		}
	} else {
		if header.MilliTimestamp()/1000 != header.Time {
			return fmt.Errorf("invalid MixDigest, have %#x, expected the last two bytes to represent milliseconds", header.MixDigest)
		}
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != types.EmptyUncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		if header.Difficulty == nil {
			return errInvalidDifficulty
		}
	}

	parent, err := h.getParent(chain, header, parents)
	if err != nil {
		return err
	}

	// Verify the block's gas usage and (if applicable) verify the base fee.
	if !chain.Config().IsLondon(header.Number) {
		// Verify BaseFee not present before EIP-1559 fork.
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, expected 'nil'", header.BaseFee)
		}
	} else if err := eip1559.VerifyEIP1559Header(chain.Config(), parent, header); err != nil {
		// Verify the header's EIP-1559 attributes.
		log.Warn("verifyHeader: VerifyEIP1559Header failed", "block", header.Number.Uint64(), "error", err)
		return err
	}

	cancun := chain.Config().IsCancun(header.Number, header.Time)
	if !cancun {
		switch {
		case header.ExcessBlobGas != nil:
			return fmt.Errorf("invalid excessBlobGas: have %d, expected nil", header.ExcessBlobGas)
		case header.BlobGasUsed != nil:
			return fmt.Errorf("invalid blobGasUsed: have %d, expected nil", header.BlobGasUsed)
		case header.WithdrawalsHash != nil:
			return fmt.Errorf("invalid WithdrawalsHash, have %#x, expected nil", header.WithdrawalsHash)
		}
	} else {
		switch {
		case !header.EmptyWithdrawalsHash():
			return errors.New("header has wrong WithdrawalsHash")
		}
		if err := eip4844.VerifyEIP4844Header(chain.Config(), parent, header); err != nil {
			return err
		}
	}

	bohr := chain.Config().IsBohr(header.Number, header.Time)
	if !bohr {
		if header.ParentBeaconRoot != nil {
			return fmt.Errorf("invalid parentBeaconRoot, have %#x, expected nil", header.ParentBeaconRoot)
		}
	} else {
		if header.ParentBeaconRoot == nil || *header.ParentBeaconRoot != (common.Hash{}) {
			return fmt.Errorf("invalid parentBeaconRoot, have %#x, expected zero hash", header.ParentBeaconRoot)
		}
	}

	prague := chain.Config().IsPrague(header.Number, header.Time)
	if !prague {
		if header.RequestsHash != nil {
			return fmt.Errorf("invalid RequestsHash, have %#x, expected nil", header.RequestsHash)
		}
	} else {
		if header.RequestsHash == nil {
			return errors.New("header has nil RequestsHash after Prague")
		}
	}

	// All basic checks passed, verify cascading fields
	return h.verifyCascadingFields(chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (h *Hotstuff) verifyCascadingFields(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}

	log.Warn("verifyCascadingFields: start",
		"block", number,
		"headerHash", header.Hash().Hex()[:10],
		"headerParentHash", header.ParentHash.Hex()[:10],
		"headerGasLimit", header.GasLimit)

	parent, err := h.getParent(chain, header, parents)
	if err != nil {
		log.Error("verifyCascadingFields: getParent failed",
			"block", number,
			"parentHash", header.ParentHash.Hex()[:10],
			"error", err)
		return err
	}

	log.Warn("verifyCascadingFields: got parent",
		"block", number,
		"parentHash", parent.Hash().Hex()[:10],
		"parentNumber", parent.Number.Uint64(),
		"parentGasLimit", parent.GasLimit)

	snap, err := h.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		log.Error("verifyCascadingFields: snapshot failed",
			"block", number,
			"parentHash", header.ParentHash.Hex()[:10],
			"error", err)
		return err
	}

	log.Warn("verifyCascadingFields: got snapshot",
		"block", number,
		"snapNumber", snap.Number,
		"snapHash", snap.Hash.Hex()[:10])

	err = h.blockTimeVerifyForRamanujanFork(snap, header, parent)
	if err != nil {
		log.Error("verifyCascadingFields: blockTimeVerifyForRamanujanFork failed",
			"block", number,
			"error", err)
		return err
	}

	// Verify that the gas limit is <= 2^63-1
	capacity := uint64(0x7fffffffffffffff)
	if header.GasLimit > capacity {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, capacity)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	// Verify that the gas limit remains within allowed bounds
	diff := int64(parent.GasLimit) - int64(header.GasLimit)
	if diff < 0 {
		diff *= -1
	}
	gasLimitBoundDivisor := gasLimitBoundDivisorBeforeLorentz
	if h.chainConfig.IsLorentz(header.Number, header.Time) {
		gasLimitBoundDivisor = params.GasLimitBoundDivisor
	}
	limit := parent.GasLimit / gasLimitBoundDivisor

	log.Warn("verifyCascadingFields: gas limit check",
		"block", header.Number.Uint64(),
		"headerGasLimit", header.GasLimit,
		"parentGasLimit", parent.GasLimit,
		"parentHash", parent.Hash().Hex()[:10],
		"parentNumber", parent.Number.Uint64(),
		"diff", diff,
		"limit", limit,
		"gasLimitBoundDivisor", gasLimitBoundDivisor)

	if uint64(diff) >= limit || header.GasLimit < params.MinGasLimit {
		return fmt.Errorf("invalid gas limit: have %d, want %d += %d", header.GasLimit, parent.GasLimit, limit-1)
	}

	// Verify vote attestation for fast finality.
	if err := h.verifyVoteAttestation(chain, header, parents); err != nil {
		log.Warn("Verify vote attestation failed", "error", err, "hash", header.Hash(), "number", header.Number,
			"parent", header.ParentHash, "coinbase", header.Coinbase, "extra", common.Bytes2Hex(header.Extra))
		verifyVoteAttestationErrorCounter.Inc(1)
		if chain.Config().IsPlato(header.Number) {
			return err
		}
	}

	// HotStuff: if TimeoutCert is embedded, verify its aggregate signature
	// CRITICAL FIX: TC is optional - parsing failure should not reject the block
	// If TC data is corrupted (EOF), treat it as "no TC" rather than rejecting
	tc, tcErr := h.parseTimeoutCert(header)
	if tcErr != nil {
		log.Debug("verifyCascadingFields: failed to parse TimeoutCert",
			"block", header.Number.Uint64(),
			"error", tcErr)
		return tcErr
	}
	if tc != nil {
		if !h.verifyTimeoutCert(tc) {
			return errors.New("invalid TimeoutCert aggregate signature")
		}
	}

	// HotStuff chained-safety minimal check: HighQC must justify parent
	if err := h.verifyHotstuffRules(chain, header, parent, snap, tc); err != nil {
		return err
	}

	// All basic checks passed, verify the seal and return
	return h.verifySeal(chain, header, parents)
}

// assembleHighQC collects votes for parent and advances HighQC in snapshot
func (h *Hotstuff) assembleHighQC(chain consensus.ChainHeaderReader, header *types.Header) error {
	// CRITICAL FIX: Get parent with fallback for Chained HotStuff
	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		log.Debug("assembleHighQC: parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10])

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent == nil {
		log.Debug("assembleHighQC: parent not found", "parentHash", header.ParentHash.Hex()[:10])
		return errors.New("parent not found")
	}
	number := header.Number.Uint64()
	snap, err := h.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	if h.VotePool == nil {
		return nil
	}
	votes := h.VotePool.FetchVoteByBlockHash(parent.Hash())
	if len(votes) == 0 {
		return nil
	}
	threshold := QuorumSize(len(snap.Validators))
	if len(votes) < threshold {
		return nil
	}
	// enough votes for parent: embed HighQC into header extra for this proposal
	// Use view number from parent header, not block number
	parentView := getViewFromHeader(parent, h.chainConfig)
	return h.embedHighQC(header, parent.Hash(), parentView)
}

// verifyHotstuffRules checks HighQC justifies parent (minimal chained HotStuff rule)
func (h *Hotstuff) verifyHotstuffRules(chain consensus.ChainHeaderReader, header, parent *types.Header, snap *Snapshot, tc *hsTimeoutCert) error {
	if !h.chainConfig.IsHotstuff(header.Number) {
		return nil
	}
	ok, hqcView, hqcHash, htcView, signersSet, sig, hasProof := parseSyncInfoWithProof(header, h.chainConfig)
	if !ok {
		return errors.New("missing HotStuff SyncInfo")
	}
	// 最小规则：HighQC 指向 parent
	if hqcHash != header.ParentHash {
		return errors.New("invalid HighQC: hash mismatch")
	}
	// Header view must be exactly derived from the proof it carries.
	currentView := getViewFromHeader(header, h.chainConfig)
	expectedView := hqcView + 1
	if tc != nil && (htcView == 0 || tc.View != htcView) {
		return errors.New("TimeoutCert does not match declared htcView")
	}
	if htcView > 0 && htcView >= hqcView {
		if tc == nil {
			return errors.New("missing TimeoutCert for htcView")
		}
		if tc.View != htcView {
			return errors.New("invalid TimeoutCert view: mismatch with htcView")
		}
		expectedView = htcView + 1
	}
	if currentView != expectedView {
		return fmt.Errorf("invalid view: have %d want %d", currentView, expectedView)
	}
	bootstrap := h.isBootstrapHeaderHighQC(header, parent, hqcHash, hqcView)
	if !hasProof && !bootstrap {
		return errors.New("missing HighQC aggregate proof")
	}
	if hasProof {
		qc := &hs.QuorumCertPacket{
			TargetHash:   hqcHash,
			ViewNumber:   hqcView,
			SignersSet:   signersSet,
			AggregateSig: sig,
		}
		if !h.verifyAggregateQC(qc) {
			return errors.New("invalid HighQC aggregate signature")
		}
	}
	return nil
}

func (h *Hotstuff) isBootstrapHeaderHighQC(header, parent *types.Header, hash common.Hash, view uint64) bool {
	if header == nil || parent == nil || header.Number == nil || parent.Number == nil ||
		parent.Hash() != hash || getViewFromHeader(parent, h.chainConfig) != view || parent.Number.Uint64()+1 != header.Number.Uint64() {
		return false
	}
	if h.chainConfig.HotstuffBlock.Sign() == 0 {
		return parent.Number.Sign() == 0 && header.Number.Uint64() == 1
	}
	return h.chainConfig.IsOnHotstuff(header.Number)
}

// snapshotWithFallback wraps snapshot with fallback logic for Chained HotStuff pipelining
// When parent block is not in canonical chain, tries alternative sources
// DEPRECATED: Use snapshot() instead, which now includes fallback logic by default
func (h *Hotstuff) snapshotWithFallback(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	return h.snapshot(chain, number, hash, parents)
}

// snapshot retrieves the authorization snapshot at a given point in time.
// !!! be careful
// the block with `number` and `hash` is just the last element of `parents`,
// unlike other interfaces such as verifyCascadingFields, `parents` are real parents
//
// CRITICAL: In Chained HotStuff, blocks may be pipelined (not yet committed to canonical chain).
// This function includes automatic fallback strategies to handle such cases.
func (h *Hotstuff) snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	// Deterministic resolution: only use the requested block (from parents, chain, or HotStuff state).
	// Do NOT fall back to chain head / genesis / grandparent, which can diverge across nodes.
	return h.snapshotInternal(chain, number, hash, parents)
}

// snapshotInternal is the original snapshot implementation without fallback logic
// This is used internally by snapshot() to avoid infinite recursion
func (h *Hotstuff) snapshotInternal(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		headers []*types.Header
		snap    *Snapshot
	)

	for snap == nil {
		// If an in-memory snapshot was found, use that
		if s, ok := h.recentSnaps.Get(hash); ok {
			snap = s
			break
		}

		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(h.config, h.signatures, h.db, hash, h.ethAPI); err == nil {
				log.Trace("Loaded snapshot from disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}

		// If we're at the genesis, snapshot the initial state. Alternatively if we have
		// piled up more headers than allowed to be reorged (chain reinit from a freezer),
		// consider the checkpoint trusted and snapshot it.

		// Unable to retrieve the exact EpochLength here.
		// As known
		// 		defaultEpochLength = 200 && turnLength = 1 or 4
		// 		lorentzEpochLength = 500 && turnLength = 8
		// 		maxwellEpochLength = 1000 && turnLength = 16
		// So just select block number like 1200, 2200, 3200, we can always get the right validators from `number - 200`
		offset := uint64(200)
		if number == 0 || (number%maxwellEpochLength == offset && (len(headers) > int(params.FullImmutabilityThreshold))) {
			var (
				checkpoint    *types.Header
				blockHash     common.Hash
				blockInterval = defaultBlockInterval
				epochLength   = defaultEpochLength
			)
			if number == 0 {
				checkpoint = chain.GetHeaderByNumber(0)
				if checkpoint != nil {
					blockHash = checkpoint.Hash()
				}
			} else {
				checkpoint = chain.GetHeaderByNumber(number - offset)
				blockHeader := chain.GetHeaderByNumber(number)
				if blockHeader != nil {
					blockHash = blockHeader.Hash()
					blockInterval = defaultBlockInterval
				}
				if number > offset { // exclude `number == 200`
					blockBeforeCheckpoint := chain.GetHeaderByNumber(number - offset - 1)
					if blockBeforeCheckpoint != nil {
						epochLength = defaultEpochLength
					}
				}
			}
			if checkpoint != nil && blockHash != (common.Hash{}) {
				// get validators from headers
				validators, voteAddrs, err := parseValidators(checkpoint, h.chainConfig, epochLength)
				if err != nil {
					return nil, err
				}

				// new snapshot
				snap = newSnapshot(h.config, h.signatures, number, blockHash, validators, voteAddrs, h.ethAPI)

				// get turnLength from headers and use that for new turnLength
				turnLength, err := parseTurnLength(checkpoint, h.chainConfig, epochLength)
				if err != nil {
					return nil, err
				}
				if turnLength != nil {
					snap.TurnLength = *turnLength
				}
				snap.BlockInterval = blockInterval
				snap.EpochLength = epochLength

				// snap.Recents is currently empty, which affects the following:
				// a. The function SignRecently - This is acceptable since an empty snap.Recents results in a more lenient check.
				// b. The function blockTimeVerifyForRamanujanFork - This is also acceptable as it won't be invoked during `snap.apply`.
				// c. This may cause a mismatch in the slash systemtx, but the transaction list is not verified during `snap.apply`.

				// snap.Attestation is nil, but Snapshot.updateAttestation will handle it correctly.
				if err := snap.store(h.db); err != nil {
					return nil, err
				}
				log.Info("Stored checkpoint snapshot to disk", "number", number, "hash", blockHash)
				break
			}
		}

		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number)
			if header == nil {
				// Try HotStuff in-memory proposals (pipelined block not in canonical chain yet)
				block := h.getBlockWithoutStateLock(hash)
				if block != nil && block.NumberU64() == number {
					header = block.Header()
					log.Debug("snapshotInternal: using HotStuff state header",
						"number", number,
						"hash", hash.Hex()[:10])
				} else {
					log.Debug("snapshotInternal: header not found",
						"number", number,
						"hash", hash.Hex()[:10],
						"foundInState", block != nil)
					return nil, consensus.ErrUnknownAncestor
				}
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}

	// check if snapshot is nil
	if snap == nil {
		return nil, fmt.Errorf("unknown error while retrieving snapshot at block number %v", number)
	}

	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}

	// Snapshot verification may run without h.lock, so only use lock-free/cache
	// and durable lookups for pipelined ancestors.
	snap, err := snap.apply(headers, chain, parents, h.chainConfig, h.getBlockWithoutStateLock)
	if err != nil {
		log.Error("Failed to apply headers to snapshot", "err", err)
		return nil, err
	}
	h.recentSnaps.Add(snap.Hash, snap)

	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(h.db); err != nil {
			return nil, err
		}
		log.Trace("Stored snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (h *Hotstuff) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

func (h *Hotstuff) VerifyRequests(header *types.Header, Requests [][]byte) error {
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (h *Hotstuff) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return h.verifySeal(chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (h *Hotstuff) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := h.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against validators
	signer, err := ecrecover(header, h.signatures, h.chainConfig.ChainID)
	if err != nil {
		return err
	}

	if signer != header.Coinbase {
		return errCoinBaseMisMatch
	}

	// check for double sign & add to cache
	key := proposalKey(*header)
	preHash, ok := h.recentHeaders.Get(key)
	if ok && preHash != header.Hash() {
		doubleSignCounter.Inc(1)
		log.Warn("DoubleSign detected", " block", header.Number, " miner", header.Coinbase,
			"hash1", preHash, "hash2", header.Hash())
	} else {
		h.recentHeaders.Add(key, header.Hash())
	}

	if _, ok := snap.Validators[signer]; !ok {
		log.Warn("Unauthorized validator", "number", header.Number, "signer", signer)
		return errUnauthorizedValidator(signer.String())
	}
	if h.chainConfig.IsHotstuff(header.Number) {
		validators := snap.validators()
		if len(validators) == 0 {
			return errors.New("empty HotStuff validator set")
		}
		view := getViewFromHeader(header, h.chainConfig)
		leader := validators[view%uint64(len(validators))]
		if signer != leader {
			return fmt.Errorf("unauthorized HotStuff proposer: have %s want leader %s for view %d", signer, leader, view)
		}
	}

	return nil
}

func (h *Hotstuff) prepareValidators(chain consensus.ChainHeaderReader, header *types.Header) error {
	epochLength, err := h.epochLength(chain, header, nil)
	if err != nil {
		log.Error("prepareValidators failed to get epoch length", "err", err)
		return err
	}

	isEpochBoundary := header.Number.Uint64()%epochLength == 0

	if !isEpochBoundary {
		return nil
	}

	// Get parent header to query validators at parent's state
	// CRITICAL: Try canonical chain first, then HotStuff state (for pipelined proposals)
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		log.Debug("[prepareValidators] parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent == nil {
		log.Error("[prepareValidators] parent header not found",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return errors.New("parent header not found")
	}

	newValidators, voteAddressMap, err := h.getCurrentValidators(parent)
	if err != nil {
		log.Error("prepareValidators failed to get current validators", "err", err)
		return err
	}
	// sort validator by address
	sort.Sort(validatorsAscending(newValidators))
	if !h.chainConfig.IsLuban(header.Number) {
		for _, validator := range newValidators {
			header.Extra = append(header.Extra, validator.Bytes()...)
		}
	} else {
		header.Extra = append(header.Extra, byte(len(newValidators)))
		for _, validator := range newValidators {
			header.Extra = append(header.Extra, validator.Bytes()...)
			header.Extra = append(header.Extra, voteAddressMap[validator].Bytes()...)
		}
	}
	return nil
}

// prepareValidatorsWithSnapshot prepares validators using an existing snapshot
// This avoids accessing the canonical chain when the parent block is not yet committed
func (h *Hotstuff) prepareValidatorsWithSnapshot(chain consensus.ChainHeaderReader, header *types.Header, snap *Snapshot) error {
	if snap == nil {
		// Fallback to original method if snapshot is nil
		return h.prepareValidators(chain, header)
	}

	epochLength := snap.EpochLength
	isEpochBoundary := header.Number.Uint64()%epochLength == 0

	if !isEpochBoundary {
		return nil
	}

	// Use validators from snapshot instead of calling getCurrentValidators
	validators := snap.validators()
	if len(validators) == 0 {
		// Fallback to original method if snapshot has no validators
		return h.prepareValidators(chain, header)
	}

	// Build voteAddressMap from snapshot
	voteAddressMap := make(map[common.Address]*types.BLSPublicKey)
	for addr, valInfo := range snap.Validators {
		if valInfo.VoteAddress != (types.BLSPublicKey{}) {
			voteAddr := valInfo.VoteAddress
			voteAddressMap[addr] = &voteAddr
		}
	}

	// Sort validators by address
	sort.Sort(validatorsAscending(validators))
	if !h.chainConfig.IsLuban(header.Number) {
		for _, validator := range validators {
			header.Extra = append(header.Extra, validator.Bytes()...)
		}
	} else {
		header.Extra = append(header.Extra, byte(len(validators)))
		for _, validator := range validators {
			header.Extra = append(header.Extra, validator.Bytes()...)
			if voteAddr, ok := voteAddressMap[validator]; ok {
				header.Extra = append(header.Extra, voteAddr.Bytes()...)
			} else {
				// If no vote address in snapshot, use zero BLS key
				zeroBlsKey := types.BLSPublicKey{}
				header.Extra = append(header.Extra, zeroBlsKey.Bytes()...)
			}
		}
	}
	return nil
}

// prepareTurnLength adds turnLength to header.Extra for epoch boundary blocks
func (h *Hotstuff) prepareTurnLength(chain consensus.ChainHeaderReader, header *types.Header) error {
	epochLength, err := h.epochLength(chain, header, nil)
	if err != nil {
		return err
	}
	if header.Number.Uint64()%epochLength != 0 ||
		!h.chainConfig.IsBohr(header.Number, header.Time) {
		return nil
	}

	turnLength, err := h.getTurnLength(chain, header)
	if err != nil {
		return err
	}

	if turnLength != nil {
		header.Extra = append(header.Extra, *turnLength)
	}

	return nil
}

// NextInTurnValidator return the next in-turn validator for header
func (h *Hotstuff) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	snap, err := h.snapshot(chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		return common.Address{}, err
	}

	return snap.inturnValidator(), nil
}

func (h *Hotstuff) blockTime(snap *Snapshot, header, parent *types.Header) uint64 {
	blockTime := parent.MilliTimestamp() + snap.BlockInterval
	if now := uint64(time.Now().UnixMilli()); blockTime < now {
		// Just to make the millisecond part of the time look more aligned.
		blockTime = uint64(cmath.CeilDiv(int(now), millisecondsUnit)) * millisecondsUnit
	}
	return blockTime
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (h *Hotstuff) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	header.Coinbase = h.ConsensusAddress()
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()
	snap, err := h.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return fmt.Errorf("resolve parent snapshot %s at block %d: %w", header.ParentHash, number-1, err)
	}

	header.Difficulty = big.NewInt(0)

	// Ensure the extra data has all it's components
	if len(header.Extra) < extraVanity-nextForkHashSize {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-nextForkHashSize-len(header.Extra))...)
	}

	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		// Fallback: if parent block is not in chain yet, try to get from HotStuff state or rawdb
		log.Warn("[Prepare] Parent block not in canonical chain, trying GetBlockFromState",
			"parentHash", header.ParentHash.Hex()[:8],
			"parentNumber", number-1,
			"blockNumber", number)
		if block := h.GetBlockFromState(header.ParentHash); block != nil {
			parent = block.Header()
			log.Info("[Prepare] Got parent header from HotStuff state",
				"parentHash", header.ParentHash.Hex()[:8],
				"parentNumber", parent.Number.Uint64(),
				"blockNumber", number)
		} else {
			log.Error("[Prepare] GetBlockFromState returned nil - CRITICAL",
				"parentHash", header.ParentHash.Hex()[:8],
				"parentNumber", number-1,
				"blockNumber", number)
			return consensus.ErrUnknownAncestor
		}
	} else {
		log.Debug("[Prepare] Got parent from canonical chain",
			"parentHash", header.ParentHash.Hex()[:8],
			"parentNumber", parent.Number.Uint64(),
			"blockNumber", number)
	}
	blockTime := h.blockTime(snap, header, parent)
	header.Time = blockTime / 1000 // get seconds
	header.SetMilliseconds(blockTime % 1000)
	if h.chainConfig.IsLorentz(header.Number, header.Time) {
		header.SetMilliseconds(blockTime % 1000)
	} else {
		header.MixDigest = common.Hash{}
	}

	header.Extra = header.Extra[:extraVanity-nextForkHashSize]
	nextForkHash := forkid.NextForkHash(h.chainConfig, h.genesisHash, chain.GenesisHeader().Time, number, header.Time)
	header.Extra = append(header.Extra, nextForkHash[:]...)

	// Use the snapshot we already have instead of calling prepareValidators which may fail
	// if parent block is not in canonical chain
	if err := h.prepareValidators(chain, header); err != nil {
		log.Error("Failed to prepare validators", "err", err)
		return err
	}
	// Add turnLength for epoch boundary blocks (must be after validators, before syncInfo)
	if err := h.prepareTurnLength(chain, header); err != nil {
		log.Error("Failed to prepare turn length", "err", err)
		return err
	}
	// Embed HotStuff SyncInfo (HighQC/HighTC minimal) into extra-data before seal
	if err := h.embedSyncInfoInHeader(header); err != nil {
		log.Error("Failed to embed sync info in header", "err", err)
		return err
	}
	// add extra seal space
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	return nil
}

func (h *Hotstuff) verifyValidators(chain consensus.ChainHeaderReader, header *types.Header) error {
	epochLength, err := h.epochLength(chain, header, nil)
	if err != nil {
		return err
	}

	isEpochBoundary := header.Number.Uint64()%epochLength == 0

	if !isEpochBoundary {
		return nil
	}

	// Get parent header to query validators at parent's state
	// CRITICAL: Try canonical chain first, then HotStuff state (for pipelined proposals)
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		log.Debug("[verifyValidators] parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent == nil {
		log.Error("[verifyValidators] parent header not found",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return errors.New("parent header not found")
	}

	newValidators, voteAddressMap, err := h.getCurrentValidators(parent)
	if err != nil {
		return err
	}
	// sort validator by address
	sort.Sort(validatorsAscending(newValidators))
	var validatorsBytes []byte
	validatorsNumber := len(newValidators)
	if !h.chainConfig.IsLuban(header.Number) {
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLengthBeforeLuban)
		for i, validator := range newValidators {
			copy(validatorsBytes[i*validatorBytesLengthBeforeLuban:], validator.Bytes())
		}
	} else {
		if uint8(validatorsNumber) != header.Extra[extraVanity] {
			return errMismatchingEpochValidators
		}
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLength)
		if h.chainConfig.IsOnLuban(header.Number) {
			voteAddressMap = make(map[common.Address]*types.BLSPublicKey, len(newValidators))
			var zeroBlsKey types.BLSPublicKey
			for _, validator := range newValidators {
				voteAddressMap[validator] = &zeroBlsKey
			}
		}
		for i, validator := range newValidators {
			copy(validatorsBytes[i*validatorBytesLength:], validator.Bytes())
			copy(validatorsBytes[i*validatorBytesLength+common.AddressLength:], voteAddressMap[validator].Bytes())
		}
	}
	if !bytes.Equal(getValidatorBytesFromHeader(header, h.chainConfig, epochLength), validatorsBytes) {
		return errMismatchingEpochValidators
	}
	return nil
}

// verifyValidatorsWithSnapshot verifies validators using an existing snapshot
// This avoids accessing the canonical chain when the parent block is not yet committed
func (h *Hotstuff) verifyValidatorsWithSnapshot(chain consensus.ChainHeaderReader, header *types.Header, snap *Snapshot) error {
	if snap == nil {
		// Fallback to original method if snapshot is nil
		log.Error("Snapshot is nil, falling back to original method")
		return h.verifyValidators(chain, header)
	}

	epochLength := snap.EpochLength
	isEpochBoundary := header.Number.Uint64()%epochLength == 0

	if !isEpochBoundary {
		return nil
	}

	// Try to get validators from parent block if it's in HotStuff state
	// Otherwise use snapshot validators (which may be outdated if parent is at epoch boundary)
	var validators []common.Address
	var voteAddressMap map[common.Address]*types.BLSPublicKey

	log.Debug("verifyValidatorsWithSnapshot: attempting to get parent block",
		"parentHash", header.ParentHash.Hex()[:10],
		"isEpochBoundary", isEpochBoundary)

	// CRITICAL: GetBlockFromState may block if called from wrong context
	// Use a goroutine with timeout to avoid indefinite blocking
	var parentBlock *types.Block
	done := make(chan struct{})
	go func() {
		parentBlock = h.GetBlockFromState(header.ParentHash)
		close(done)
	}()

	select {
	case <-done:
		log.Debug("verifyValidatorsWithSnapshot: got parent block",
			"found", parentBlock != nil)
	case <-time.After(100 * time.Millisecond):
		log.Warn("verifyValidatorsWithSnapshot: GetBlockFromState timed out, using snapshot validators",
			"parentHash", header.ParentHash.Hex()[:10])
		parentBlock = nil
	}

	if parentBlock != nil && isEpochBoundary {
		// If parent block is at epoch boundary, it should have validators in its header
		// Parse validators from parent block's header
		parentHeader := parentBlock.Header()
		// Use snapshot's epochLength instead of calling epochLength which may block
		parentEpochLength := epochLength
		validatorsBytes := getValidatorBytesFromHeader(parentHeader, h.chainConfig, parentEpochLength)
		if len(validatorsBytes) > 0 {
			// Parse validators from parent header
			if !h.chainConfig.IsLuban(parentHeader.Number) {
				n := len(validatorsBytes) / validatorBytesLengthBeforeLuban
				validators = make([]common.Address, n)
				for i := 0; i < n; i++ {
					validators[i] = common.BytesToAddress(validatorsBytes[i*validatorBytesLengthBeforeLuban : (i+1)*validatorBytesLengthBeforeLuban])
				}
				voteAddressMap = nil
			} else {
				n := len(validatorsBytes) / validatorBytesLength
				validators = make([]common.Address, n)
				voteAddressMap = make(map[common.Address]*types.BLSPublicKey, n)
				for i := 0; i < n; i++ {
					validators[i] = common.BytesToAddress(validatorsBytes[i*validatorBytesLength : i*validatorBytesLength+common.AddressLength])
					var blsKey types.BLSPublicKey
					copy(blsKey[:], validatorsBytes[i*validatorBytesLength+common.AddressLength:(i+1)*validatorBytesLength])
					voteAddressMap[validators[i]] = &blsKey
				}
			}
		}
	}

	// If couldn't get validators from parent block, use snapshot validators
	if len(validators) == 0 {
		validators = snap.validators()
		if len(validators) == 0 {
			// Fallback to original method if snapshot has no validators
			return h.verifyValidators(chain, header)
		}
		// Build voteAddressMap from snapshot
		voteAddressMap = make(map[common.Address]*types.BLSPublicKey)
		for addr, valInfo := range snap.Validators {
			if valInfo.VoteAddress != (types.BLSPublicKey{}) {
				voteAddr := valInfo.VoteAddress
				voteAddressMap[addr] = &voteAddr
			}
		}
	}

	// Sort validators by address
	sort.Sort(validatorsAscending(validators))
	var validatorsBytes []byte
	validatorsNumber := len(validators)
	if !h.chainConfig.IsLuban(header.Number) {
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLengthBeforeLuban)
		for i, validator := range validators {
			copy(validatorsBytes[i*validatorBytesLengthBeforeLuban:], validator.Bytes())
		}
	} else {
		if uint8(validatorsNumber) != header.Extra[extraVanity] {
			return errMismatchingEpochValidators
		}
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLength)
		if h.chainConfig.IsOnLuban(header.Number) && voteAddressMap == nil {
			voteAddressMap = make(map[common.Address]*types.BLSPublicKey, len(validators))
			var zeroBlsKey types.BLSPublicKey
			for _, validator := range validators {
				voteAddressMap[validator] = &zeroBlsKey
			}
		}
		for i, validator := range validators {
			copy(validatorsBytes[i*validatorBytesLength:], validator.Bytes())
			if voteAddr, ok := voteAddressMap[validator]; ok {
				copy(validatorsBytes[i*validatorBytesLength+common.AddressLength:], voteAddr.Bytes())
			} else {
				// If no vote address, use zero BLS key
				zeroBlsKey := types.BLSPublicKey{}
				copy(validatorsBytes[i*validatorBytesLength+common.AddressLength:], zeroBlsKey.Bytes())
			}
		}
	}
	if !bytes.Equal(getValidatorBytesFromHeader(header, h.chainConfig, epochLength), validatorsBytes) {
		return errMismatchingEpochValidators
	}
	return nil
}

func (h *Hotstuff) verifyTurnLength(chain consensus.ChainHeaderReader, header *types.Header) error {
	epochLength, err := h.epochLength(chain, header, nil)
	if err != nil {
		return err
	}
	if header.Number.Uint64()%epochLength != 0 ||
		!h.chainConfig.IsBohr(header.Number, header.Time) {
		return nil
	}

	turnLengthFromHeader, err := parseTurnLength(header, h.chainConfig, epochLength)
	if err != nil {
		return err
	}
	if turnLengthFromHeader != nil {
		turnLength, err := h.getTurnLength(chain, header)
		if err != nil {
			log.Warn("verifyTurnLength: both snapshot and contract query failed, skipping verification",
				"error", err,
				"block", header.Number.Uint64())
			return nil // Skip verification if both methods fail
		}

		if turnLength != nil && *turnLength == *turnLengthFromHeader {
			log.Debug("verifyTurnLength", "turnLength", *turnLength)
			return nil
		}

		log.Error("turnLength mismatch",
			"headerTurnLength", *turnLengthFromHeader,
			"turnLength", *turnLength,
			"block", header.Number.Uint64())
		return errMismatchingEpochTurnLength
	}

	return nil
}

func (h *Hotstuff) distributeFinalityReward(chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	cx core.ChainContext, txs *[]*types.Transaction, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction,
	usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	log.Debug("[distributeFinalityReward] ENTER", "block", header.Number)

	currentHeight := header.Number.Uint64()
	if currentHeight%finalityRewardInterval != 0 {
		log.Debug("[distributeFinalityReward] EXIT - not finality reward block", "block", header.Number)
		return nil
	}

	head := header
	accumulatedWeights := make(map[common.Address]uint64)
	for height := currentHeight - 1; height+finalityRewardInterval >= currentHeight && height >= 1; height-- {
		// CRITICAL FIX: Get header with fallback for Chained HotStuff pipelined blocks
		parentHash := head.ParentHash
		head = chain.GetHeaderByHash(parentHash)
		if head == nil {
			// Not in canonical chain, try HotStuff state
			log.Debug("[distributeFinalityReward] parent not in canonical chain, trying HotStuff state",
				"parentHash", parentHash.Hex()[:10],
				"height", height)

			log.Debug("[distributeFinalityReward] calling GetBlockFromState (may acquire RLock)",
				"parentHash", parentHash.Hex()[:10])
			parentBlock := h.GetBlockFromState(parentHash)
			log.Debug("[distributeFinalityReward] GetBlockFromState returned",
				"parentHash", parentHash.Hex()[:10],
				"found", parentBlock != nil)
			if parentBlock != nil {
				head = parentBlock.Header()
				log.Debug("[distributeFinalityReward] got parent header from HotStuff state",
					"number", head.Number.Uint64(),
					"hash", head.Hash().Hex()[:10])
			} else {
				log.Warn("[distributeFinalityReward] parent not found, returning error",
					"height", height,
					"parentHash", parentHash.Hex()[:10])
				return fmt.Errorf("header for hash not found at height %d (hash: %s)",
					height, parentHash.Hex()[:10])
			}
		}
		epochLength, err := h.epochLength(chain, head, nil)
		if err != nil {
			return err
		}
		voteAttestation, err := getVoteAttestationFromHeader(head, chain.Config(), epochLength)
		if err != nil {
			return err
		}
		if voteAttestation == nil {
			continue
		}
		// CRITICAL FIX: Get justifiedBlock with fallback for Chained HotStuff pipelined blocks
		justifiedBlock := chain.GetHeaderByHash(voteAttestation.Data.TargetHash)
		if justifiedBlock == nil {
			// Not in canonical chain, try HotStuff state
			log.Debug("distributeFinalityReward: justified block not in canonical chain, trying HotStuff state",
				"targetHash", voteAttestation.Data.TargetHash.Hex()[:10],
				"targetNumber", voteAttestation.Data.TargetNumber)

			justifiedBlockFull := h.GetBlockFromState(voteAttestation.Data.TargetHash)
			if justifiedBlockFull != nil {
				justifiedBlock = justifiedBlockFull.Header()
				log.Debug("distributeFinalityReward: got justified block from HotStuff state",
					"number", justifiedBlock.Number.Uint64(),
					"hash", justifiedBlock.Hash().Hex()[:10])
			} else {
				log.Warn("justifiedBlock not found at height %d (hash: %s)",
					voteAttestation.Data.TargetNumber,
					voteAttestation.Data.TargetHash.Hex()[:10])
				continue
			}
		}

		snap, err := h.snapshot(chain, justifiedBlock.Number.Uint64()-1, justifiedBlock.ParentHash, nil)
		if err != nil {
			return err
		}
		validators := snap.validators()
		validatorsBitSet := bitset.From([]uint64{uint64(voteAttestation.VoteAddressSet)})
		if validatorsBitSet.Count() > uint(len(validators)) {
			log.Error("invalid attestation, vote number larger than validators number")
			continue
		}
		validVoteCount := 0
		for index, val := range validators {
			if validatorsBitSet.Test(uint(index)) {
				accumulatedWeights[val] += 1
				validVoteCount += 1
			}
		}
		quorum := cmath.CeilDiv(len(snap.Validators)*2, 3)
		if validVoteCount > quorum {
			accumulatedWeights[head.Coinbase] += uint64((validVoteCount - quorum) * collectAdditionalVotesRewardRatio / 100)
		}
	}

	// If there is no valid vote within the interval, skip calling system contract.
	if len(accumulatedWeights) == 0 {
		return nil
	}

	validators := make([]common.Address, 0, len(accumulatedWeights))
	weights := make([]*big.Int, 0, len(accumulatedWeights))
	for val := range accumulatedWeights {
		validators = append(validators, val)
	}
	sort.Sort(validatorsAscending(validators))
	for _, val := range validators {
		weights = append(weights, big.NewInt(int64(accumulatedWeights[val])))
	}

	// generate system transaction
	method := "distributeFinalityReward"
	data, err := h.validatorSetABI.Pack(method, validators, weights)
	if err != nil {
		log.Error("Unable to pack tx for distributeFinalityReward", "error", err)
		return err
	}
	msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	return h.applyTransaction(msg, state, header, cx, txs, receipts, systemTxs, usedGas, mining, tracer)
}

func (h *Hotstuff) EstimateGasReservedForSystemTxs(chain consensus.ChainHeaderReader, header *types.Header) uint64 {
	// CRITICAL FIX: Get parent with fallback for Chained HotStuff
	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		log.Debug("EstimateGasReservedForSystemTxs: parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10])

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent != nil {
		// Mainnet and Chapel have both passed Feynman. Now, simplify the logic before and during the Feynman hard fork.
		if h.chainConfig.IsFeynman(header.Number, header.Time) &&
			!h.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
			// const (
			// 	the following values represent the maximum values found in the most recent blocks on the mainnet
			// 	depositTxGas         = uint64(60_000)
			// 	slashTxGas           = uint64(140_000)
			// 	finalityRewardTxGas  = uint64(350_000)
			// 	updateValidatorTxGas = uint64(12_160_000)
			// )
			// suggestReservedGas := depositTxGas
			// if header.Difficulty.Cmp(diffInTurn) != 0 {
			// 	snap, err := h.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
			// 	if err != nil || !snap.SignRecently(snap.inturnValidator()) {
			// 		suggestReservedGas += slashTxGas
			// 	}
			// }
			// if header.Number.Uint64()%h.config.Epoch == 0 {
			// 	suggestReservedGas += finalityRewardTxGas
			// }
			// if isBreatheBlock(parent.Time, header.Time) {
			// 	suggestReservedGas += updateValidatorTxGas
			// }
			// return suggestReservedGas * 150 / 100
			if !isBreatheBlock(parent.Time, header.Time) {
				// params.SystemTxsGasSoftLimit > (depositTxGas+slashTxGas+finalityRewardTxGas)*150/100
				return params.SystemTxsGasSoftLimit
			}
		}
	}

	// params.SystemTxsGasHardLimit > (depositTxGas+slashTxGas+finalityRewardTxGas+updateValidatorTxGas)*150/100
	return params.SystemTxsGasHardLimit
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given.
func (h *Hotstuff) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB, txs *[]*types.Transaction,
	uncles []*types.Header, _ []*types.Withdrawal, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	// warn if not in majority fork
	number := header.Number.Uint64()

	// CRITICAL FIX: Build a complete parents chain by walking backwards from current block
	// This ensures snapshotInternal can access all ancestors even if they're not in canonical chain
	parents := make([]*types.Header, 0)
	currentHash := header.ParentHash
	currentNumber := number - 1

	// Walk backwards to collect parent headers (from chain or HotStuff state)
	for currentNumber >= 0 {
		// Try canonical chain first
		parentHeader := chain.GetHeaderByHash(currentHash)
		if parentHeader == nil {
			// Not in canonical chain, try HotStuff state
			parentBlock := h.GetBlockFromState(currentHash)
			if parentBlock != nil {
				parentHeader = parentBlock.Header()
				log.Debug("Finalize: got ancestor header from HotStuff state",
					"number", currentNumber,
					"hash", currentHash.Hex()[:10])
			} else {
				// Ancestor not found - but might be available via snapshot cache
				log.Debug("Finalize: ancestor header not found, relying on snapshot cache",
					"number", currentNumber,
					"hash", currentHash.Hex()[:10])
				break
			}
		}

		parents = append(parents, parentHeader)

		// Stop if we've collected enough headers or reached genesis
		if currentNumber == 0 {
			break
		}

		// Check if we can find a snapshot for this parent (to avoid collecting all the way to genesis)
		if _, ok := h.recentSnaps.Get(currentHash); ok {
			log.Debug("Finalize: found snapshot in cache, stopping parent collection",
				"number", currentNumber,
				"hash", currentHash.Hex()[:10])
			break
		}

		// Move to next ancestor
		currentHash = parentHeader.ParentHash
		currentNumber--

		// Safety limit: don't collect more than checkpoint interval
		if len(parents) >= int(checkpointInterval) {
			log.Debug("Finalize: reached checkpoint interval limit, stopping parent collection",
				"collected", len(parents))
			break
		}
	}

	if len(parents) == 0 {
		log.Error("Parent not found", "block", header.Number, "hash", header.Hash().Hex()[:10], "parentHash", header.ParentHash.Hex()[:10])
		return errors.New("parent not found")
	}

	// CRITICAL: Reverse parents array for snapshotInternal
	// snapshotInternal expects parents in order from oldest to newest (parents[len-1] is newest)
	// But we collected them from newest to oldest, so reverse the array
	for i, j := 0, len(parents)-1; i < j; i, j = i+1, j-1 {
		parents[i], parents[j] = parents[j], parents[i]
	}

	log.Debug("Finalize: collected parent headers",
		"block", header.Number,
		"parentCount", len(parents),
		"oldestParent", parents[0].Number.Uint64(),
		"newestParent", parents[len(parents)-1].Number.Uint64())

	// CRITICAL FIX: Use snapshotWithFallback to ensure consistency with FinalizeAndAssemble
	// Both Finalize and FinalizeAndAssemble MUST use the same snapshot to calculate spoiledVal
	// Pass explicit parents chain to handle pipelined blocks not yet in canonical chain
	snap, err := h.snapshotWithFallback(chain, number-1, header.ParentHash, parents)
	if err != nil {
		log.Error("Finalize: failed to get snapshot", "error", err, "block", header.Number)
		return err
	}

	log.Warn("Finalize: got snapshot",
		"block", header.Number,
		"requestedNumber", number-1,
		"requestedHash", header.ParentHash.Hex()[:10],
		"returnedSnapNumber", snap.Number,
		"returnedSnapHash", snap.Hash.Hex()[:10],
		"validatorCount", len(snap.validators()),
		"turnLength", snap.TurnLength)
	nextForkHash := forkid.NextForkHash(h.chainConfig, h.genesisHash, chain.GenesisHeader().Time, number, header.Time)
	if !snap.isMajorityFork(hex.EncodeToString(nextForkHash[:])) {
		logger := log.Debug
		logger("there is a possible fork, and your client is not the majority. Please check...", "nextForkHash", hex.EncodeToString(nextForkHash[:]))
	}
	// If the block is an epoch end block, verify the validator list
	// The verification can only be done when the state is ready, it can't be done in VerifyHeader.
	// Use snapshot-based verification if parent block is not in canonical chain
	log.Debug("verifyValidators start", "block", header.Number, "hash", header.Hash().Hex()[:10])
	if err := h.verifyValidators(chain, header); err != nil {
		log.Error("Failed to verify validators", "error", err)
		return err
	}

	log.Debug("verifyValidatorsWithSnapshot end", "block", header.Number, "hash", header.Hash().Hex()[:10])

	if err := h.verifyTurnLength(chain, header); err != nil {
		log.Error("Failed to verify turn length", "error", err)
		return err
	}

	cx := chainContext{Chain: chain, parlia: h}

	// Parents are ordered oldest-to-newest for snapshot application. Consensus
	// state transitions must use the direct parent, i.e. the final element.
	parent := parents[len(parents)-1]

	// DEBUG: Initial state root in Finalize
	initialRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[Finalize] Initial state root",
		"block", header.Number,
		"hash", header.Hash().Hex()[:10],
		"parentRoot", parent.Root.Hex()[:10],
		"initialRoot", initialRoot.Hex()[:10])

	systemcontracts.TryUpdateBuildInSystemContract(h.chainConfig, header.Number, parent.Time, header.Time, state, false)

	// DEBUG: State root after system contract upgrade
	afterUpgradeRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[Finalize] After TryUpdateBuildInSystemContract",
		"block", header.Number,
		"stateRoot", afterUpgradeRoot.Hex()[:10])

	if err := h.checkNanoBlackList(state, header); err != nil {
		return err
	}

	// DEBUG: After checkNanoBlackList
	afterBlackListRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[Finalize] After checkNanoBlackList",
		"block", header.Number,
		"stateRoot", afterBlackListRoot.Hex()[:10])

	if h.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
		err := h.initializeFeynmanContract(state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer)
		if err != nil {
			log.Error("init feynman contract failed", "error", err)
		}
		// DEBUG: After initializeFeynmanContract
		afterFeynmanRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
		log.Warn("[Finalize] After initializeFeynmanContract",
			"block", header.Number,
			"stateRoot", afterFeynmanRoot.Hex()[:10])
	}

	// No block rewards in PoA, so the state remains as is and uncles are dropped
	if header.Number.Cmp(common.Big1) == 0 {
		err := h.initContract(state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer)
		if err != nil {
			log.Error("init contract failed")
		}
		// DEBUG: After initContract
		afterInitRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
		log.Warn("[Finalize] After initContract",
			"block", header.Number,
			"stateRoot", afterInitRoot.Hex()[:10])
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		// CRITICAL: Calculate inturn validator based on parent block number, not snapshot number
		parentNumber := header.Number.Uint64() - 1
		log.Warn("Finalize: calculating spoiledVal",
			"block", header.Number,
			"parentNumber", parentNumber,
			"snapNumber", snap.Number,
			"snapHash", snap.Hash.Hex()[:10],
			"turnLength", snap.TurnLength,
			"validatorCount", len(snap.validators()))

		spoiledVal := snap.inturnValidatorAt(parentNumber)
		log.Warn("Finalize: calculated spoiledVal",
			"block", header.Number,
			"spoiledVal", spoiledVal.Hex(),
			"parentNumber", parentNumber,
			"snapNumber", snap.Number)

		signedRecently := false
		if h.chainConfig.IsPlato(header.Number) {
			signedRecently = snap.SignRecently(spoiledVal)
			log.Warn("Finalize: SignRecently check",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex(),
				"signedRecently", signedRecently,
				"recentsCount", len(snap.Recents),
				"method", "Plato-SignRecently")
		} else {
			for blockNum, recent := range snap.Recents {
				log.Debug("Finalize: Recents entry",
					"blockNum", blockNum,
					"validator", recent.Hex())
				if recent == spoiledVal {
					signedRecently = true
					break
				}
			}
			log.Warn("Finalize: SignRecently check",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex(),
				"signedRecently", signedRecently,
				"recentsCount", len(snap.Recents),
				"method", "Legacy-Recents")
		}

		if !signedRecently {
			// REMOVED: slash functionality disabled
			// log.Trace("slash validator", "block hash", header.Hash(), "address", spoiledVal)
			// err = h.slash(spoiledVal, state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer)
			// if err != nil {
			// 	log.Error("slash validator failed", "block hash", header.Hash(), "address", spoiledVal, "err", err)
			// }

			log.Debug("Double sign detected but slash disabled",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex())

			// DEBUG: state root without slash
			afterSlashRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
			log.Warn("[Finalize] After slash check (slash disabled)",
				"block", header.Number,
				"stateRoot", afterSlashRoot.Hex()[:10])
		}
	}

	val := header.Coinbase
	PenalizeForDelayMining, err := h.isIntentionalDelayMining(chain, header)
	if err != nil {
		log.Debug("unexpected error happened when detecting intentional delay mining", "err", err)
	}
	if PenalizeForDelayMining {
		intentionalDelayMiningCounter.Inc(1)
		log.Warn("intentional delay mining detected", "validator", val, "number", header.Number, "hash", header.Hash())
	}
	err = h.distributeIncoming(val, state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer)
	if err != nil {
		return err
	}

	// DEBUG: After distributeIncoming
	afterDistributeRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[Finalize] After distributeIncoming",
		"block", header.Number,
		"stateRoot", afterDistributeRoot.Hex()[:10])

	if h.chainConfig.IsPlato(header.Number) {
		if err := h.distributeFinalityReward(chain, state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer); err != nil {
			return err
		}
		// DEBUG: After distributeFinalityReward
		afterFinalityRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
		log.Warn("[Finalize] After distributeFinalityReward",
			"block", header.Number,
			"stateRoot", afterFinalityRoot.Hex()[:10])
	}

	// update validators every day
	if h.chainConfig.IsFeynman(header.Number, header.Time) && isBreatheBlock(parent.Time, header.Time) {
		// we should avoid update validators in the Feynman upgrade block
		if !h.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
			if err := h.updateValidatorSetV2(state, header, cx, txs, receipts, systemTxs, usedGas, false, tracer); err != nil {
				return err
			}
			// DEBUG: After updateValidatorSetV2
			afterUpdateValidatorRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
			log.Warn("[Finalize] After updateValidatorSetV2",
				"block", header.Number,
				"stateRoot", afterUpdateValidatorRoot.Hex()[:10])

			// Sync validator connections after validator set update
			// validator connector removed; hs protocol handles connectivity
		}
	}

	// DEBUG: Final state root in Finalize
	finalRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[Finalize] FINAL state root",
		"block", header.Number,
		"finalRoot", finalRoot.Hex()[:10],
		"systemTxCount", len(*systemTxs))

	if len(*systemTxs) > 0 {
		return errors.New("the length of systemTxs do not match")
	}
	return nil
}

// FinalizeAndAssemble implements consensus.Engine, ensuring no uncles are set,
// nor block rewards given, and returns the final block.
func (h *Hotstuff) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB,
	body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error) {
	// No block rewards in PoA, so the state remains as is and uncles are dropped
	cx := chainContext{Chain: chain, parlia: h}

	if body.Transactions == nil {
		body.Transactions = make([]*types.Transaction, 0)
	}
	if receipts == nil {
		receipts = make([]*types.Receipt, 0)
	}

	// CRITICAL FIX: Get parent with fallback for Chained HotStuff
	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		log.Debug("FinalizeAndAssemble: parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10])

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
			log.Debug("FinalizeAndAssemble: got parent header from HotStuff state",
				"parentHash", header.ParentHash.Hex()[:10],
				"parentNumber", parent.Number.Uint64())
		}
	}

	if parent == nil {
		log.Error("FinalizeAndAssemble: parent not found", "block", header.Number, "parentHash", header.ParentHash.Hex()[:10])
		return nil, nil, errors.New("parent not found")
	}

	systemcontracts.TryUpdateBuildInSystemContract(h.chainConfig, header.Number, parent.Time, header.Time, state, false)

	if err := h.checkNanoBlackList(state, header); err != nil {
		return nil, nil, err
	}

	log.Warn("[FinalizeAndAssemble] After TryUpdateBuildInSystemContract",
		"block", header.Number,
		"stateRoot", state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number)).Hex()[:10])
	if h.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
		err := h.initializeFeynmanContract(state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer)
		if err != nil {
			log.Error("init feynman contract failed", "error", err)
		}
		log.Warn("[FinalizeAndAssemble] After initializeFeynmanContract",
			"block", header.Number,
			"stateRoot", state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number)).Hex()[:10])
	}

	if header.Number.Cmp(common.Big1) == 0 {
		err := h.initContract(state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer)
		if err != nil {
			log.Error("init contract failed")
		}
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		number := header.Number.Uint64()
		// CRITICAL FIX: Use snapshotWithFallback with explicit parents to match Finalize's snapshot
		// This ensures spoiledVal is consistent between Finalize and FinalizeAndAssemble
		parents := []*types.Header{parent}
		snap, err := h.snapshotWithFallback(chain, number-1, header.ParentHash, parents)
		if err != nil {
			log.Error("FinalizeAndAssemble: failed to get snapshot for slash", "error", err)
			return nil, nil, err
		}

		log.Warn("FinalizeAndAssemble: got snapshot",
			"block", header.Number,
			"requestedNumber", number-1,
			"requestedHash", header.ParentHash.Hex()[:10],
			"returnedSnapNumber", snap.Number,
			"returnedSnapHash", snap.Hash.Hex()[:10],
			"validatorCount", len(snap.validators()),
			"turnLength", snap.TurnLength)

		// CRITICAL: Calculate inturn validator based on parent block number, not snapshot number
		parentNumber := number - 1
		log.Warn("FinalizeAndAssemble: calculating spoiledVal",
			"block", header.Number,
			"parentNumber", parentNumber,
			"snapNumber", snap.Number,
			"snapHash", snap.Hash.Hex()[:10],
			"turnLength", snap.TurnLength,
			"validatorCount", len(snap.validators()))

		spoiledVal := snap.inturnValidatorAt(parentNumber)
		log.Warn("FinalizeAndAssemble: calculated spoiledVal",
			"block", header.Number,
			"spoiledVal", spoiledVal.Hex(),
			"parentNumber", parentNumber,
			"snapNumber", snap.Number)

		signedRecently := false
		if h.chainConfig.IsPlato(header.Number) {
			signedRecently = snap.SignRecently(spoiledVal)
			log.Warn("FinalizeAndAssemble: SignRecently check",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex(),
				"signedRecently", signedRecently,
				"recentsCount", len(snap.Recents),
				"method", "Plato-SignRecently")
		} else {
			for blockNum, recent := range snap.Recents {
				log.Debug("FinalizeAndAssemble: Recents entry",
					"blockNum", blockNum,
					"validator", recent.Hex())
				if recent == spoiledVal {
					signedRecently = true
					break
				}
			}
			log.Warn("FinalizeAndAssemble: SignRecently check",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex(),
				"signedRecently", signedRecently,
				"recentsCount", len(snap.Recents),
				"method", "Legacy-Recents")
		}
		if !signedRecently {
			// REMOVED: slash functionality disabled
			// err = h.slash(spoiledVal, state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer)
			// if err != nil {
			// 	log.Error("slash validator failed", "block hash", header.Hash(), "address", spoiledVal)
			// }

			log.Debug("Double sign detected but slash disabled",
				"block", header.Number,
				"spoiledVal", spoiledVal.Hex())

			// DEBUG: state root without slash
			afterSlashRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
			log.Warn("[FinalizeAndAssemble] After slash check (slash disabled)",
				"block", header.Number,
				"stateRoot", afterSlashRoot.Hex()[:10])
		}
	}

	err := h.distributeIncoming(h.ConsensusAddress(), state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer)
	if err != nil {
		return nil, nil, err
	}

	// DEBUG: After distributeIncoming
	afterDistributeRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
	log.Warn("[FinalizeAndAssemble] After distributeIncoming",
		"block", header.Number,
		"stateRoot", afterDistributeRoot.Hex()[:10])

	if h.chainConfig.IsPlato(header.Number) {
		if err := h.distributeFinalityReward(chain, state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer); err != nil {
			return nil, nil, err
		}
		// DEBUG: After distributeFinalityReward
		afterFinalityRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
		log.Warn("[FinalizeAndAssemble] After distributeFinalityReward",
			"block", header.Number,
			"stateRoot", afterFinalityRoot.Hex()[:10])
	}

	// update validators every day
	if h.chainConfig.IsFeynman(header.Number, header.Time) && isBreatheBlock(parent.Time, header.Time) {
		// we should avoid update validators in the Feynman upgrade block
		if !h.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
			if err := h.updateValidatorSetV2(state, header, cx, &body.Transactions, &receipts, nil, &header.GasUsed, true, tracer); err != nil {
				return nil, nil, err
			}
			// DEBUG: After updateValidatorSetV2
			afterUpdateValidatorRoot := state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number))
			log.Warn("[FinalizeAndAssemble] After updateValidatorSetV2",
				"block", header.Number,
				"stateRoot", afterUpdateValidatorRoot.Hex()[:10])

			// Sync validator connections after validator set update
			// validator connector removed; hs protocol handles connectivity
		}
	}

	// should not happen. Once happen, stop the node is better than broadcast the block
	if header.GasLimit < header.GasUsed {
		return nil, nil, errors.New("gas consumption of system txs exceed the gas limit")
	}
	header.UncleHash = types.EmptyUncleHash
	var blk *types.Block
	var rootHash common.Hash
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		rootHash = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
		wg.Done()
	}()
	go func() {
		blk = types.NewBlock(header, body, receipts, trie.NewStackTrie(nil))
		wg.Done()
	}()
	wg.Wait()
	blk.SetRoot(rootHash)

	// DEBUG: Final state root
	log.Warn("[FinalizeAndAssemble] FINAL state root",
		"block", header.Number,
		"finalRoot", rootHash.Hex()[:10],
		"txCount", len(body.Transactions),
		"receiptCount", len(receipts))

	// Assemble and return the final block for sealing
	return blk, receipts, nil
}

func (h *Hotstuff) IsActiveValidatorAt(chain consensus.ChainHeaderReader, header *types.Header, checkVoteKeyFn func(bLSPublicKey *types.BLSPublicKey) bool) bool {
	number := header.Number.Uint64()
	snap, err := h.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Error("failed to get the snapshot from consensus", "error", err)
		return false
	}
	validators := snap.Validators
	validatorInfo, ok := validators[h.ConsensusAddress()]

	return ok && (checkVoteKeyFn == nil || (validatorInfo != nil && checkVoteKeyFn(&validatorInfo.VoteAddress)))
}

// VerifyVote will verify: 1. If the vote comes from valid validators 2. If the vote's sourceNumber and sourceHash are correct
func (h *Hotstuff) VerifyVote(chain consensus.ChainHeaderReader, vote *types.VoteEnvelope) error {
	targetNumber := vote.Data.TargetNumber
	targetHash := vote.Data.TargetHash
	header := chain.GetVerifiedBlockByHash(targetHash)
	if header == nil {
		log.Warn("BlockHeader at current voteBlockNumber is nil", "targetNumber", targetNumber, "targetHash", targetHash)
		return errors.New("BlockHeader at current voteBlockNumber is nil")
	}
	if header.Number.Uint64() != targetNumber {
		log.Warn("unexpected target number", "expect", header.Number.Uint64(), "real", targetNumber)
		return errors.New("target number mismatch")
	}

	number := header.Number.Uint64()
	snap, err := h.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Error("failed to get the snapshot from consensus", "error", err)
		return errors.New("failed to get the snapshot from consensus")
	}

	validators := snap.Validators
	voteAddress := vote.VoteAddress
	local := h.ConsensusAddress()
	for addr, validator := range validators {
		if validator.VoteAddress == voteAddress {
			if addr == local {
				validVotesfromSelfCounter.Inc(1)
			}
			metrics.GetOrRegisterCounter(fmt.Sprintf("parlia/VerifyVote/%s", addr.String()), nil).Inc(1)
			return nil
		}
	}

	return errors.New("vote verification failed")
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (h *Hotstuff) Authorize(val common.Address, signFn SignerFn, signTxFn SignerTxFn) {
	h.authorized.Store(&authorizedSigner{val: val, signFn: signFn, signTxFn: signTxFn})
}

// Argument leftOver is the time reserved for block finalize(calculate root, distribute income...)
func (h *Hotstuff) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	number := header.Number.Uint64()
	snap, err := h.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Debug("Failed to resolve exact parent snapshot in Delay", "parentHash", header.ParentHash, "parentNumber", number-1, "err", err)
		return nil
	}
	delay := time.Until(time.UnixMilli(int64(header.MilliTimestamp())))

	if *leftOver >= time.Duration(snap.BlockInterval)*time.Millisecond {
		// ignore invalid leftOver
		log.Error("Delay invalid argument", "leftOver", leftOver.String(), "Period", snap.BlockInterval)
	} else if *leftOver >= delay {
		delay = time.Duration(0)
		return &delay
	} else {
		delay = delay - *leftOver
	}
	return &delay
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (h *Hotstuff) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	header := block.Header()

	log.Info("[Seal] ENTER - starting seal process",
		"blockNumber", header.Number.Uint64(),
		"blockHash", block.Hash().Hex()[:10],
		"parentHash", header.ParentHash.Hex()[:10])

	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		log.Info("[Seal] EXIT - genesis block, skipping")
		return errUnknownBlock
	}
	val, signFn, _ := h.signerCredentials()
	h.lock.RLock()
	st := h.getHsState()
	currentView := uint64(0)
	var highQCView uint64
	var highQCHash common.Hash
	if st != nil {
		currentView = st.currentView
		if st.highQC != nil {
			highQCView = st.highQC.View
			highQCHash = st.highQC.BlockHash
		}
	}
	h.lock.RUnlock()
	if signFn == nil {
		return errors.New("HotStuff signer is not configured")
	}

	log.Info("[Seal] Got HotStuff state",
		"currentView", currentView,
		"highQCView", highQCView,
		"highQCHash", highQCHash.Hex()[:10],
		"validator", val.Hex())

	// CRITICAL FIX: Use snapshotWithFallback for Chained HotStuff pipelining
	snap, err := h.snapshotWithFallback(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Warn("[Seal] EXIT - could not get snapshot", "block", number, "parentHash", header.ParentHash.Hex()[:10], "err", err)
		return err
	}
	log.Info("[Seal] Got snapshot", "snapNumber", snap.Number, "validatorCount", len(snap.Validators))

	// only for test
	// // Bail out if we're unauthorized to sign a block
	// if _, authorized := snap.Validators[val]; !authorized {
	// 	return errUnauthorizedValidator(val.String())
	// }

	// HotStuff: Check if we are the leader for current view
	// Non-leaders should not propose to avoid "Ignore proposal from non-leader" errors
	log.Info("[Seal] Checking if we are leader", "currentView", currentView, "self", val.Hex())
	leader, err := h.getLeaderForViewAt(chain, header.ParentHash, currentView)
	if err != nil {
		return fmt.Errorf("resolve HotStuff leader for view %d: %w", currentView, err)
	}
	log.Info("[Seal] Leader check result", "leader", leader.Hex(), "self", val.Hex(), "isLeader", leader == val)
	isLeader := (leader == val)

	if !isLeader {
		// Not the leader for this view - don't propose
		// Wait for view timeout, then view will advance and we may become leader
		log.Info("[Seal] EXIT - Not leader for current view, skipping proposal",
			"view", currentView, "blockNum", number, "leader", leader.Hex(), "self", val.Hex())

		// Return the block immediately to unblock miner, but don't broadcast
		// The block won't be accepted by other nodes anyway since we're not the leader
		// Alternative: we could wait for view timeout here and retry
		// For now, just skip and let view timeout mechanism handle view change
		return nil
	}

	// CRITICAL FIX: Check if block's parent matches current highQC
	// If highQC was updated after block construction, this block is stale
	log.Info("[Seal] We are leader! Checking parent match with highQC", "view", currentView)
	parentHeader := chain.GetHeaderByHash(header.ParentHash)
	if parentHeader == nil {
		if parentBlock := h.getBlockWithoutStateLock(header.ParentHash); parentBlock != nil {
			parentHeader = parentBlock.Header()
		}
	}
	h.lock.RLock()
	st = h.getHsState()
	if h.chainConfig.IsHotstuff(header.Number) {
		bootstrap := st != nil && st.highQC != nil && h.isBootstrapHeaderHighQC(header, parentHeader, st.highQC.BlockHash, st.highQC.View)
		if st == nil || st.highQC == nil || (!st.highQC.hasAggregateProof() && !bootstrap) {
			log.Warn("[Seal] EXIT - missing verified highQC after HotStuff activation",
				"blockNumber", number,
				"view", currentView)
			h.lock.RUnlock()
			return nil
		}
	}
	if st != nil && st.highQC != nil {
		if header.ParentHash != st.highQC.BlockHash {
			// Check if highQC block actually exists (in chain or HotStuff state)
			// If highQC block doesn't exist, GetProposalParent would have fallen back to chain head
			// In that case, we should allow using chain head as parent
			highQCBlockExists := false
			if h.chain.GetHeaderByHash(st.highQC.BlockHash) != nil {
				highQCBlockExists = true
			} else if st.proposalsByHashBlock[st.highQC.BlockHash] != nil {
				highQCBlockExists = true
			}

			if highQCBlockExists {
				// highQC block exists but parent doesn't match - block is truly stale
				log.Warn("[Seal] EXIT - Block parent doesn't match current highQC, block is stale",
					"blockNumber", number,
					"blockParent", header.ParentHash.Hex()[:10],
					"highQCBlock", st.highQC.BlockHash.Hex()[:10],
					"highQCView", st.highQC.View,
					"currentView", currentView)
				h.lock.RUnlock()
				return nil
			} else {
				// highQC block doesn't exist (node is behind, QC arrived before block proposal)
				// CRITICAL: Do NOT use chain head as it would cause a fork!
				// The correct behavior is to skip this round and wait for:
				// 1. The missing proposal to arrive (will be cached and processed)
				// 2. View timeout to trigger TC and advance view
				log.Warn("[Seal] EXIT - highQC block not found, skipping proposal to prevent fork",
					"blockNumber", number,
					"blockParent", header.ParentHash.Hex()[:10],
					"highQCBlock", st.highQC.BlockHash.Hex()[:10],
					"highQCView", st.highQC.View,
					"currentView", currentView,
					"note", "Waiting for missing proposal or view timeout")
				h.lock.RUnlock()
				return nil
			}
		} else {
			log.Info("[Seal] Parent matches highQC")
		}
		log.Info("[Seal] Checking for duplicate proposal")

		// CRITICAL FIX: Check if current view already has a proposal
		// In HotStuff, each view should have only ONE proposal from the leader
		// If we already proposed for this view, don't propose again
		if existingProposal, exists := st.proposalsByView[currentView]; exists {
			if existingProposal.Hash() != header.Hash() {
				log.Warn("[Seal] EXIT - Current view already has a different proposal, skipping duplicate",
					"view", currentView,
					"existingBlock", existingProposal.Hash().Hex()[:10],
					"existingNumber", existingProposal.Number.Uint64(),
					"newBlock", header.Hash().Hex()[:10],
					"newNumber", number)
				h.lock.RUnlock()
				return nil
			} else {
				log.Info("[Seal] EXIT - Current view already has same proposal, skipping duplicate",
					"view", currentView,
					"block", header.Hash().Hex()[:10])
				h.lock.RUnlock()
				return nil
			}
		}
		log.Info("[Seal] No duplicate proposal found, proceeding")
	} else {
		log.Info("[Seal] No highQC check (st or highQC is nil)")
	}
	h.lock.RUnlock()

	log.Info("[Seal] ✅ All checks passed! Leader for current view, proceeding with proposal",
		"view", currentView, "blockNum", number, "val", val.Hex())

	delay := h.delayForRamanujanFork(snap, header)

	log.Info("[Seal] Calculated delay", "number", number, "delay", delay, "headerDifficulty", header.Difficulty)

	// Wait until sealing is terminated or delay timeout.
	log.Info("[Seal] Starting goroutine to wait for delay and broadcast", "delay", delay)
	go func() {
		log.Info("[Seal goroutine] Started, waiting for delay or stop", "delay", delay)
		select {
		case <-stop:
			log.Info("[Seal goroutine] Received stop signal, exiting")
			return
		case <-time.After(delay):
			log.Info("[Seal goroutine] Delay expired, proceeding with seal")
		}

		// Assemble SyncInfo (HighQC/HighTC) for HotStuff
		log.Info("[Seal goroutine] Assembling HighQC")
		err := h.assembleHighQC(chain, header)
		if err != nil {
			/* If the highQC can't be assembled successfully, the blockchain won't get
			   fast finalized, but it can be tolerated, so just report this error here. */
			log.Error("[Seal goroutine] Assemble highQC failed", "err", err)
		} else {
			log.Info("[Seal goroutine] HighQC assembled successfully")
		}

		// Assemble and attach TimeoutCert if we have sufficient timeout messages
		log.Info("[Seal goroutine] Assembling HighTC")
		err = h.assembleHighTC(chain, header)
		if err != nil {
			log.Error("[Seal goroutine] Assemble highTC failed", "err", err)
		} else {
			log.Info("[Seal goroutine] HighTC assembled (or no TC needed)")
		}

		headerView := getViewFromHeader(header, h.chainConfig)
		h.lock.RLock()
		stateView := currentView
		if st := h.getHsState(); st != nil {
			stateView = st.currentView
		}
		h.lock.RUnlock()
		if headerView != stateView {
			log.Warn("[Seal goroutine] EXIT - header view mismatch current view",
				"headerView", headerView,
				"currentView", stateView,
				"blockNumber", header.Number.Uint64(),
				"blockHash", header.Hash().Hex()[:10])
			return
		}

		// Sign all the things!
		log.Info("[Seal goroutine] Signing block header")
		sig, err := signFn(accounts.Account{Address: val}, accounts.MimetypeParlia, HotstuffRLP(header, h.chainConfig.ChainID))
		if err != nil {
			log.Error("[Seal goroutine] Sign for the block header failed", "err", err)
			return
		}
		log.Info("[Seal goroutine] Block signed successfully", "sigLen", len(sig))

		sealer := make([]byte, extraSeal)
		copy(sealer, sig)
		x := header.Extra
		copy(x[len(x)-extraSeal:], sealer)
		block := block.WithSeal(header)
		commitWaiter := h.registerCommitWaiter(block.Hash())
		defer h.unregisterCommitWaiter(block.Hash(), commitWaiter)
		log.Info("[Seal goroutine] Block sealed with signature")

		// Broadcast HotStuff proposal from miner path using the sealed header and body
		log.Info("[Seal goroutine] Encoding header for proposal broadcast")
		if encHead, err := rlp.EncodeToBytes(header); err == nil {
			log.Info("[Seal goroutine] Header encoded successfully", "encLen", len(encHead))

			// Encode body: only transactions are needed for replicas to reconstruct full block
			body := &types.Body{Transactions: block.Transactions()}
			encBody, berr := rlp.EncodeToBytes(body)
			if berr != nil {
				log.Warn("[Seal goroutine] Failed to encode body for HS proposal broadcast", "err", berr)
				return
			}
			log.Info("[Seal goroutine] Body encoded successfully", "encLen", len(encBody), "txCount", len(body.Transactions))

			// Use current HotStuff view as ProposalPacket.Number (view number)
			view := headerView
			log.Info("[Seal goroutine] Creating proposal packet", "view", view, "blockHash", header.Hash().Hex()[:10])

			hasSyncInfo, highQCView, highQCHash, _, highQCSigners, highQCSig, _ := parseSyncInfoWithProof(header, h.chainConfig)
			if !hasSyncInfo {
				log.Warn("[Seal goroutine] Refusing to broadcast proposal without SyncInfo", "block", header.Hash())
				return
			}
			prop := &hs.ProposalPacket{
				ParentHash: header.ParentHash,
				BlockHash:  header.Hash(),
				HighQC: hs.HsQC{
					BlockHash: highQCHash, View: highQCView,
					SignersSet: highQCSigners, Sig: common.CopyBytes(highQCSig),
				},
				View: view, HeaderRLP: encHead, BodyRLP: encBody,
			}

			log.Info("[Seal goroutine] Processing own proposal locally", "view", view, "block", header.Hash().Hex()[:8])
			if err := h.OnHsProposal("self", prop); err != nil {
				log.Warn("[Seal goroutine] Failed to process own proposal", "err", err)
				return
			}
			h.lock.RLock()
			acceptedLocally := false
			if st := h.getHsState(); st != nil {
				acceptedLocally = st.proposalsByHashBlock[block.Hash()] != nil
			}
			h.lock.RUnlock()
			if !acceptedLocally {
				log.Warn("[Seal goroutine] Local consensus rejected proposal; skipping broadcast", "block", block.Hash())
				return
			}

			log.Info("[Seal goroutine] Broadcasting proposal to replicas")
			if err := h.broadcastHsProposal(prop); err != nil {
				// A local quorum may still make progress, but an uncommitted block
				// must never be returned to the miner.
				log.Warn("[Seal goroutine] HS proposal broadcast failed; waiting for commit", "err", err)
			}
			log.Info("[Seal goroutine] ✅ Proposal broadcast successful!")
		} else {
			log.Warn("[Seal goroutine] Failed to encode header for HS proposal broadcast", "err", err)
			return
		}
		// Wait for commit signal from HotStuff consensus (3-chain rule)
		log.Info("[Seal goroutine] Waiting for commit signal from 3-chain rule", "blockHash", block.Hash().Hex()[:10])
		for {
			select {
			case result := <-commitWaiter:
				// if the block hash and the hash from channel are the same,
				// return the result. Otherwise, keep waiting the next hash.
				if result != nil && block.Hash() == result.Hash() {
					results <- result
					log.Info("[Seal goroutine] ✅ Block committed via HotStuff 3-chain rule",
						"hash", result.Hash().Hex(),
						"number", result.NumberU64())
					return
				} else {
					log.Warn("[Seal goroutine] Commit signal for different block, continuing to wait")
					continue
				}
			case <-stop:
				log.Warn("[Seal goroutine] Sealing stopped before commit", "sealhash", h.SealHash(header).Hex()[:10])
				return
			}
		}
	}()
	log.Info("[Seal] Seal goroutine started, returning from Seal function")
	return nil
}

func (h *Hotstuff) shouldWaitForCurrentBlockProcess(chain consensus.ChainHeaderReader, header *types.Header, snap *Snapshot) bool {
	if header.Difficulty.Cmp(diffInTurn) == 0 {
		return false
	}

	highestVerifiedHeader := chain.GetHighestVerifiedHeader()
	if highestVerifiedHeader == nil {
		return false
	}

	if header.ParentHash == highestVerifiedHeader.ParentHash {
		return true
	}
	return false
}

func (h *Hotstuff) EnoughDistance(chain consensus.ChainReader, header *types.Header) bool {
	snap, err := h.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return true
	}
	return snap.enoughDistance(h.ConsensusAddress(), header)
}

func (h *Hotstuff) IsLocalBlock(header *types.Header) bool {
	return h.ConsensusAddress() == header.Coinbase
}

func (h *Hotstuff) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return common.Big1
}

func encodeSigHeaderWithoutVoteAttestation(w io.Writer, header *types.Header, chainId *big.Int) {
	err := rlp.Encode(w, []interface{}{
		chainId,
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:extraVanity], // this will panic if extra is too short, should check before calling encodeSigHeaderWithoutVoteAttestation
		header.MixDigest,
		header.Nonce,
	})
	if err != nil {
		panic("can't encode: " + err.Error())
	}
}

// SealHash returns the hash of a block without vote attestation prior to it being sealed.
// So it's not the real hash of a block, just used as unique id to distinguish task
func (h *Hotstuff) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeaderWithoutVoteAttestation(hasher, header, h.chainConfig.ChainID)
	hasher.Sum(hash[:0])
	return hash
}

// APIs implements consensus.Engine, returning the user facing RPC API to query snapshot.
func (h *Hotstuff) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{{
		Namespace: "parlia",
		Version:   "1.0",
		Service:   &API{chain: chain, parlia: h},
		Public:    false,
	}}
}

func (h *Hotstuff) Close() error {
	h.lock.Lock()
	h.closed = true
	if h.hsTimer != nil {
		h.hsTimer.Stop()
		h.hsTimer = nil
	}
	h.lock.Unlock()
	return nil
}

// ==========================  interaction with contract/account =========

// getCurrentValidators get current validators
// getCurrentValidators gets current validators at the given header's state
// This function uses callContractAtState to support queries on uncommitted parent blocks (HotStuff pipelining)
func (h *Hotstuff) getCurrentValidators(header *types.Header) ([]common.Address, map[common.Address]*types.BLSPublicKey, error) {
	if !h.chainConfig.IsLuban(header.Number) {
		validators, err := h.getCurrentValidatorsBeforeLuban(header)
		return validators, nil, err
	}

	method := "getMiningValidators"
	contractAddr := common.HexToAddress(systemcontracts.ValidatorContract)

	data, err := h.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("[getCurrentValidators] Unable to pack tx for getMiningValidators", "error", err)
		return nil, nil, err
	}

	// Use callContractAtState to query contract at header's state
	// This works even if the block is not committed yet (Prepare phase)
	result, err := h.callContractAtState(header, contractAddr, data)
	if err != nil {
		log.Error("[getCurrentValidators] callContractAtState failed",
			"error", err,
			"blockNumber", header.Number.Uint64(),
			"contract", systemcontracts.ValidatorContract)
		return nil, nil, err
	}

	var valSet []common.Address
	var voteAddrSet []types.BLSPublicKey
	if err := h.validatorSetABI.UnpackIntoInterface(&[]interface{}{&valSet, &voteAddrSet}, method, result); err != nil {
		return nil, nil, fmt.Errorf("failed to unpack result: %w", err)
	}

	voteAddrMap := make(map[common.Address]*types.BLSPublicKey, len(valSet))
	for i := 0; i < len(valSet); i++ {
		voteAddrMap[valSet[i]] = &(voteAddrSet)[i]
	}

	log.Debug("[getCurrentValidators] successfully queried validators",
		"blockNumber", header.Number.Uint64(),
		"validatorCount", len(valSet))

	return valSet, voteAddrMap, nil
}

func (h *Hotstuff) isIntentionalDelayMining(chain consensus.ChainHeaderReader, header *types.Header) (bool, error) {
	log.Debug("[isIntentionalDelayMining] ENTER",
		"block", header.Number,
		"parentHash", header.ParentHash.Hex()[:10])

	// CRITICAL FIX: Get parent with fallback for Chained HotStuff
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		log.Debug("[isIntentionalDelayMining] parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10])

		log.Debug("[isIntentionalDelayMining] calling GetBlockFromState (may acquire RLock)",
			"parentHash", header.ParentHash.Hex()[:10])
		parentBlock := h.GetBlockFromState(header.ParentHash)
		log.Debug("[isIntentionalDelayMining] GetBlockFromState returned",
			"parentHash", header.ParentHash.Hex()[:10],
			"found", parentBlock != nil)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent == nil {
		log.Debug("isIntentionalDelayMining: parent not found", "parentHash", header.ParentHash.Hex()[:10])
		return false, errors.New("parent not found")
	}
	blockInterval, err := h.BlockInterval(chain, header)
	if err != nil {
		return false, err
	}
	isIntentional := header.Coinbase == parent.Coinbase &&
		header.Difficulty == diffInTurn && parent.Difficulty == diffInTurn &&
		parent.MilliTimestamp()+blockInterval < header.MilliTimestamp()
	return isIntentional, nil
}

// distributeIncoming distributes system incoming of the block
func (h *Hotstuff) distributeIncoming(val common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	coinbase := header.Coinbase

	doDistributeSysReward := !h.chainConfig.IsKepler(header.Number, header.Time) &&
		state.GetBalance(common.HexToAddress(systemcontracts.SystemRewardContract)).Cmp(maxSystemBalance) < 0
	if doDistributeSysReward {
		balance := state.GetBalance(consensus.SystemAddress)
		rewards := new(uint256.Int)
		rewards = rewards.Rsh(balance, systemRewardPercent)
		if rewards.Cmp(common.U2560) > 0 {
			state.SetBalance(consensus.SystemAddress, balance.Sub(balance, rewards), tracing.BalanceChangeUnspecified)
			state.AddBalance(coinbase, rewards, tracing.BalanceChangeUnspecified)
			err := h.distributeToSystem(rewards.ToBig(), state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
			if err != nil {
				return err
			}
			log.Trace("distribute to system reward pool", "block hash", header.Hash(), "amount", rewards)
		}
	}

	balance := state.GetBalance(consensus.SystemAddress)
	if balance.Cmp(common.U2560) <= 0 {
		return nil
	}

	state.SetBalance(consensus.SystemAddress, common.U2560, tracing.BalanceDecreaseBSCDistributeReward)
	state.AddBalance(coinbase, balance, tracing.BalanceIncreaseBSCDistributeReward)
	log.Trace("distribute to validator contract", "block hash", header.Hash(), "amount", balance)
	return h.distributeToValidator(balance.ToBig(), val, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// slash spoiled validators
func (h *Hotstuff) slash(spoiledVal common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// method
	method := "slash"

	// get packed data
	data, err := h.slashABI.Pack(method,
		spoiledVal,
	)
	if err != nil {
		log.Error("Unable to pack tx for slash", "error", err)
		return err
	}
	// get system message
	msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.SlashContract), data, common.Big0)
	// apply message
	return h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// init contract
func (h *Hotstuff) initContract(state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// method
	method := "init"
	// contracts
	contracts := []string{
		systemcontracts.ValidatorContract,
		systemcontracts.SlashContract,
		systemcontracts.LightClientContract,
		systemcontracts.RelayerHubContract,
		systemcontracts.TokenHubContract,
		systemcontracts.RelayerIncentivizeContract,
		systemcontracts.CrossChainContract,
	}
	// get packed data
	data, err := h.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for init validator set", "error", err)
		return err
	}
	for _, c := range contracts {
		msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(c), data, common.Big0)
		// apply message
		log.Trace("init contract", "block hash", header.Hash(), "contract", c)
		err = h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *Hotstuff) distributeToSystem(amount *big.Int, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// get system message
	msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.SystemRewardContract), nil, amount)
	// apply message
	return h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// distributeToValidator deposits validator reward to validator contract
func (h *Hotstuff) distributeToValidator(amount *big.Int, validator common.Address,
	state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// method
	method := "deposit"

	// get packed data
	data, err := h.validatorSetABI.Pack(method,
		validator,
	)
	if err != nil {
		log.Error("Unable to pack tx for deposit", "error", err)
		return err
	}
	// get system message
	msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, amount)
	// apply message
	return h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// get system message
func (h *Hotstuff) getSystemMessage(from, toAddress common.Address, data []byte, value *big.Int) *core.Message {
	return &core.Message{
		From:     from,
		GasLimit: math.MaxUint64 / 2,
		GasPrice: big.NewInt(0),
		Value:    value,
		To:       &toAddress,
		Data:     data,
	}
}

func (h *Hotstuff) applyTransaction(
	msg *core.Message,
	state vm.StateDB,
	header *types.Header,
	chainContext core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt,
	receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool,
	tracer *tracing.Hooks,
) (applyErr error) {
	nonce := state.GetNonce(msg.From)
	expectedTx := types.NewTransaction(nonce, *msg.To, msg.Value, msg.GasLimit, msg.GasPrice, msg.Data)
	expectedHash := h.signer.Hash(expectedTx)

	val, _, signTxFn := h.signerCredentials()
	if msg.From == val && mining {
		if signTxFn == nil {
			return errors.New("HotStuff transaction signer is not configured")
		}
		var err error
		expectedTx, err = signTxFn(accounts.Account{Address: msg.From}, expectedTx, h.chainConfig.ChainID)
		if err != nil {
			return err
		}
	} else {
		if receivedTxs == nil || len(*receivedTxs) == 0 || (*receivedTxs)[0] == nil {
			return errors.New("supposed to get a actual transaction, but get none")
		}
		actualTx := (*receivedTxs)[0]
		if !bytes.Equal(h.signer.Hash(actualTx).Bytes(), expectedHash.Bytes()) {
			return fmt.Errorf("expected tx hash %v, get %v, nonce %d, to %s, value %s, gas %d, gasPrice %s, data %s,actual nonce %d,actual to %s,actual value %s,actual gas %d,actual gasPrice %s,actual data %s", expectedHash.String(), actualTx.Hash().String(),
				expectedTx.Nonce(),
				expectedTx.To().String(),
				expectedTx.Value().String(),
				expectedTx.Gas(),
				expectedTx.GasPrice().String(),
				hex.EncodeToString(expectedTx.Data()),
				actualTx.Nonce(),
				actualTx.To().String(),
				actualTx.Value().String(),
				actualTx.Gas(),
				actualTx.GasPrice().String(),
				hex.EncodeToString(actualTx.Data()),
			)
		}
		expectedTx = actualTx
		// move to next
		*receivedTxs = (*receivedTxs)[1:]
	}
	state.SetTxContext(expectedTx.Hash(), len(*txs))

	// Create a new context to be used in the EVM environment
	context := core.NewEVMBlockContext(header, chainContext, nil)
	// Create a new environment which holds all relevant information
	// about the transaction and calling mechanisms.
	evm := vm.NewEVM(context, state, h.chainConfig, vm.Config{Tracer: tracer})
	evm.SetTxContext(core.NewEVMTxContext(msg))

	// Tracing receipt will be set if there is no error and will be used to trace the transaction
	var tracingReceipt *types.Receipt
	if tracer != nil {
		if tracer.OnSystemTxStart != nil {
			tracer.OnSystemTxStart()
		}
		if tracer.OnTxStart != nil {
			tracer.OnTxStart(evm.GetVMContext(), expectedTx, msg.From)
		}

		// Defers are last in first out, so OnTxEnd will run before OnSystemTxEnd in this transaction,
		// which is what we want.
		if tracer.OnSystemTxEnd != nil {
			defer func() {
				tracer.OnSystemTxEnd()
			}()
		}
		if tracer.OnTxEnd != nil {
			defer func() {
				tracer.OnTxEnd(tracingReceipt, applyErr)
			}()
		}
	}

	gasUsed, err := applyMessage(msg, evm, state, header, h.chainConfig, chainContext)
	if err != nil {
		return err
	}
	*txs = append(*txs, expectedTx)
	var root []byte
	if h.chainConfig.IsByzantium(header.Number) {
		state.Finalise(true)
	} else {
		root = state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number)).Bytes()
	}
	*usedGas += gasUsed
	tracingReceipt = types.NewReceipt(root, false, *usedGas)
	tracingReceipt.TxHash = expectedTx.Hash()
	tracingReceipt.GasUsed = gasUsed

	// Set the receipt logs and create a bloom for filtering
	tracingReceipt.Logs = state.GetLogs(expectedTx.Hash(), header.Number.Uint64(), header.Hash(), header.Time)
	tracingReceipt.Bloom = types.CreateBloom(tracingReceipt)
	tracingReceipt.BlockHash = header.Hash()
	tracingReceipt.BlockNumber = header.Number
	tracingReceipt.TransactionIndex = uint(state.TxIndex())
	*receipts = append(*receipts, tracingReceipt)
	return nil
}

// GetFinalizedHeader returns highest finalized block header.
func (h *Hotstuff) GetFinalizedHeader(chain consensus.ChainHeaderReader, header *types.Header) *types.Header {
	if chain == nil || header == nil {
		return nil
	}
	if finalized := h.finalizedHeaderOnBranch(chain, header, nil); finalized != nil {
		return finalized
	}
	return chain.GetHeaderByNumber(0)
}

// GetJustifiedNumberAndHash retrieves the number and hash of the highest justified block
// within the branch including `headers` and utilizing the latest element as the head.
func (h *Hotstuff) GetJustifiedNumberAndHash(chain consensus.ChainHeaderReader, headers []*types.Header) (uint64, common.Hash, error) {
	if chain == nil || len(headers) == 0 || headers[len(headers)-1] == nil {
		return 0, common.Hash{}, errors.New("illegal chain or header")
	}
	head := headers[len(headers)-1]
	if finalized := h.finalizedHeaderOnBranch(chain, head, headers); finalized != nil {
		return finalized.Number.Uint64(), finalized.Hash(), nil
	}
	genesis := chain.GetHeaderByNumber(0)
	if genesis == nil {
		return 0, common.Hash{}, errors.New("genesis header not found")
	}
	return 0, genesis.Hash(), nil
}

// finalizedHeaderOnBranch returns the highest block at or below head that is
// known finalized by the durable marker. The marker can be either before head
// (normal latest-head queries) or after head (historical queries).
func (h *Hotstuff) finalizedHeaderOnBranch(chain consensus.ChainHeaderReader, head *types.Header, headers []*types.Header) *types.Header {
	finalized := h.durableFinalizedHeader(chain)
	if finalized == nil {
		return nil
	}
	if headerDescendsFrom(chain, head, finalized, headers) {
		return finalized
	}
	if headerDescendsFrom(chain, finalized, head, nil) {
		return head
	}
	return nil
}

func (h *Hotstuff) durableFinalizedHeader(chain consensus.ChainHeaderReader) *types.Header {
	if provider, ok := chain.(interface{ CurrentFinalBlock() *types.Header }); ok {
		if finalized := provider.CurrentFinalBlock(); finalized != nil {
			return finalized
		}
	}
	if h.db != nil {
		if hash := rawdb.ReadFinalizedBlockHash(h.db); hash != (common.Hash{}) {
			if number := rawdb.ReadHeaderNumber(h.db, hash); number != nil {
				if header := rawdb.ReadHeader(h.db, hash, *number); header != nil {
					return header
				}
			}
		}
	}
	// SetFinalized is authoritative, but retain an in-process fallback until its
	// durable marker becomes visible through the chain reader.
	h.lock.RLock()
	var best *types.Header
	if st := h.getHsState(); st != nil {
		for hash, number := range st.committedBlocks {
			if best != nil && number <= best.Number.Uint64() {
				continue
			}
			if block := h.getBlockWithoutStateLock(hash); block != nil {
				best = block.Header()
			} else if candidate := chain.GetHeader(hash, number); candidate != nil {
				best = candidate
			}
		}
	}
	h.lock.RUnlock()
	return best
}

func headerDescendsFrom(chain consensus.ChainHeaderReader, head, ancestor *types.Header, headers []*types.Header) bool {
	if head == nil || ancestor == nil || head.Number.Uint64() < ancestor.Number.Uint64() {
		return false
	}
	byHash := make(map[common.Hash]*types.Header, len(headers))
	for _, header := range headers {
		if header != nil {
			byHash[header.Hash()] = header
		}
	}
	cursor := head
	for cursor.Number.Uint64() > ancestor.Number.Uint64() {
		if parent := byHash[cursor.ParentHash]; parent != nil {
			cursor = parent
		} else {
			cursor = chain.GetHeader(cursor.ParentHash, cursor.Number.Uint64()-1)
		}
		if cursor == nil {
			return false
		}
	}
	return cursor.Hash() == ancestor.Hash()
}

// BlockInterval returns number of blocks in one epoch for the given header
func (h *Hotstuff) epochLength(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) (uint64, error) {
	if header == nil {
		return defaultEpochLength, errUnknownBlock
	}
	if header.Number.Uint64() == 0 {
		return defaultEpochLength, nil
	}

	snap, err := h.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, parents)
	if err != nil {
		return defaultEpochLength, err
	}
	return snap.EpochLength, nil
}

// BlockInterval returns the block interval in milliseconds for the given header
func (h *Hotstuff) BlockInterval(chain consensus.ChainHeaderReader, header *types.Header) (uint64, error) {
	if header == nil {
		return defaultBlockInterval, errUnknownBlock
	}
	if header.Number.Uint64() == 0 {
		return defaultBlockInterval, nil
	}

	// CRITICAL FIX: Use snapshotWithFallback for Chained HotStuff pipelining
	snap, err := h.snapshotWithFallback(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return defaultBlockInterval, err
	}
	return snap.BlockInterval, nil
}

func (h *Hotstuff) NextProposalBlock(chain consensus.ChainHeaderReader, header *types.Header, proposer common.Address) (uint64, uint64, error) {
	snap, err := h.snapshot(chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		return 0, 0, err
	}

	return snap.nextProposalBlock(proposer)
}

func (h *Hotstuff) checkNanoBlackList(state vm.StateDB, header *types.Header) error {
	if h.chainConfig.IsNano(header.Number) {
		for _, blackListAddr := range types.NanoBlackList {
			if state.IsAddressInMutations(blackListAddr) {
				log.Error("blacklisted address found", "address", blackListAddr)
				return fmt.Errorf("block contains blacklisted address: %s", blackListAddr.Hex())
			}
		}
	}
	return nil
}

// chain context
type chainContext struct {
	Chain  consensus.ChainHeaderReader
	parlia consensus.Engine
}

func (c chainContext) Engine() consensus.Engine {
	return c.parlia
}

func (c chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	return c.Chain.GetHeader(hash, number)
}

func (c chainContext) Config() *params.ChainConfig {
	return c.Chain.Config()
}

// apply message
func applyMessage(
	msg *core.Message,
	evm *vm.EVM,
	state vm.StateDB,
	header *types.Header,
	chainConfig *params.ChainConfig,
	chainContext core.ChainContext,
) (uint64, error) {
	log.Info("[applyMessage] applyMessage", "msg", msg.From, "to", msg.To, "data", hex.EncodeToString(msg.Data), "value",
		msg.Value.String(), "gasLimit", msg.GasLimit, "gasPrice", msg.GasPrice.String(), "blockNumber", header.Number, "blockTime", header.Time)
	// Apply the transaction to the current state (included in the env)
	if chainConfig.IsCancun(header.Number, header.Time) {
		rules := evm.ChainConfig().Rules(evm.Context.BlockNumber, evm.Context.Random != nil, evm.Context.Time)
		state.Prepare(rules, msg.From, evm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)
	} else {
		state.ClearAccessList()
	}
	log.Info("[applyMessage] from's nonce", "nonce", state.GetNonce(msg.From))
	// Increment the nonce for the next transaction
	state.SetNonce(msg.From, state.GetNonce(msg.From)+1, tracing.NonceChangeEoACall)

	ret, returnGas, err := evm.Call(
		msg.From,
		*msg.To,
		msg.Data,
		msg.GasLimit,
		uint256.MustFromBig(msg.Value),
	)
	if err != nil {
		log.Error("apply message failed", "msg", string(ret), "err", err)
	}
	return msg.GasLimit - returnGas, err
}

// proposalKey build a key which is a combination of the block number and the proposer address.
func proposalKey(header types.Header) string {
	return header.ParentHash.String() + header.Coinbase.String()
}

// delayForRamanujanFork computes sealing delay for current validator with simple backoff
func (h *Hotstuff) delayForRamanujanFork(snap *Snapshot, header *types.Header) time.Duration {
	delay := time.Until(time.UnixMilli(int64(header.MilliTimestamp())))
	if h.chainConfig.IsRamanujan(header.Number) {
		return delay
	}
	if header.Difficulty.Cmp(diffNoTurn) == 0 {
		// It's not our explicit turn; add a small randomized backoff
		wiggle := time.Duration(len(snap.Validators)/2+1) * time.Millisecond * time.Duration(wiggleTime)
		delay += time.Duration(defaultInitialBackOffTime)*time.Millisecond + time.Duration(rand.Int63n(int64(wiggle)))
	}
	return delay
}

// blockTimeVerifyForRamanujanFork validates header time against expected under Ramanujan
func (h *Hotstuff) blockTimeVerifyForRamanujanFork(snap *Snapshot, header, parent *types.Header) error {
	if h.chainConfig.IsRamanujan(header.Number) {
		expected := parent.MilliTimestamp() + snap.BlockInterval
		if header.MilliTimestamp() < expected {
			return consensus.ErrFutureBlock
		}
	}
	return nil
}

// getTurnLength obtains turn length; falls back to default prior to Bohr
func (h *Hotstuff) getTurnLength(chain consensus.ChainHeaderReader, header *types.Header) (*uint8, error) {
	log.Info("[getTurnLength] ENTER", "blockNumber", header.Number.Uint64(), "parentHash", header.ParentHash.Hex()[:10])
	// CRITICAL FIX: Get parent with fallback for Chained HotStuff
	log.Info("[getTurnLength] Getting parent from canonical chain")
	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		log.Warn("[getTurnLength] parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())

		log.Info("[getTurnLength] Calling GetBlockFromState")
		parentBlock := h.GetBlockFromState(header.ParentHash)
		log.Info("[getTurnLength] GetBlockFromState returned", "found", parentBlock != nil)
		if parentBlock != nil {
			parent = parentBlock.Header()
			log.Info("[getTurnLength] Got parent from HotStuff state",
				"parentHash", header.ParentHash.Hex()[:10],
				"parentNumber", parent.Number.Uint64())
		}
	} else {
		log.Info("[getTurnLength] Got parent from canonical chain",
			"parentHash", header.ParentHash.Hex()[:10],
			"parentNumber", parent.Number.Uint64())
	}

	if parent == nil {
		log.Error("[getTurnLength] parent not found - CRITICAL",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return nil, errors.New("parent not found")
	}
	log.Info("[getTurnLength] Checking Bohr fork", "isBohr", h.chainConfig.IsBohr(parent.Number, parent.Time))
	var turnLength uint8
	if h.chainConfig.IsBohr(parent.Number, parent.Time) {
		log.Info("[getTurnLength] Calling getTurnLengthFromContract")
		turnFromContract, err := h.getTurnLengthFromContract(parent)
		log.Info("[getTurnLength] getTurnLengthFromContract returned", "err", err, "result", turnFromContract)
		if err != nil || turnFromContract == nil {
			log.Error("getTurnLengthFromContract: unexpected error", "error", err)
			return nil, errors.New("unexpected error when getTurnLengthFromContract")
		}
		turnLength = uint8(turnFromContract.Int64())
	} else {
		log.Info("[getTurnLength] Using default turn length")
		turnLength = defaultTurnLength
	}
	log.Info("[getTurnLength] EXIT", "turnLength", turnLength)
	return &turnLength, nil
}

// callContractAtState is a generic function to call a contract method at a specific state root.
// This function works in both Prepare and Verify phases:
// - Prepare: parent (highQC block) may not be in canonical chain yet, but state is committed by executeBlocks
// - Verify: parent is in canonical chain, and we ensure we use parent's state for consensus consistency
//
// Why not use ethAPI.Call with block number?
// 1. ethAPI.Call(blockNumber) only queries canonical chain, would fail for uncommitted highQC blocks
// 2. Using "latest" would cause consensus inconsistency during sync (block 200 would see block 2000's state)
//
// Parameters:
// - header: the block header whose state root to use (typically parent header)
// - contractAddr: the address of the contract to call
// - callData: the encoded method call data
// Returns:
// - result bytes from contract execution
// - error if state unavailable or execution failed
func (h *Hotstuff) callContractAtState(header *types.Header, contractAddr common.Address, callData []byte) ([]byte, error) {
	// Get statedb from header's state root using type assertion
	statedb := h.GetProposalState(header.Hash())
	var err error

	if statedb != nil {
		log.Debug("[callContractAtState] using isolated proposal state", "blockNumber", header.Number.Uint64(), "hash", header.Hash())
	} else if bc, ok := h.chain.(interface {
		StateAt(root common.Hash) (*state.StateDB, error)
	}); ok {
		statedb, err = bc.StateAt(header.Root)
		if err != nil {
			log.Error("[callContractAtState] failed to get state",
				"error", err,
				"blockNumber", header.Number.Uint64(),
				"stateRoot", header.Root.Hex()[:10],
				"contract", contractAddr.Hex())
			return nil, fmt.Errorf("failed to get state at root %s: %w", header.Root.Hex()[:10], err)
		}
	} else {
		return nil, errors.New("chain does not support StateAt method")
	}

	// Create a simple chain context for EVM
	chainContext := &chainContextAdapter{engine: h, chainHeaderReader: h.chain}

	// Execute contract call on the state
	// IMPORTANT: This is a consensus-level system contract query, not a user transaction
	// NoBaseFee=true bypasses base fee checks, so gas prices don't matter
	msg := &core.Message{
		To:        &contractAddr,
		From:      common.Address{}, // Zero address for read-only calls
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  math.MaxUint64 / 2,
		GasPrice:  big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		GasTipCap: big.NewInt(0),
		Data:      callData,
	}

	blockContext := core.NewEVMBlockContext(header, chainContext, nil)
	// NoBaseFee: true - bypass base fee checks for consensus-level calls
	evm := vm.NewEVM(blockContext, statedb, h.chainConfig, vm.Config{NoBaseFee: true})

	result, err := core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(msg.GasLimit))
	if err != nil {
		log.Error("[callContractAtState] failed to execute contract call",
			"error", err,
			"blockNumber", header.Number.Uint64(),
			"contract", contractAddr.Hex())
		return nil, fmt.Errorf("contract call failed: %w", err)
	}

	if result.Failed() {
		log.Error("[callContractAtState] contract call reverted",
			"blockNumber", header.Number.Uint64(),
			"contract", contractAddr.Hex(),
			"revertReason", string(result.Revert()))
		return nil, errors.New("contract call reverted")
	}

	log.Debug("[callContractAtState] successfully called contract",
		"blockNumber", header.Number.Uint64(),
		"stateRoot", header.Root.Hex()[:10],
		"contract", contractAddr.Hex(),
		"resultLen", len(result.Return()))

	return result.Return(), nil
}

func (h *Hotstuff) getTurnLengthFromContract(header *types.Header) (turnLength *big.Int, err error) {
	if params.FixedTurnLength >= 1 && params.FixedTurnLength <= 9 {
		if params.FixedTurnLength == 2 {
			return h.getRandTurnLength(header)
		}
		return big.NewInt(int64(params.FixedTurnLength)), nil
	}

	method := "getTurnLength"
	contractAddr := common.HexToAddress(systemcontracts.ValidatorContract)

	// Pack method call data
	callData, err := h.validatorSetABI.Pack(method)
	if err != nil {
		return nil, fmt.Errorf("failed to pack method %s: %w", method, err)
	}

	// Call contract at parent header's state
	resultData, err := h.callContractAtState(header, contractAddr, callData)
	if err != nil {
		log.Error("[getTurnLengthFromContract] callContractAtState failed",
			"error", err,
			"parentBlock", header.Number.Uint64(),
			"contract", systemcontracts.ValidatorContract)
		return nil, err
	}

	// Unpack result
	if err := h.validatorSetABI.UnpackIntoInterface(&turnLength, method, resultData); err != nil {
		return nil, fmt.Errorf("failed to unpack result: %w", err)
	}

	log.Debug("[getTurnLengthFromContract] successfully queried contract",
		"parentBlock", header.Number.Uint64(),
		"parentRoot", header.Root.Hex()[:10],
		"turnLength", turnLength)

	return turnLength, nil
}

func (h *Hotstuff) getRandTurnLength(header *types.Header) (turnLength *big.Int, err error) {
	turns := [8]uint8{1, 3, 4, 5, 6, 7, 8, 9}
	r := rand.New(rand.NewSource(int64(header.Time)))
	idx := int(r.Int31n(int32(len(turns))))
	return big.NewInt(int64(turns[idx])), nil
}

// sameDayInUTC uses params.BreatheBlockInterval to compare days
func sameDayInUTC(first, second uint64) bool {
	return first/params.BreatheBlockInterval == second/params.BreatheBlockInterval
}
func isBreatheBlock(lastBlockTime, blockTime uint64) bool {
	return lastBlockTime != 0 && !sameDayInUTC(lastBlockTime, blockTime)
}

// initializeFeynmanContract triggers initialize() on stake-related contracts
func (h *Hotstuff) initializeFeynmanContract(state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks,
) error {
	method := "initialize"
	contracts := []string{systemcontracts.StakeHubContract, systemcontracts.GovernorContract, systemcontracts.GovTokenContract, systemcontracts.TimelockContract, systemcontracts.TokenRecoverPortalContract}
	data, err := h.stakeHubABI.Pack(method)
	if err != nil {
		return err
	}
	for _, c := range contracts {
		msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(c), data, common.Big0)
		if err := h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer); err != nil {
			return err
		}
		log.Warn("[initializeFeynmanContract] After applyTransaction",
			"block", header.Number,
			"contract", c,
			"stateRoot", state.IntermediateRoot(h.chainConfig.IsEIP158(header.Number)).Hex()[:10])
	}
	return nil
}

// election info from StakeHub
// getValidatorElectionInfo queries validator election info from StakeHub contract at given header's state
// This function uses callContractAtState to support queries on uncommitted parent blocks (HotStuff pipelining)
func (h *Hotstuff) getValidatorElectionInfo(header *types.Header) ([]common.Address, []*big.Int, [][]byte, *big.Int, error) {
	method := "getValidatorElectionInfo"
	contractAddr := common.HexToAddress(systemcontracts.StakeHubContract)

	data, err := h.stakeHubABI.Pack(method, big.NewInt(0), big.NewInt(0))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to pack method %s: %w", method, err)
	}

	// Use callContractAtState to query contract at header's state
	// This works even if the block is not committed yet (Prepare phase)
	result, err := h.callContractAtState(header, contractAddr, data)
	if err != nil {
		log.Error("[getValidatorElectionInfo] callContractAtState failed",
			"error", err,
			"blockNumber", header.Number.Uint64(),
			"contract", systemcontracts.StakeHubContract)
		return nil, nil, nil, nil, err
	}

	var addrs []common.Address
	var powers []*big.Int
	var votes [][]byte
	var total *big.Int
	if err := h.stakeHubABI.UnpackIntoInterface(&[]interface{}{&addrs, &powers, &votes, &total}, method, result); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to unpack result: %w", err)
	}

	log.Debug("[getValidatorElectionInfo] successfully queried contract",
		"blockNumber", header.Number.Uint64(),
		"validatorCount", len(addrs))

	return addrs, powers, votes, total, nil
}

// getMaxElectedValidators queries max elected validators from StakeHub contract at given header's state
// This function uses callContractAtState to support queries on uncommitted parent blocks (HotStuff pipelining)
func (h *Hotstuff) getMaxElectedValidators(header *types.Header) (maxElectedValidators *big.Int, err error) {
	method := "maxElectedValidators"
	contractAddr := common.HexToAddress(systemcontracts.StakeHubContract)

	data, err := h.stakeHubABI.Pack(method)
	if err != nil {
		return nil, fmt.Errorf("failed to pack method %s: %w", method, err)
	}

	// Use callContractAtState to query contract at header's state
	// This works even if the block is not committed yet (Prepare phase)
	result, err := h.callContractAtState(header, contractAddr, data)
	if err != nil {
		log.Error("[getMaxElectedValidators] callContractAtState failed",
			"error", err,
			"blockNumber", header.Number.Uint64(),
			"contract", systemcontracts.StakeHubContract)
		return nil, err
	}

	if err := h.stakeHubABI.UnpackIntoInterface(&maxElectedValidators, method, result); err != nil {
		return nil, fmt.Errorf("failed to unpack result: %w", err)
	}

	log.Debug("[getMaxElectedValidators] successfully queried contract",
		"blockNumber", header.Number.Uint64(),
		"maxElectedValidators", maxElectedValidators)

	return maxElectedValidators, nil
}

func getTopValidatorsByVotingPower(addrs []common.Address, powers []*big.Int, votes [][]byte, max *big.Int) ([]common.Address, []uint64, [][]byte) {
	type item struct {
		a common.Address
		h *big.Int
		v []byte
	}
	arr := make([]item, 0, len(addrs))
	for i := range addrs {
		if powers[i].Cmp(big.NewInt(0)) == 1 {
			arr = append(arr, item{a: addrs[i], h: powers[i], v: votes[i]})
		}
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].h.Cmp(arr[j].h) == 0 {
			return arr[i].a.Hex() < arr[j].a.Hex()
		}
		return arr[i].h.Cmp(arr[j].h) == 1
	})
	n := int(max.Int64())
	if n > len(arr) {
		n = len(arr)
	}
	resA := make([]common.Address, n)
	resP := make([]uint64, n)
	resV := make([][]byte, n)
	for i := 0; i < n; i++ {
		resA[i] = arr[i].a
		resP[i] = new(big.Int).Div(arr[i].h, big.NewInt(1e10)).Uint64()
		resV[i] = arr[i].v
	}
	return resA, resP, resV
}

func (h *Hotstuff) updateValidatorSetV2(state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks,
) error {
	// CRITICAL: Must use parent header to query contract state
	// In HotStuff, parent may not be committed yet, so we cannot use API queries with block number/hash
	// Instead, we get parent header and use callContractAtState to query its state
	// CRITICAL: Try canonical chain first, then HotStuff state (for pipelined proposals)
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		log.Debug("[updateValidatorSetV2] parent not in canonical chain, trying HotStuff state",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())

		parentBlock := h.GetBlockFromState(header.ParentHash)
		if parentBlock != nil {
			parent = parentBlock.Header()
		}
	}

	if parent == nil {
		log.Error("[updateValidatorSetV2] parent header not found",
			"parentHash", header.ParentHash.Hex()[:10],
			"blockNumber", header.Number.Uint64())
		return errors.New("parent header not found")
	}

	addrs, powers, votes, total, err := h.getValidatorElectionInfo(parent)
	if err != nil {
		return err
	}
	max, err := h.getMaxElectedValidators(parent)
	if err != nil {
		return err
	}
	if total == nil || int(total.Int64()) != len(addrs) || len(powers) != len(addrs) || len(votes) != len(addrs) {
		return errors.New("validator length not match")
	}
	eAddrs, ePowers, eVotes := getTopValidatorsByVotingPower(addrs, powers, votes, max)
	data, err := h.validatorSetABI.Pack("updateValidatorSetV2", eAddrs, ePowers, eVotes)
	if err != nil {
		return err
	}
	msg := h.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	return h.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// API is a minimal holder for RPC registration
type API struct {
	chain  consensus.ChainHeaderReader
	parlia *Hotstuff
}

func (h *Hotstuff) IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error) {
	if tx.To() == nil || !isToSystemContract(*tx.To()) {
		return false, nil
	}
	if tx.GasPrice().Sign() != 0 {
		return false, nil
	}
	sender, err := types.Sender(h.signer, tx)
	if err != nil {
		return false, errors.New("UnAuthorized transaction")
	}
	return sender == header.Coinbase, nil
}

func (h *Hotstuff) IsSystemContract(to *common.Address) bool {
	if to == nil {
		return false
	}
	return isToSystemContract(*to)
}

// GetValidatorPeers returns a map of validator addresses to their (empty) peer list.
// With hs subprotocol, peer discovery is handled by the p2p layer; we only expose addresses here.
// getNodeReplicaID returns the HotStuff replica ID for this node
func (h *Hotstuff) getNodeReplicaID() ID {
	// Get this node's validator address
	addr := h.ConsensusAddress()
	if (addr == common.Address{}) {
		log.Warn("No validator address set for replica ID, using default")
		return ID(1)
	}

	// Deterministic mapping
	hash := sha256.Sum256(addr.Bytes())
	replicaID := ID(binary.BigEndian.Uint32(hash[:4]))

	log.Info("Assigned replica ID", "address", addr.Hex(), "replicaID", replicaID)
	return replicaID
}

func (h *Hotstuff) GetValidatorPeers() (map[common.Address][]*enode.Node, error) {
	nodeIDsMap, err := h.GetNodeIDsMap()
	if err != nil {
		return nil, err
	}
	validatorPeers := make(map[common.Address][]*enode.Node, len(nodeIDsMap))
	for addr := range nodeIDsMap {
		validatorPeers[addr] = []*enode.Node{}
	}
	return validatorPeers, nil
}

// Author implements consensus.Engine, returning the SystemAddress
func (h *Hotstuff) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, h.signatures, h.chainConfig.ChainID)
}

// Utility functions moved to hotstuff_utils.go

// hsTimeoutCert represents a timeout certificate
type hsTimeoutCert struct {
	View      uint64
	HighQC    *HsQC
	Signers   []common.Address
	SignerSet types.ValidatorsBitSet
	AggSig    types.BLSSignature
}

// HotStuff core state (non-final, for hs subprotocol integration)
type HsQC struct {
	BlockHash  common.Hash
	View       uint64
	Sig        []byte
	SignersSet types.ValidatorsBitSet
}

func (qc *HsQC) hasAggregateProof() bool {
	return qc != nil && qc.SignersSet != 0 && len(qc.Sig) == types.BLSSignatureLength
}

type hsState struct {
	currentView     uint64
	highQC          *HsQC
	lockedQC        *HsQC
	hasLastVote     bool
	lastVotedView   uint64
	lastVotedHash   common.Hash
	votes           map[uint64]map[common.Hash]map[common.Address]*hs.VotePacket
	newViews        map[uint64]map[common.Address]*hs.NewViewPacket
	proposalsByView map[uint64]*types.Header
	proposalsByHash map[common.Hash]*types.Header
	// 存储完整的区块数据，用于提交时使用
	proposalsByHashBlock    map[common.Hash]*types.Block
	proposalsByHashReceipts map[common.Hash]types.Receipts
	proposalStates          map[common.Hash]*state.StateDB
	qcsByView               map[uint64]*HsQC
	// timeout aggregation per view
	timeouts map[uint64]map[common.Address]*hs.TimeoutPacket
	// highest known TC view (if any)
	highTCView uint64
	// committed blocks: hash -> block number
	committedBlocks map[common.Hash]uint64
	// prewritten blocks (rawdb) awaiting commit
	prewritten map[common.Hash]struct{}

	// Track blocks currently being processed (Phase 3 executeBlocks)
	processingBlocks map[common.Hash]struct{}
	// Pending proposals waiting for their parent to complete processing
	// Key: parent hash, Value: list of pending proposal packets
	pendingProposals map[common.Hash][]*pendingProposal
	// Index pending proposals by their own block hash (for QC-triggered processing)
	// Key: proposal block hash, Value: pointer to the pending proposal
	pendingProposalsByHash map[common.Hash]*pendingProposal
}

// pendingProposal represents a proposal waiting for its parent
type pendingProposal struct {
	peerID string
	packet *hs.ProposalPacket
	header *types.Header
	body   *types.Body
}

// Initialize HotStuff state (invoke during New)
func (h *Hotstuff) initHsState() {
	if h == nil {
		return
	}
	// NOTE: not concurrency-safe init; call in constructor before other goroutines
	if hss := h.getHsState(); hss == nil {
		h._hs = &hsState{
			votes:                   make(map[uint64]map[common.Hash]map[common.Address]*hs.VotePacket),
			newViews:                make(map[uint64]map[common.Address]*hs.NewViewPacket),
			proposalsByView:         make(map[uint64]*types.Header),
			proposalsByHash:         make(map[common.Hash]*types.Header),
			proposalsByHashBlock:    make(map[common.Hash]*types.Block),
			proposalsByHashReceipts: make(map[common.Hash]types.Receipts),
			proposalStates:          make(map[common.Hash]*state.StateDB),
			qcsByView:               make(map[uint64]*HsQC),
			timeouts:                make(map[uint64]map[common.Address]*hs.TimeoutPacket),
			committedBlocks:         make(map[common.Hash]uint64),
			prewritten:              make(map[common.Hash]struct{}),
			processingBlocks:        make(map[common.Hash]struct{}),
			pendingProposals:        make(map[common.Hash][]*pendingProposal),
			pendingProposalsByHash:  make(map[common.Hash]*pendingProposal),
		}
	}
}

// getHsState returns the pointer to the HotStuff state; helper to avoid nil deref
// getHsState returns the HotStuff state.
// CRITICAL: This function does NOT acquire locks.
// Callers MUST hold either RLock or Lock before calling this function.
func (h *Hotstuff) getHsState() *hsState {
	return h._hs
}

// GetCurrentView returns the current view number (thread-safe for external callers)
func (h *Hotstuff) GetCurrentView() uint64 {
	h.ensureBootstrapAtHead()
	h.lock.RLock()
	defer h.lock.RUnlock()
	if h._hs == nil {
		return 0
	}
	return h._hs.currentView
}

// ensureBootstrapAtHead initializes the proofless QC allowed exactly at the
// Parlia->HotStuff boundary (or genesis activation). It is idempotent and is
// also called by runtime entry points so a node need not restart at the fork.
func (h *Hotstuff) ensureBootstrapAtHead() {
	if h == nil || h.chain == nil || h.chainConfig == nil || h.chainConfig.HotstuffBlock == nil {
		return
	}
	head := h.chain.CurrentHeader()
	if head == nil || head.Number == nil {
		return
	}
	next := new(big.Int).Add(head.Number, common.Big1)
	bootstrap := h.chainConfig.IsOnHotstuff(next) ||
		(h.chainConfig.HotstuffBlock.Sign() == 0 && head.Number.Sign() == 0)
	if !bootstrap {
		return
	}
	headView := getViewFromHeader(head, h.chainConfig)
	if headView == math.MaxUint64 {
		return
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	st := h.getHsState()
	if st == nil {
		return
	}
	if st.highQC == nil {
		st.highQC = &HsQC{BlockHash: head.Hash(), View: headView}
	}
	if headView+1 > st.currentView {
		st.currentView = headView + 1
	}
}

// HasProposalForView checks if we already have a proposal for the given view (thread-safe for external callers)
func (h *Hotstuff) HasProposalForView(view uint64) bool {
	h.lock.RLock()
	defer h.lock.RUnlock()
	if h._hs == nil {
		return false
	}
	_, exists := h._hs.proposalsByView[view]
	return exists
}

// getSnapshotAtHashOrView tries to resolve a snapshot using baseHash if provided;
// otherwise it attempts to resolve by view (NOTE: when using TC, view may not equal block number,
// so this fallback may not find the exact block and will use current head instead).
func (h *Hotstuff) getSnapshotAtHashOrView(chain consensus.ChainHeaderReader, baseHash common.Hash, view uint64) (*Snapshot, error) {
	// CRITICAL: Try canonical chain first, then HotStuff state (for pipelined blocks)
	var targetHeader *types.Header

	if baseHash != (common.Hash{}) {
		// Try canonical chain first
		targetHeader = chain.GetHeaderByHash(baseHash)

		// Fallback: try HotStuff state (for pipelined blocks not yet committed)
		if targetHeader == nil {
			if block := h.getBlockWithoutStateLock(baseHash); block != nil {
				targetHeader = block.Header()
				log.Debug("getSnapshotAtHashOrView: got header from HotStuff state",
					"baseHash", baseHash.Hex()[:8],
					"number", block.NumberU64())
			}
		}

		if targetHeader == nil {
			return nil, consensus.ErrUnknownAncestor
		}
		return h.snapshot(chain, targetHeader.Number.Uint64(), targetHeader.Hash(), nil)
	}

	// Try to find block by number (view may not equal block number when TC is used)
	if view > 0 {
		// Try canonical chain by view as block number
		if hdr := chain.GetHeaderByNumber(view); hdr != nil {
			if snap, err := h.snapshot(chain, hdr.Number.Uint64(), hdr.Hash(), nil); err == nil {
				return snap, nil
			}
		}

	}

	// Fallback to current head
	head := chain.CurrentHeader()
	if head == nil {
		return nil, errors.New("no head")
	}
	return h.snapshot(chain, head.Number.Uint64(), head.Hash(), nil)
}

// getLeaderForView picks the leader from a snapshot resolved for the given view.
func (h *Hotstuff) getLeaderForView(chain consensus.ChainHeaderReader, view uint64) (common.Address, error) {
	snap, err := h.getSnapshotAtHashOrView(chain, common.Hash{}, view)
	if err != nil {
		return common.Address{}, err
	}
	vals := snap.validators()
	if len(vals) == 0 {
		return common.Address{}, errors.New("no validators")
	}
	idx := int(view % uint64(len(vals)))
	return vals[idx], nil
}

// getLeaderForViewAt is a contextual variant using a base hash (e.g. parent or HighQC)
func (h *Hotstuff) getLeaderForViewAt(chain consensus.ChainHeaderReader, baseHash common.Hash, view uint64) (common.Address, error) {
	snap, err := h.getSnapshotAtHashOrView(chain, baseHash, view)
	if err != nil {
		return common.Address{}, err
	}
	vals := snap.validators()
	if len(vals) == 0 {
		return common.Address{}, errors.New("no validators")
	}
	idx := int(view % uint64(len(vals)))
	leader := vals[idx]

	// Debug logging for leader election
	log.Debug("Leader election",
		"view", view,
		"numValidators", len(vals),
		"leaderIdx", idx,
		"leader", leader.Hex(),
		"allValidators", func() []string {
			addrs := make([]string, len(vals))
			for i, v := range vals {
				addrs[i] = v.Hex()
			}
			return addrs
		}())

	return leader, nil
}

// chainContextAdapter adapts ChainHeaderReader to ChainContext interface
type chainContextAdapter struct {
	chainHeaderReader consensus.ChainHeaderReader
	engine            consensus.Engine
}

func (cca *chainContextAdapter) Engine() consensus.Engine {
	return cca.engine
}

func (cca *chainContextAdapter) GetHeader(hash common.Hash, number uint64) *types.Header {
	return cca.chainHeaderReader.GetHeader(hash, number)
}

func (cca *chainContextAdapter) Config() *params.ChainConfig {
	return cca.chainHeaderReader.Config()
}

// getVoteAttestationFromHeader extracts vote attestation from header extra data
func getVoteAttestationFromHeader(header *types.Header, chainConfig *params.ChainConfig, epochLength uint64) (*types.VoteAttestation, error) {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil, nil
	}
	if !chainConfig.IsLuban(header.Number) {
		return nil, nil
	}

	hsExtraSize := hotstuffExtraSize(header, chainConfig)
	end := len(header.Extra) - extraSeal - hsExtraSize
	if end <= extraVanity {
		return nil, nil
	}

	var start int
	if header.Number.Uint64()%epochLength != 0 {
		start = extraVanity
	} else {
		num := int(header.Extra[extraVanity])
		start = extraVanity + validatorNumberSize + num*validatorBytesLength
		if chainConfig.IsBohr(header.Number, header.Time) {
			start += turnLengthSize
		}
	}
	if end <= start {
		return nil, nil
	}

	var attestation types.VoteAttestation
	if err := rlp.Decode(bytes.NewReader(header.Extra[start:end]), &attestation); err != nil {
		return nil, fmt.Errorf("block %d has vote attestation info, decode err: %s", header.Number.Uint64(), err)
	}
	return &attestation, nil
}

func (h *Hotstuff) verifyVoteAttestation(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	epochLength, err := h.epochLength(chain, header, parents)
	if err != nil {
		return err
	}
	attestation, err := getVoteAttestationFromHeader(header, chain.Config(), epochLength)
	if err != nil {
		return err
	}
	if attestation == nil {
		return nil
	}
	if attestation.Data == nil {
		return errors.New("invalid attestation, vote data is nil")
	}
	if len(attestation.Extra) > types.MaxAttestationExtraLength {
		return fmt.Errorf("invalid attestation, too large extra length: %d", len(attestation.Extra))
	}

	parent, err := h.getParent(chain, header, parents)
	if err != nil {
		return err
	}
	targetNumber := attestation.Data.TargetNumber
	targetHash := attestation.Data.TargetHash
	if targetNumber != parent.Number.Uint64() || targetHash != parent.Hash() {
		return fmt.Errorf("invalid attestation, target mismatch, expected block: %d, hash: %s; real block: %d, hash: %s",
			parent.Number.Uint64(), parent.Hash(), targetNumber, targetHash)
	}

	sourceNumber := attestation.Data.SourceNumber
	sourceHash := attestation.Data.SourceHash
	headers := []*types.Header{parent}
	if len(parents) > 0 {
		headers = parents
	}
	justifiedBlockNumber, justifiedBlockHash, err := h.GetJustifiedNumberAndHash(chain, headers)
	if err != nil {
		return errors.New("unexpected error when getting the highest justified number and hash")
	}
	if sourceNumber != justifiedBlockNumber || sourceHash != justifiedBlockHash {
		return fmt.Errorf("invalid attestation, source mismatch, expected block: %d, hash: %s; real block: %d, hash: %s",
			justifiedBlockNumber, justifiedBlockHash, sourceNumber, sourceHash)
	}

	if len(parents) > 1 {
		parents = parents[:len(parents)-1]
	} else {
		parents = nil
	}
	snap, err := h.snapshot(chain, parent.Number.Uint64()-1, parent.ParentHash, parents)
	if err != nil {
		return err
	}

	validators := snap.validators()
	validatorsBitSet := bitset.From([]uint64{uint64(attestation.VoteAddressSet)})
	if validatorsBitSet.Count() > uint(len(validators)) {
		return errors.New("invalid attestation, vote number larger than validators number")
	}
	votedAddrs := make([]bls.PublicKey, 0, validatorsBitSet.Count())
	for index, val := range validators {
		if !validatorsBitSet.Test(uint(index)) {
			continue
		}
		voteAddr, err := bls.PublicKeyFromBytes(snap.Validators[val].VoteAddress[:])
		if err != nil {
			return fmt.Errorf("BLS public key converts failed: %v", err)
		}
		votedAddrs = append(votedAddrs, voteAddr)
	}
	if len(votedAddrs) < cmath.CeilDiv(len(snap.Validators)*2, 3) {
		return errors.New("invalid attestation, not enough validators voted")
	}

	aggSig, err := bls.SignatureFromBytes(attestation.AggSignature[:])
	if err != nil {
		return fmt.Errorf("BLS signature converts failed: %v", err)
	}
	if !aggSig.FastAggregateVerify(votedAddrs, attestation.Data.Hash()) {
		return errors.New("invalid attestation, signature verify failed")
	}
	return nil
}

// moveToView updates local view and performs leader/replica actions
func (h *Hotstuff) moveToView(view uint64) {
	log.Info("moveToView: ENTER", "view", view)
	// CRITICAL FIX:
	// - moveToView updates shared hsState, so it MUST hold the write lock.
	// - Do NOT hold h.lock while doing potentially blocking work (snapshot/contract calls/network IO),
	//   otherwise TC paths can stall liveness.
	var (
		highQCView       uint64
		highQCHash       common.Hash
		highQCSignersSet types.ValidatorsBitSet
		highQCAggSig     []byte
		highTCView       uint64
		timeoutMapCopy   map[common.Address]*hs.TimeoutPacket
	)

	log.Info("moveToView: acquiring lock", "view", view)
	h.lock.Lock()
	log.Info("moveToView: lock acquired", "view", view)
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return
	}
	if view <= st.currentView {
		log.Debug("moveToView: view <= currentView", "view", view, "currentView", st.currentView)
		h.lock.Unlock()
		return
	}
	st.currentView = view
	if err := h.checkpointHsStateLocked(); err != nil {
		log.Error("Failed to persist HotStuff view change", "view", view, "err", err)
	}
	if st.highQC != nil {
		highQCView = st.highQC.View
		highQCHash = st.highQC.BlockHash
		highQCSignersSet = st.highQC.SignersSet
		highQCAggSig = common.CopyBytes(st.highQC.Sig)
	}
	highTCView = st.highTCView
	if highTCView > 0 {
		if tcMap, ok := st.timeouts[highTCView]; ok && len(tcMap) > 0 {
			// Shallow copy so we can aggregate signatures outside the lock safely
			timeoutMapCopy = make(map[common.Address]*hs.TimeoutPacket, len(tcMap))
			for k, v := range tcMap {
				timeoutMapCopy[k] = v
			}
		}
	}
	h.lock.Unlock()

	// Restart timeout for new view without holding h.lock.
	// Use fixed base timeout to avoid calling snapshot() which may cause lock contention
	timeoutMs := h.hsBaseTimeoutMS
	if timeoutMs == 0 {
		timeoutMs = defaultBlockInterval + 5000 // 5 second buffer
	}
	log.Info("moveToView: about to call restartViewTimeoutForView", "view", view, "timeoutMs", timeoutMs)
	h.restartViewTimeoutForView(view, timeoutMs)
	log.Info("moveToView: restartViewTimeoutForView returned", "view", view)

	// As replica, send NewView to leader with our HighQC and HighTC.
	log.Info("moveToView: preparing NewView packet", "view", view, "highQCView", highQCView, "highTCView", highTCView)
	nv := &hs.NewViewPacket{
		HighQCView:       highQCView,
		HighQCHash:       highQCHash,
		HighQCSignersSet: highQCSignersSet,
		HighQCAggSig:     highQCAggSig,
		HighTCView:       highTCView,
	}
	requiresTC := highTCView > 0 && highTCView >= highQCView && view == highTCView+1
	if requiresTC && timeoutMapCopy == nil {
		log.Error("moveToView: missing timeout messages for required TC", "view", view, "highTCView", highTCView)
		return
	}
	if highTCView > 0 && timeoutMapCopy != nil {
		// Carry TC signer set + aggregate signature.
		if tc, err := h.createTimeoutCert(highTCView, timeoutMapCopy); err == nil && tc != nil {
			nv.TimeoutSignersSet = tc.SignerSet
			nv.TimeoutAggSig = tc.AggSig
		} else {
			log.Warn("moveToView: failed to attach timeout certificate",
				"view", view,
				"highTCView", highTCView,
				"err", err)
			if requiresTC {
				return
			}
			nv.HighTCView = 0
		}
	}
	log.Info("moveToView: broadcasting NewView", "view", view)
	h.broadcastHsNewView(nv)
	log.Info("moveToView: NewView broadcasted", "view", view)

	// If we're leader for this view, try to propose (this is a soft trigger; miner does the actual work).
	log.Info("moveToView: checking if we are leader", "view", view)
	leader, err := h.getLeaderForViewAt(h.chain, highQCHash, view)
	if err != nil {
		log.Error("moveToView: failed to resolve contextual leader", "view", view, "highQC", highQCHash, "err", err)
		return
	}
	self := h.ConsensusAddress()
	log.Info("moveToView: leader determined", "view", view, "leader", leader, "self", self, "isLeader", leader == self)
	if leader == self {
		base := highQCHash
		log.Info("moveToView: we are leader, proposing from HighQC", "view", view, "base", base.Hex()[:10])
		h.proposeFromHighQC(view, base)
	}
	log.Info("moveToView: EXIT", "view", view)
}

// restartViewTimeoutForView restarts the per-view timeout timer for the provided view.
// It receives timeout as a parameter to avoid calling snapshot() internally which may cause lock contention.
// MUST NOT hold h.lock when calling this function.
func (h *Hotstuff) restartViewTimeoutForView(view uint64, timeoutMs uint64) {
	log.Info("restartViewTimeoutForView: ENTER", "view", view, "timeoutMs", timeoutMs)

	// Only protect hsTimer mutation with the lock (no snapshot/IO here).
	log.Info("restartViewTimeoutForView: acquiring lock", "view", view)
	h.lock.Lock()
	log.Info("restartViewTimeoutForView: lock acquired", "view", view)
	if h.closed {
		h.lock.Unlock()
		return
	}

	oldTimer := h.hsTimer
	if oldTimer != nil {
		stopped := oldTimer.Stop()
		log.Info("restartViewTimeoutForView: stopped old timer", "view", view, "stopped", stopped)
	} else {
		log.Info("restartViewTimeoutForView: no old timer to stop", "view", view)
	}

	h.hsTimer = time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		log.Info("Timer callback triggered", "forView", view)
		h.onLocalViewTimeout(view)
	})
	log.Info("restartViewTimeoutForView: new timer set", "view", view, "timeoutMs", timeoutMs)

	h.lock.Unlock()
	log.Info("restartViewTimeoutForView: EXIT", "view", view)
}

// getCurrentValidatorsBeforeLuban gets validators before Luban fork at the given header's state
// This function uses callContractAtState to support queries on uncommitted parent blocks (HotStuff pipelining)
func (h *Hotstuff) getCurrentValidatorsBeforeLuban(header *types.Header) ([]common.Address, error) {
	// prepare different method
	method := "getValidators"
	if h.chainConfig.IsEuler(header.Number) {
		method = "getMiningValidators"
	}

	contractAddr := common.HexToAddress(systemcontracts.ValidatorContract)
	data, err := h.validatorSetABIBeforeLuban.Pack(method)
	if err != nil {
		log.Error("[getCurrentValidatorsBeforeLuban] Unable to pack tx for getValidators", "error", err)
		return nil, err
	}

	// Use callContractAtState to query contract at header's state
	// This works even if the block is not committed yet (Prepare phase)
	result, err := h.callContractAtState(header, contractAddr, data)
	if err != nil {
		log.Error("[getCurrentValidatorsBeforeLuban] callContractAtState failed",
			"error", err,
			"blockNumber", header.Number.Uint64(),
			"contract", systemcontracts.ValidatorContract)
		return nil, err
	}

	var valSet []common.Address
	err = h.validatorSetABIBeforeLuban.UnpackIntoInterface(&valSet, method, result)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack result: %w", err)
	}

	log.Debug("[getCurrentValidatorsBeforeLuban] successfully queried validators",
		"blockNumber", header.Number.Uint64(),
		"validatorCount", len(valSet))

	return valSet, err
}

// GetBlockFromState retrieves a block from HotStuff state or rawdb
// This is used by the miner to get parent blocks that may not be in canonical chain yet
//
// CRITICAL FIX: First check lock-free cache to prevent RWMutex priority deadlock.
// Deadlock scenario without this fix:
//  1. OnHsProposal (view N) in Phase 3: no lock held, calling executeBlocks
//  2. OnHsQuorumCert (view N+1): waiting for h.lock.Lock()
//  3. executeBlocks -> Finalize -> GetBlockFromState: tries to acquire RLock
//  4. RLock blocked because Lock() is waiting (Go RWMutex write-priority)
//  5. Deadlock: Lock() waits for executeBlocks, executeBlocks waits for RLock
func (h *Hotstuff) GetBlockFromState(hash common.Hash) *types.Block {
	if block := h.getBlockWithoutStateLock(hash); block != nil {
		return block
	}

	// Step 3: Fall back to locked access (may block, but necessary for correctness)
	log.Debug("[GetBlockFromState] Trying locked access", "hash", hash.Hex()[:10])
	h.lock.RLock()
	defer h.lock.RUnlock()
	result := h.getBlockFromStateUnsafe(hash)
	log.Debug("[GetBlockFromState] EXIT", "hash", hash.Hex()[:10], "found", result != nil)
	return result
}

func (h *Hotstuff) getBlockWithoutStateLock(hash common.Hash) *types.Block {
	if cached, ok := h.proposalBlocksCache.Load(hash); ok {
		if block, ok := cached.(*types.Block); ok && block != nil {
			return block
		}
	}
	if h.db != nil {
		if number := rawdb.ReadHeaderNumber(h.db, hash); number != nil {
			return rawdb.ReadBlock(h.db, hash, *number)
		}
	}
	return nil
}

// getBlockFromStateUnsafe retrieves a block without acquiring locks.
// MUST be called with h.lock held (either RLock or Lock).
func (h *Hotstuff) getBlockFromStateUnsafe(hash common.Hash) *types.Block {
	st := h.getHsState()
	if st == nil {
		log.Debug("GetBlockFromState: state is nil", "hash", hash.Hex()[:8])
		return nil
	}

	log.Debug("GetBlockFromState: searching",
		"hash", hash.Hex()[:8],
		"proposalsByHashBlockCount", len(st.proposalsByHashBlock))

	// Debug: list all blocks in proposals
	// for h, b := range st.proposalsByHashBlock {
	// 	log.Debug("GetBlockFromState: available block",
	// 		"hash", h.Hex()[:8],
	// 		"number", b.NumberU64())
	// }

	// 1. Try to get from HotStuff proposals (in-memory)
	if block := st.proposalsByHashBlock[hash]; block != nil {
		log.Debug("GetBlockFromState: found in HotStuff proposals",
			"hash", hash.Hex()[:8],
			"number", block.NumberU64())
		return block
	}

	// 2. Try to get from rawdb (prewritten blocks)
	if block := h.getBlockWithoutStateLock(hash); block != nil {
		return block
	}

	log.Debug("GetBlockFromState: block not found",
		"hash", hash.Hex()[:8])
	return nil
}
