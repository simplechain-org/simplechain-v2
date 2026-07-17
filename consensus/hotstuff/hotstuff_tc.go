package hotstuff

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/hs"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

const (
	tcFlag       byte = 0xB6
	tcFooterSize      = 5 // uint32 encoded payload length + marker
)

// assembleHighTC aggregates timeout messages and creates TimeoutCert for this view
// CRITICAL FIX: Refactored to avoid holding lock during blocking operations.
func (h *Hotstuff) assembleHighTC(chain consensus.ChainHeaderReader, header *types.Header) error {
	h.lock.RLock()
	st := h.getHsState()
	if st == nil {
		h.lock.RUnlock()
		return nil
	}

	timeoutMaps := make(map[uint64]map[common.Address]*hs.TimeoutPacket, len(st.timeouts))
	for view, timeoutMap := range st.timeouts {
		copied := make(map[common.Address]*hs.TimeoutPacket, len(timeoutMap))
		for addr, packet := range timeoutMap {
			copied[addr] = packet
		}
		timeoutMaps[view] = copied
	}
	h.lock.RUnlock()

	var (
		highestTCView  uint64
		timeoutMapCopy map[common.Address]*hs.TimeoutPacket
		requiredQuorum int
	)
	for view, timeoutMap := range timeoutMaps {
		count, qsize, err := h.timeoutQuorum(timeoutMap)
		if err == nil && count >= qsize && (timeoutMapCopy == nil || view > highestTCView) {
			highestTCView, timeoutMapCopy, requiredQuorum = view, timeoutMap, qsize
		}
	}
	if timeoutMapCopy != nil {
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
			"requiredQuorum", requiredQuorum)
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
	highestQC := selectTimeoutHighQC(timeoutMap)
	if highestQC == nil {
		return nil, fmt.Errorf("timeout certificate has no HighQC")
	}

	// Create the timeout certificate
	tc := &hsTimeoutCert{
		View:    view,
		HighQC:  highestQC,
		Signers: make([]common.Address, 0, len(timeoutMap)),
	}

	// Collect signers and aggregate signatures
	sigs := make([][]byte, 0, len(timeoutMap))

	snap, err := h.getSnapshotAtHashOrView(h.chain, highestQC.BlockHash, highestQC.View)
	if err != nil || snap == nil {
		return nil, fmt.Errorf("resolve timeout validator snapshot: %w", err)
	}

	validators := snap.validators()
	vsetIndex := make(map[common.Address]int, len(validators))
	for i, v := range validators {
		vsetIndex[v] = i
	}
	var bitset uint64
	for addr, pkt := range timeoutMap {
		if _, ok := snap.Validators[addr]; !ok || pkt.ViewNumber != view {
			continue
		}
		tc.Signers = append(tc.Signers, addr)
		sigs = append(sigs, pkt.Signature[:])
		if idx, ok := vsetIndex[addr]; ok && idx < 64 {
			bitset |= (1 << uint(idx))
		}
	}
	if len(tc.Signers) < QuorumSize(len(validators)) {
		return nil, fmt.Errorf("insufficient validator timeouts: have %d want %d", len(tc.Signers), QuorumSize(len(validators)))
	}
	tc.SignerSet = types.ValidatorsBitSet(bitset)
	if ms, err := bls.MultipleSignaturesFromBytes(sigs); err == nil && len(ms) > 0 {
		copy(tc.AggSig[:], bls.AggregateSignatures(ms).Marshal())
	}

	log.Debug("Created TimeoutCert with aggregate signature",
		"view", view,
		"signerCount", len(tc.Signers),
		"highQCView", highestQC.View,
		"signerSet", tc.SignerSet,
		"aggSigLen", len(tc.AggSig),
		"validatorCount", len(validators))

	return tc, nil
}

