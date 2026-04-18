package hotstuff

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

// hsVoteDigest computes the digest for a HotStuff vote
func (h *Hotstuff) hsVoteDigest(blockHash common.Hash, view uint64) [32]byte {
	type voteTuple struct {
		BlockHash  common.Hash
		ViewNumber uint64
	}
	enc, err := rlp.EncodeToBytes(&voteTuple{BlockHash: blockHash, ViewNumber: view})
	if err != nil {
		return [32]byte{}
	}
	return crypto.Keccak256Hash(enc)
}

// signHsVote signs a vote using BLS
func (h *Hotstuff) signHsVote(root [32]byte) (types.BLSPublicKey, types.BLSSignature, error) {
	if h.voteSigner == nil {
		return types.BLSPublicKey{}, types.BLSSignature{}, errors.New("vote signer not initialized")
	}
	return h.voteSigner.SignRoot(root)
}

// addrFromVotePubKeyAt resolves vote pubkey at a contextual snapshot (by baseHash or view)
// Returns (address, isBLSAvailable, error)
// When BLS is not available, returns (zero address, false, nil) to allow flow to continue
func (h *Hotstuff) addrFromVotePubKeyAt(pk types.BLSPublicKey, baseHash common.Hash, view uint64) (common.Address, bool, error) {
	snap, err := h.getSnapshotAtHashOrView(h.chain, baseHash, view)
	if err != nil {
		return common.Address{}, false, err
	}
	if snap == nil {
		return common.Address{}, false, errors.New("snapshot not found")
	}

	// Before Luban fork, BLS addresses are not available in validator set
	// Return false to indicate BLS validation should be skipped, but flow continues
	if baseHash != (common.Hash{}) {
		// Try canonical chain first, then HotStuff state
		var hdr *types.Header
		hdr = h.chain.GetHeaderByHash(baseHash)
		if hdr == nil {
			// Fallback: try HotStuff state (for pipelined blocks)
			if block := h.getBlockFromStateUnsafe(baseHash); block != nil {
				hdr = block.Header()
			}
		}

		if hdr != nil {
			if !h.chainConfig.IsLuban(hdr.Number) {
				log.Debug("Skip BLS pubkey resolution before Luban fork", "view", view, "blockNum", hdr.Number)
				return common.Address{}, false, nil
			}
		}
	}

	// Check if the snapshot has BLS addresses (even after Luban fork, early snapshots may not have them)
	// If all validators have zero BLS addresses, it means BLS addresses are not yet initialized
	hasBlsAddresses := false
	var zeroBlsKey types.BLSPublicKey
	for _, vi := range snap.Validators {
		if vi != nil && vi.VoteAddress != zeroBlsKey {
			hasBlsAddresses = true
			break
		}
	}

	if !hasBlsAddresses {
		log.Debug("Snapshot has no BLS addresses yet", "view", view, "snapNum", snap.Number)
		return common.Address{}, false, nil
	}

	for addr1, vi1 := range snap.Validators {
		log.Info("addrFromVotePubKeyAt", "view", view, "validators", addr1, "vote", fmt.Sprintf("%x", vi1.VoteAddress))
	}
	for addr, vi := range snap.Validators {
		if vi != nil && vi.VoteAddress == pk {
			return addr, true, nil
		}
	}
	return common.Address{}, true, errors.New("vote pubkey not in validator set")
}

// addrFromVotePubKey is a backward-compatible helper (resolves at head)
func (h *Hotstuff) addrFromVotePubKey(pk types.BLSPublicKey) (common.Address, bool, error) {
	return h.addrFromVotePubKeyAt(pk, common.Hash{}, 0)
}

