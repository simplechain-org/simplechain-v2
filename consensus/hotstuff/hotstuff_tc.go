package hotstuff

import (
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

// assembleHighTC aggregates timeout messages and creates TimeoutCert for this view
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations.
func (h *Hotstuff) assembleHighTC(chain consensus.ChainHeaderReader, header *types.Header) error {
	// Phase 1: Get snapshot (no lock - may block)
	head := chain.CurrentHeader()
	if head == nil {
		return nil
	}
	snap, err := h.snapshot(chain, head.Number.Uint64(), head.Hash(), nil)
	if err != nil {
		return err
	}
	qsize := QuorumSize(len(snap.validators()))
	validators := snap.validators()

	// Phase 2: Find highest TC view and copy timeout map (short lock)
	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		return nil
	}

	// Find the highest view with sufficient timeouts to form a TC
	var highestTCView uint64 = 0

	// Check all timeout collections to find the highest valid TC
	for view, timeoutMap := range st.timeouts {
		if len(timeoutMap) >= qsize && view > highestTCView {
			// Verify timeout messages are valid
			if h.validateTimeoutMessages(timeoutMap, view, validators) {
				highestTCView = view
			}
		}
	}

	// CRITICAL: Copy timeout map under lock before releasing
	var timeoutMapCopy map[common.Address]*hs.TimeoutPacket
	if highestTCView > 0 {
		timeoutMapCopy = make(map[common.Address]*hs.TimeoutPacket, len(st.timeouts[highestTCView]))
		for addr, tp := range st.timeouts[highestTCView] {
			timeoutMapCopy[addr] = tp
		}
	}
	h.lock.RUnlock()

	// If we found a valid TC, create and embed it
	if highestTCView > 0 {
		tc, err := h.createTimeoutCert(highestTCView, timeoutMapCopy)
		if err != nil {
			log.Warn("Failed to create timeout certificate", "view", highestTCView, "err", err)
			return err
		}

		// Embed the TC into the header
		err = h.embedTimeoutCert(header, tc)
		if err != nil {
			log.Warn("Failed to embed timeout certificate", "view", highestTCView, "err", err)
			return err
		}

		log.Info("Assembled and embedded TimeoutCert",
			"view", highestTCView,
			"timeoutCount", len(timeoutMapCopy),
			"requiredQuorum", qsize)
	}

	return nil
}

// validateTimeoutMessages validates a collection of timeout messages for a specific view
func (h *Hotstuff) validateTimeoutMessages(timeoutMap map[common.Address]*hs.TimeoutPacket, view uint64, validators []common.Address) bool {
	// build validator set
	vset := make(map[common.Address]struct{}, len(validators))
	for _, v := range validators {
		vset[v] = struct{}{}
	}
	for addr, timeout := range timeoutMap {
		// Check if the sender is a valid validator
		if _, isValidator := vset[addr]; !isValidator {
			log.Warn("Timeout message from non-validator", "addr", addr.Hex(), "view", view)
			return false
		}

		// Check if the timeout is for the correct view
		if timeout.ViewNumber != view {
			log.Warn("Timeout message view mismatch", "expected", view, "got", timeout.ViewNumber)
			return false
		}

		// Verify timeout BLS signature and that resolved address matches the map key
		resolved, blsAvailable, err := h.verifyHsTimeoutAndResolveAddr(timeout)
		if err != nil {
			log.Warn("Invalid timeout signature", "addr", addr.Hex(), "view", view, "err", err)
			return false
		}
		// If BLS not available (pre-Luban), skip address verification (using fallback address)
		if blsAvailable && resolved != addr {
			log.Warn("Timeout sender mismatch after verification", "claimed", addr.Hex(), "resolved", resolved.Hex(), "view", view)
			return false
		}
	}
	return true
}