func selectTimeoutHighQC(timeoutMap map[common.Address]*hs.TimeoutPacket) *HsQC {
	var highestQC *HsQC
	for _, timeout := range timeoutMap {
		if timeout == nil {
			continue
		}
		if highestQC == nil || timeout.HighQCView > highestQC.View ||
			(timeout.HighQCView == highestQC.View && bytes.Compare(timeout.HighQCHash[:], highestQC.BlockHash[:]) > 0) {
			highestQC = &HsQC{
				BlockHash: timeout.HighQCHash, View: timeout.HighQCView,
				SignersSet: timeout.HighQCSignersSet, Sig: common.CopyBytes(timeout.HighQCAggSig),
			}
		}
	}
	return highestQC
}

func (h *Hotstuff) timeoutQuorum(timeoutMap map[common.Address]*hs.TimeoutPacket) (int, int, error) {
	highestQC := selectTimeoutHighQC(timeoutMap)
	if highestQC == nil {
		return 0, 0, errors.New("timeout set has no HighQC")
	}
	snap, err := h.getSnapshotAtHashOrView(h.chain, highestQC.BlockHash, highestQC.View)
	if err != nil || snap == nil {
		return 0, 0, err
	}
	count := 0
	for addr := range timeoutMap {
		if _, ok := snap.Validators[addr]; ok {
			count++
		}
	}
	return count, QuorumSize(len(snap.validators())), nil
}

// embedTimeoutCert embeds a TimeoutCert into the block header
func (h *Hotstuff) embedTimeoutCert(header *types.Header, tc *hsTimeoutCert) error {
	// Encode the timeout certificate
	tcData, err := h.encodeTimeoutCert(tc)
	if err != nil {
		return fmt.Errorf("failed to encode timeout cert: %w", err)
	}

	// TC is a deterministic trailer immediately before SyncInfo:
	// [base extra][tcData][uint32 tcDataLen][tcFlag][SyncInfo][seal].
	tcLength := make([]byte, 4)
	binary.LittleEndian.PutUint32(tcLength, uint32(len(tcData)))
	insertData := make([]byte, 0, len(tcData)+tcFooterSize)
	insertData = append(insertData, tcData...)
	insertData = append(insertData, tcLength...)
	insertData = append(insertData, tcFlag)

	payload := header.Extra
	if len(payload) < extraSeal {
		return fmt.Errorf("header extra missing seal")
	}
	insertAt := len(payload) - extraSeal - syncInfoSize(header, h.chainConfig)
	if insertAt < extraVanity {
		return fmt.Errorf("malformed HotStuff extra-data")
	}
	header.Extra = append(payload[:insertAt], append(insertData, payload[insertAt:]...)...)

	log.Debug("Embedded TimeoutCert in header", "tcDataLen", len(tcData), "view", tc.View)
	return nil
}

// encodeTimeoutCert encodes a TimeoutCert for embedding in block header
func (h *Hotstuff) encodeTimeoutCert(tc *hsTimeoutCert) ([]byte, error) {
	// RLP encoding with signer bitset and agg sig
	type encTC struct {
		View             uint64
		HighQCView       uint64
		HighQCHash       common.Hash
		HighQCSignerSet  uint64
		HighQCAggSig     []byte
		TimeoutSignerSet uint64
		TimeoutAggSig    []byte
	}
	var hqv uint64
	var hqh common.Hash
	if tc.HighQC != nil {
		hqv = tc.HighQC.View
		hqh = tc.HighQC.BlockHash
	}
	var hqSignerSet uint64
	var hqSig []byte
	if tc.HighQC != nil {
		hqSignerSet = uint64(tc.HighQC.SignersSet)
		hqSig = common.CopyBytes(tc.HighQC.Sig)
	}
	e := encTC{
		View:             tc.View,
		HighQCView:       hqv,
		HighQCHash:       hqh,
		HighQCSignerSet:  hqSignerSet,
		HighQCAggSig:     hqSig,
		TimeoutSignerSet: uint64(tc.SignerSet),
		TimeoutAggSig:    tc.AggSig[:],
	}
	return rlp.EncodeToBytes(&e)
}