// verifyHsBlsVoteAndResolveAddr verifies a BLS vote and resolves the voter address
// Returns (address, isBLSAvailable, error)
// When BLS is not available, returns (zero address, false, nil) to allow flow to continue without BLS validation
func (h *Hotstuff) verifyHsBlsVoteAndResolveAddr(vp *hs.VotePacket, targetHeader *types.Header) (common.Address, bool, error) {
	// Try to verify BLS signature and resolve address
	blsPub, err := bls.PublicKeyFromBytes(vp.VotePubKey[:])
	if err != nil {
		return common.Address{}, false, err
	}
	sig, err := bls.SignatureFromBytes(vp.Signature[:])
	if err != nil {
		return common.Address{}, false, err
	}
	root := h.hsVoteDigest(vp.BlockHash, vp.ViewNumber)
	// Convert [32]byte to []byte for BLS signature verification
	rootBytes := make([]byte, 32)
	copy(rootBytes, root[:])
	if !sig.Verify(blsPub, rootBytes) {
		return common.Address{}, false, errors.New("invalid bls signature")
	}

	// CRITICAL FIX: Use target block's PARENT for snapshot resolution
	// Following Parlia convention: validators are determined by parent block
	// This must match the snapshot used when forming and verifying QC
	base := vp.BlockHash
	if targetHeader != nil {
		base = targetHeader.ParentHash
		log.Debug("verifyHsBlsVoteAndResolveAddr: resolving address",
			"view", vp.ViewNumber,
			"targetBlock", vp.BlockHash.Hex()[:8],
			"targetNumber", targetHeader.Number.Uint64(),
			"parentHash", targetHeader.ParentHash.Hex()[:8])
	}
	addr, blsAvailable, err := h.addrFromVotePubKeyAt(vp.VotePubKey, base, vp.ViewNumber)
	if err != nil {
		log.Warn("verifyHsBlsVoteAndResolveAddr address from vote pubkey failed",
			"err", err,
			"view", vp.ViewNumber,
			"targetBlock", vp.BlockHash.Hex()[:8],
			"parentHash", base.Hex()[:8],
			"blsAvailable", blsAvailable)
		return common.Address{}, blsAvailable, err
	}
	if !blsAvailable {
		// BLS addresses not available, skip validation but continue flow
		log.Warn("verifyHsBlsVoteAndResolveAddr: BLS addresses not available, using fallback",
			"view", vp.ViewNumber,
			"targetBlock", vp.BlockHash.Hex()[:8],
			"parentHash", base.Hex()[:8])
		return common.Address{}, false, nil
	}
	log.Debug(""+
		": resolved address",
		"view", vp.ViewNumber,
		"addr", addr.Hex(),
		"targetBlock", vp.BlockHash.Hex()[:8])
	return addr, true, nil
}

// aggregateHsVoteSignatures aggregates multiple BLS vote signatures
func (h *Hotstuff) aggregateHsVoteSignatures(votes map[common.Address]*hs.VotePacket) []byte {
	sigs := make([][]byte, 0, len(votes))
	for _, v := range votes {
		sigs = append(sigs, v.Signature[:])
	}
	ms, err := bls.MultipleSignaturesFromBytes(sigs)
	if err != nil || len(ms) == 0 {
		return nil
	}
	return bls.AggregateSignatures(ms).Marshal()
}

// verifyAggregateQC verifies an aggregated QC signature with provided signer set from qc packet
func (h *Hotstuff) verifyAggregateQC(qc *hs.QuorumCertPacket) bool {
	// CRITICAL: Try to get target header with fallback to HotStuff state
	// In HotStuff pipelining, target block may not be in canonical chain yet
	var targetHeader *types.Header

	// Try canonical chain first
	targetHeader = h.chain.GetHeaderByHash(qc.TargetHash)

	// Fallback: try HotStuff state (for pipelined blocks)
	// CRITICAL: Use getBlockFromStateUnsafe because caller (OnHsProposal/OnHsQuorumCert/processQCPacket)
	// already holds h.lock.Lock(), calling GetBlockFromState would deadlock trying to acquire RLock
	if targetHeader == nil {
		log.Debug("verifyAggregateQC: target not in canonical chain, trying HotStuff state",
			"targetHash", qc.TargetHash.Hex()[:8],
			"view", qc.ViewNumber)
		if block := h.getBlockFromStateUnsafe(qc.TargetHash); block != nil {
			targetHeader = block.Header()
			log.Debug("verifyAggregateQC: got target from HotStuff state",
				"targetHash", qc.TargetHash.Hex()[:8],
				"number", block.NumberU64())
		}
	}

	// Before Luban fork, BLS aggregate QC is not supported, skip verification but allow QC
	if targetHeader != nil {
		if !h.chainConfig.IsLuban(targetHeader.Number) {
			log.Debug("Skip aggregate QC verification before Luban fork (allow QC)",
				"block", targetHeader.Number, "view", qc.ViewNumber)
			return true
		}
	} else {
		log.Warn("verifyAggregateQC: target header not found (neither in chain nor HotStuff state)",
			"targetHash", qc.TargetHash.Hex()[:8],
			"view", qc.ViewNumber)
		// Cannot verify without target header, but allow QC to avoid blocking consensus
		// This might happen if we're far behind and haven't received the block yet
		return true
	}

	// Get snapshot at target block's parent (following Parlia convention)
	var snap *Snapshot
	var err error
	snap, err = h.snapshot(h.chain, targetHeader.Number.Uint64()-1, targetHeader.ParentHash, nil)
	if err != nil {
		log.Warn("verifyAggregateQC: failed to get snapshot from target parent",
			"targetNumber", targetHeader.Number.Uint64(),
			"targetHash", qc.TargetHash.Hex()[:8],
			"parentHash", targetHeader.ParentHash.Hex()[:8],
			"error", err)
		// Cannot verify without snapshot, but allow QC to avoid blocking consensus
		return true
	}

	// Check if BLS addresses are initialized in snapshot
	validators := snap.validators()
	hasBlsAddresses := false
	for _, addr := range validators {
		if snap.Validators[addr].VoteAddress != (types.BLSPublicKey{}) {
			hasBlsAddresses = true
			break
		}
	}

	// If BLS addresses not initialized, skip verification but allow QC
	if !hasBlsAddresses {
		log.Debug("Skip aggregate QC verification (BLS addresses not initialized)",
			"view", qc.ViewNumber,
			"snapNumber", snap.Number,
			"targetNumber", targetHeader.Number.Uint64())
		return true
	}

	log.Debug("verifyAggregateQC: checking QC",
		"view", qc.ViewNumber,
		"targetHash", qc.TargetHash.Hex()[:8],
		"signersSet", qc.SignersSet,
		"aggSigLen", len(qc.AggregateSig),
		"validatorCount", len(validators))

	votedKeys := make([]bls.PublicKey, 0, len(validators))
	for idx := range validators {
		if (qc.SignersSet & (types.ValidatorsBitSet(1) << uint(idx))) != 0 {
			pk, err := bls.PublicKeyFromBytes(snap.Validators[validators[idx]].VoteAddress[:])
			if err != nil {
				log.Debug("Failed to parse BLS public key for QC verification", "idx", idx, "validator", validators[idx].Hex(), "err", err)
				return false
			}
			votedKeys = append(votedKeys, pk)
		}
	}

	log.Debug("verifyAggregateQC: collected voted keys",
		"votedCount", len(votedKeys),
		"requiredQuorum", QuorumSize(len(validators)))

	// threshold: >= 2/3
	if len(votedKeys) < QuorumSize(len(validators)) {
		log.Debug("verifyAggregateQC: insufficient votes",
			"votedCount", len(votedKeys),
			"requiredQuorum", QuorumSize(len(validators)))
		return false
	}

	// message is the same vote digest as individual votes
	root := h.hsVoteDigest(qc.TargetHash, qc.ViewNumber)
	aggSig, err := bls.SignatureFromBytes(qc.AggregateSig)
	if err != nil {
		log.Debug("verifyAggregateQC: failed to parse aggregate signature", "err", err)
		return false
	}

	// Convert [32]byte to [32]byte for BLS signature verification
	var rootBytes [32]byte
	copy(rootBytes[:], root[:])

	result := aggSig.FastAggregateVerify(votedKeys, rootBytes)
	if !result {
		log.Debug("verifyAggregateQC: BLS FastAggregateVerify failed",
			"view", qc.ViewNumber,
			"targetHash", qc.TargetHash.Hex()[:8],
			"digest", common.BytesToHash(root[:]).Hex()[:16],
			"votedCount", len(votedKeys))
	} else {
		log.Debug("verifyAggregateQC: BLS FastAggregateVerify succeeded",
			"view", qc.ViewNumber,
			"targetHash", qc.TargetHash.Hex()[:8])
	}

	return result
}