// createTimeoutCert creates a TimeoutCert from aggregated timeout messages
func (h *Hotstuff) createTimeoutCert(view uint64, timeoutMap map[common.Address]*hs.TimeoutPacket) (*hsTimeoutCert, error) {
	// Extract highest QC from timeout messages
	var highestQC *HsQC
	var highestQCView uint64 = 0

	for _, timeout := range timeoutMap {
		// CRITICAL FIX: Compare HighQCView, not ViewNumber!
		// timeout.ViewNumber is the view that timed out (e.g., 46)
		// timeout.HighQCView is the view of the highest QC the sender has (e.g., 45 or earlier)
		if timeout.HighQCView > highestQCView {
			highestQCView = timeout.HighQCView
			// The TimeoutPacket carries HighQC view/hash only; no QC aggregate
			// signature is present to reconstruct. Leave Sig/SignersSet empty.
			highestQC = &HsQC{BlockHash: timeout.HighQCHash, View: timeout.HighQCView}
		}
	}

	// Create the timeout certificate
	tc := &hsTimeoutCert{
		View:    view,
		HighQC:  highestQC,
		Signers: make([]common.Address, 0, len(timeoutMap)),
	}

	// Collect signers and aggregate signatures
	sigs := make([][]byte, 0, len(timeoutMap))

	// CRITICAL FIX: Use snapshot from HighQC block's PARENT, not current head
	// This ensures SignerSet bitset matches the validator set at the time of timeout
	// Following Parlia convention: validators are determined by parent block
	var snap *Snapshot
	if highestQC != nil && highestQC.BlockHash != (common.Hash{}) {
		if hdr := h.chain.GetHeaderByHash(highestQC.BlockHash); hdr != nil {
			// Use parent block's snapshot (number-1, ParentHash)
			snap, _ = h.snapshot(h.chain, hdr.Number.Uint64()-1, hdr.ParentHash, nil)
		}
	}
	if snap == nil {
		// Fallback to current head if HighQC block not found
		head := h.chain.CurrentHeader()
		snap, _ = h.snapshot(h.chain, head.Number.Uint64(), head.Hash(), nil)
		log.Debug("createTimeoutCert: using head snapshot (highQC block not found)", "view", view)
	}

	validators := snap.validators()
	vsetIndex := make(map[common.Address]int, len(validators))
	for i, v := range validators {
		vsetIndex[v] = i
	}
	var bitset uint64
	for addr, pkt := range timeoutMap {
		tc.Signers = append(tc.Signers, addr)
		sigs = append(sigs, pkt.Signature[:])
		if idx, ok := vsetIndex[addr]; ok && idx < 64 {
			bitset |= (1 << uint(idx))
		}
	}
	tc.SignerSet = types.ValidatorsBitSet(bitset)
	if ms, err := bls.MultipleSignaturesFromBytes(sigs); err == nil && len(ms) > 0 {
		copy(tc.AggSig[:], bls.AggregateSignatures(ms).Marshal())
	}

	log.Debug("Created TimeoutCert with aggregate signature",
		"view", view,
		"signerCount", len(tc.Signers),
		"highQCView", highestQCView,
		"signerSet", tc.SignerSet,
		"aggSigLen", len(tc.AggSig),
		"validatorCount", len(validators))

	return tc, nil
}

// embedTimeoutCert embeds a TimeoutCert into the block header
func (h *Hotstuff) embedTimeoutCert(header *types.Header, tc *hsTimeoutCert) error {
	// Encode the timeout certificate
	tcData, err := h.encodeTimeoutCert(tc)
	if err != nil {
		return fmt.Errorf("failed to encode timeout cert: %w", err)
	}

	// Embed into header Extra field with a special marker
	const tcFlag byte = 0xB6 // Different from QC flag (0xA5)
	marker := []byte{tcFlag}
	tcLength := make([]byte, 4)
	binary.LittleEndian.PutUint32(tcLength, uint32(len(tcData)))

	// Find insertion point (before signature seal)
	payload := header.Extra
	end := len(payload)
	if end >= extraSeal {
		end = end - extraSeal
	}

	// Insert: marker + length + tcData
	insertData := append(marker, append(tcLength, tcData...)...)
	header.Extra = append(payload[:end], append(insertData, payload[end:]...)...)

	log.Debug("Embedded TimeoutCert in header", "tcDataLen", len(tcData), "view", tc.View)
	return nil
}