// timeoutCertSize returns the exact TC trailer size. Malformed or misplaced
// markers are ignored here and rejected by parseTimeoutCert during validation.
func timeoutCertSize(header *types.Header, chainConfig *params.ChainConfig) int {
	if header == nil || header.Number == nil || chainConfig == nil || !chainConfig.IsHotstuff(header.Number) ||
		len(header.Extra) < extraVanity+extraSeal+tcFooterSize {
		return 0
	}
	end := len(header.Extra) - extraSeal - syncInfoSize(header, chainConfig)
	if end < extraVanity+tcFooterSize || header.Extra[end-1] != tcFlag {
		return 0
	}
	length := int(binary.LittleEndian.Uint32(header.Extra[end-tcFooterSize : end-1]))
	if length <= 0 || end-tcFooterSize-length < extraVanity {
		return 0
	}
	return length + tcFooterSize
}

// parseTimeoutCert decodes the deterministic TC trailer before SyncInfo.
func (h *Hotstuff) parseTimeoutCert(header *types.Header) (*hsTimeoutCert, error) {
	if header == nil || len(header.Extra) < extraVanity+extraSeal {
		return nil, nil
	}
	payload := header.Extra
	end := len(payload) - extraSeal - syncInfoSize(header, h.chainConfig)
	if end >= extraVanity+tcFooterSize && payload[end-1] == tcFlag {
		l := int(binary.LittleEndian.Uint32(payload[end-tcFooterSize : end-1]))
		start := end - tcFooterSize - l
		if l <= 0 || start < extraVanity {
			return nil, fmt.Errorf("invalid TimeoutCert framing")
		}
		type encTC struct {
			View             uint64
			HighQCView       uint64
			HighQCHash       common.Hash
			HighQCSignerSet  uint64
			HighQCAggSig     []byte
			TimeoutSignerSet uint64
			TimeoutAggSig    []byte
		}
		var e encTC
		if err := rlp.DecodeBytes(payload[start:start+l], &e); err != nil {
			log.Debug("parseTimeoutCert: failed to decode TC data",
				"error", err,
				"tcDataLength", l,
				"availableLength", end-start-tcFooterSize)
			return nil, fmt.Errorf("invalid TimeoutCert encoding: %w", err)
		}

		if len(e.TimeoutAggSig) != types.BLSSignatureLength {
			log.Debug("parseTimeoutCert: invalid AggSig length",
				"expected", types.BLSSignatureLength,
				"got", len(e.TimeoutAggSig))
			return nil, fmt.Errorf("invalid TimeoutCert aggregate signature length: have %d want %d", len(e.TimeoutAggSig), types.BLSSignatureLength)
		}
		tc := &hsTimeoutCert{
			View: e.View,
			HighQC: &HsQC{
				View:       e.HighQCView,
				BlockHash:  e.HighQCHash,
				SignersSet: types.ValidatorsBitSet(e.HighQCSignerSet),
				Sig:        common.CopyBytes(e.HighQCAggSig),
			},
			SignerSet: types.ValidatorsBitSet(e.TimeoutSignerSet),
		}
		copy(tc.AggSig[:], e.TimeoutAggSig)
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
	if !h.verifyHighQCPayload(tc.HighQC.BlockHash, tc.HighQC.View, tc.HighQC.SignersSet, tc.HighQC.Sig) {
		log.Debug("verifyTimeoutCert: invalid carried HighQC", "view", tc.View, "highQCView", tc.HighQC.View)
		return false
	}

	snap, err := h.getSnapshotAtHashOrView(h.chain, tc.HighQC.BlockHash, tc.HighQC.View)
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

	root := h.hsTimeoutDigest(&hs.TimeoutPacket{ViewNumber: tc.View})
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
		View: nv.HighTCView,
		HighQC: &HsQC{
			View:       nv.HighQCView,
			BlockHash:  nv.HighQCHash,
			SignersSet: nv.HighQCSignersSet,
			Sig:        common.CopyBytes(nv.HighQCAggSig),
		},
		SignerSet: nv.TimeoutSignersSet,
	}
	tc.AggSig = nv.TimeoutAggSig
	return tc
}