// hsTimeoutDigest computes the digest for a timeout message
func (h *Hotstuff) hsTimeoutDigest(to *hs.TimeoutPacket) [32]byte {
	type toTuple struct {
		ViewNumber uint64
		HighQCView uint64
		HighQCHash common.Hash
	}
	enc, err := rlp.EncodeToBytes(&toTuple{ViewNumber: to.ViewNumber, HighQCView: to.HighQCView, HighQCHash: to.HighQCHash})
	if err != nil {
		return [32]byte{}
	}
	return crypto.Keccak256Hash(enc)
}

// verifyHsTimeoutAndResolveAddr verifies a BLS timeout signature and resolves the sender address
// Returns (address, isBLSAvailable, error)
// When BLS is not available, returns (zero address, false, nil) to allow flow to continue without BLS validation
func (h *Hotstuff) verifyHsTimeoutAndResolveAddr(to *hs.TimeoutPacket) (common.Address, bool, error) {
	blsPub, err := bls.PublicKeyFromBytes(to.VotePubKey[:])
	if err != nil {
		return common.Address{}, false, err
	}
	sig, err := bls.SignatureFromBytes(to.Signature[:])
	if err != nil {
		return common.Address{}, false, err
	}
	root := h.hsTimeoutDigest(to)
	// Convert [32]byte to []byte for BLS signature verification
	rootBytes := make([]byte, 32)
	copy(rootBytes, root[:])
	if !sig.Verify(blsPub, rootBytes) {
		return common.Address{}, false, errors.New("invalid timeout bls signature")
	}
	baseHash := to.HighQCHash
	addr, blsAvailable, err := h.addrFromVotePubKeyAt(to.VotePubKey, baseHash, to.ViewNumber)
	if err != nil {
		log.Error("verifyHsTimeoutAndResolveAddr address from vote pubkey failed", "err", err, "view", to.ViewNumber)
		return common.Address{}, blsAvailable, err
	}
	if !blsAvailable {
		// BLS addresses not available, skip validation but continue flow
		log.Debug("Skip timeout verification, BLS addresses not available", "view", to.ViewNumber)
		return common.Address{}, false, nil
	}
	return addr, true, nil
}