// encodeTimeoutCert encodes a TimeoutCert for embedding in block header
func (h *Hotstuff) encodeTimeoutCert(tc *hsTimeoutCert) ([]byte, error) {
	// RLP encoding with signer bitset and agg sig
	type encTC struct {
		View       uint64
		HighQCView uint64
		HighQCHash common.Hash
		SignerSet  uint64
		AggSig     []byte
	}
	var hqv uint64
	var hqh common.Hash
	if tc.HighQC != nil {
		hqv = tc.HighQC.View
		hqh = tc.HighQC.BlockHash
	}
	e := encTC{View: tc.View, HighQCView: hqv, HighQCHash: hqh, SignerSet: uint64(tc.SignerSet), AggSig: tc.AggSig[:]}
	return rlp.EncodeToBytes(&e)
}

// parseTimeoutCert scans header.Extra for a timeout cert block (flag 0xB6) and decodes it
func (h *Hotstuff) parseTimeoutCert(header *types.Header) (*hsTimeoutCert, error) {
	const tcFlag byte = 0xB6
	payload := header.Extra
	end := len(payload)
	if end >= extraSeal {
		end = end - extraSeal
	}
	for i := 0; i < end; i++ {
		if payload[i] != tcFlag {
			continue
		}
		if i+1+4 > end {
			break
		}
		l := binary.LittleEndian.Uint32(payload[i+1 : i+5])
		if i+5+int(l) > end {
			break
		}
		if i+5+int(l) != end {
			continue
		}
		type encTC struct {
			View       uint64
			HighQCView uint64
			HighQCHash common.Hash
			SignerSet  uint64
			AggSig     []byte
		}
		var e encTC
		if err := rlp.DecodeBytes(payload[i+5:i+5+int(l)], &e); err != nil {
			log.Debug("parseTimeoutCert: failed to decode TC data",
				"error", err,
				"tcDataLength", l,
				"availableLength", end-i-5)
			return nil, fmt.Errorf("invalid TimeoutCert encoding: %w", err)
		}

		// Validate AggSig length
		if len(e.AggSig) != types.BLSSignatureLength {
			log.Debug("parseTimeoutCert: invalid AggSig length",
				"expected", types.BLSSignatureLength,
				"got", len(e.AggSig))
			return nil, fmt.Errorf("invalid TimeoutCert aggregate signature length: have %d want %d", len(e.AggSig), types.BLSSignatureLength)
		}

		tc := &hsTimeoutCert{View: e.View, HighQC: &HsQC{View: e.HighQCView, BlockHash: e.HighQCHash}, SignerSet: types.ValidatorsBitSet(e.SignerSet)}
		copy(tc.AggSig[:], e.AggSig)
		return tc, nil
	}
	return nil, nil
}

// verifyTimeoutCert verifies the aggregated signature in TimeoutCert against signer set
func (h *Hotstuff) verifyTimeoutCert(tc *hsTimeoutCert) bool {
	if tc == nil {
		return true
	}
	if tc.HighQC == nil || tc.HighQC.BlockHash == (common.Hash{}) {
		return false
	}
	if tc.SignerSet == 0 {
		return false
	}

	// Before Luban fork, BLS timeout cert is not supported, skip verification but allow TC
	if tc.HighQC != nil && tc.HighQC.BlockHash != (common.Hash{}) {
		if hdr := h.chain.GetHeaderByHash(tc.HighQC.BlockHash); hdr != nil {
			if !h.chainConfig.IsLuban(hdr.Number) {
				log.Debug("Skip timeout cert verification before Luban fork (allow TC)", "block", hdr.Number, "view", tc.View)
				// Return true to allow chain to progress before Luban fork
				return true
			}
		}
	}

	// CRITICAL FIX: Use snapshot from HighQC block's PARENT
	// Following Parlia convention: validators are determined by parent block
	var snap *Snapshot
	var err error
	if tc.HighQC != nil && tc.HighQC.BlockHash != (common.Hash{}) {
		if hdr := h.chain.GetHeaderByHash(tc.HighQC.BlockHash); hdr != nil {
			// Use parent block's snapshot (number-1, ParentHash)
			snap, err = h.snapshot(h.chain, hdr.Number.Uint64()-1, hdr.ParentHash, nil)
		}
	}
	if snap == nil {
		// Fallback to getSnapshotAtHashOrView
		snap, err = h.getSnapshotAtHashOrView(h.chain, tc.HighQC.BlockHash, tc.View)
	}
	if err != nil || snap == nil {
		return false
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

	// After Luban, TC verification requires initialized BLS vote addresses.
	if !hasBlsAddresses {
		log.Warn("verifyTimeoutCert: BLS addresses not initialized", "view", tc.View)
		return false
	}

	log.Debug("verifyTimeoutCert: checking TC",
		"view", tc.View,
		"highQCHash", tc.HighQC.BlockHash.Hex()[:8],
		"highQCView", tc.HighQC.View,
		"signerSet", tc.SignerSet,
		"aggSigLen", len(tc.AggSig),
		"validatorCount", len(validators))

	voted := make([]bls.PublicKey, 0, len(validators))
	for idx := range validators {
		if (tc.SignerSet & (types.ValidatorsBitSet(1) << uint(idx))) != 0 {
			pk, err := bls.PublicKeyFromBytes(snap.Validators[validators[idx]].VoteAddress[:])
			if err != nil {
				log.Debug("Failed to parse BLS public key for TC verification", "idx", idx, "validator", validators[idx].Hex(), "err", err)
				return false
			}
			voted = append(voted, pk)
		}
	}

	log.Debug("verifyTimeoutCert: collected voted keys",
		"votedCount", len(voted),
		"requiredQuorum", QuorumSize(len(validators)))

	if len(voted) < QuorumSize(len(validators)) {
		log.Debug("verifyTimeoutCert: insufficient timeouts",
			"votedCount", len(voted),
			"requiredQuorum", QuorumSize(len(validators)))
		return false
	}

	root := h.hsTimeoutDigest(&hs.TimeoutPacket{ViewNumber: tc.View, HighQCView: tc.HighQC.View, HighQCHash: tc.HighQC.BlockHash})
	var rootBytes [32]byte
	copy(rootBytes[:], root[:])
	agg, err := bls.SignatureFromBytes(tc.AggSig[:])
	if err != nil {
		log.Debug("verifyTimeoutCert: failed to parse aggregate signature", "err", err)
		return false
	}

	result := agg.FastAggregateVerify(voted, rootBytes)
	if !result {
		log.Debug("verifyTimeoutCert: BLS FastAggregateVerify failed",
			"view", tc.View,
			"highQCHash", tc.HighQC.BlockHash.Hex()[:8],
			"digest", common.BytesToHash(root[:]).Hex()[:16],
			"votedCount", len(voted))
	} else {
		log.Debug("verifyTimeoutCert: BLS FastAggregateVerify succeeded",
			"view", tc.View,
			"highQCHash", tc.HighQC.BlockHash.Hex()[:8])
	}

	return result
}

func (h *Hotstuff) timeoutCertFromNewView(nv *hs.NewViewPacket) *hsTimeoutCert {
	if nv == nil || nv.HighTCView == 0 {
		return nil
	}
	if nv.TimeoutSignersSet == 0 {
		return nil
	}
	tc := &hsTimeoutCert{
		View:      nv.HighTCView,
		HighQC:    &HsQC{View: nv.HighQCView, BlockHash: nv.HighQCHash},
		SignerSet: nv.TimeoutSignersSet,
	}
	tc.AggSig = nv.TimeoutAggSig
	return tc
}
