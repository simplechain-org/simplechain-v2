package hotstuff

import (
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

func syncInfoSize(header *types.Header) int {
	if header.Number == nil || header.Number.Uint64() == 0 {
		return 0
	}
	proofSyncInfoStart := len(header.Extra) - extraSeal - (1 + syncInfoProofTotalSize)
	if proofSyncInfoStart > extraVanity && proofSyncInfoStart < len(header.Extra) && header.Extra[proofSyncInfoStart] == hsProofFlag {
		return 1 + syncInfoProofTotalSize
	}
	syncInfoStart := len(header.Extra) - extraSeal - (1 + syncInfoTotalSize)
	if syncInfoStart <= extraVanity || syncInfoStart >= len(header.Extra) {
		return 0
	}
	if header.Extra[syncInfoStart] != hsFlag {
		return 0
	}
	return 1 + syncInfoTotalSize
}

// parseSyncInfo decodes minimal SyncInfo from header.Extra
func parseSyncInfo(header *types.Header) (has bool, hqcView uint64, hqcHash common.Hash, htcView uint64) {
	has, hqcView, hqcHash, htcView, _, _, _ = parseSyncInfoWithProof(header)
	return has, hqcView, hqcHash, htcView
}

func parseSyncInfoWithProof(header *types.Header) (has bool, hqcView uint64, hqcHash common.Hash, htcView uint64, signersSet types.ValidatorsBitSet, sig []byte, hasProof bool) {
	// CRITICAL FIX: Don't scan for hsFlag! It can appear in validator BLS pubkeys!
	// Instead, calculate the exact position where syncInfo should be.
	//
	// header.Extra layout:
	// [extraVanity(32)] | [validatorNum(1)] | [validators(num*validatorBytesLength)] |
	// [turnLength(1, optional)] | [syncInfo(49)] | [extraSeal(65)]

	payload := header.Extra
	if len(payload) < extraVanity+extraSeal {
		return false, 0, common.Hash{}, 0, 0, nil, false
	}

	size := syncInfoSize(header)
	if size == 0 {
		return false, 0, common.Hash{}, 0, 0, nil, false
	}

	// Calculate where syncInfo should start (right before extraSeal).
	syncInfoStart := len(payload) - extraSeal - size

	// Parse syncInfo from the exact position
	hqcView = binary.LittleEndian.Uint64(payload[syncInfoStart+1 : syncInfoStart+1+viewSize])
	copy(hqcHash[:], payload[syncInfoStart+1+viewSize:syncInfoStart+1+viewSize+hashSize])
	htcView = binary.LittleEndian.Uint64(payload[syncInfoStart+1+viewSize+hashSize : syncInfoStart+1+syncInfoTotalSize])
	if payload[syncInfoStart] == hsProofFlag {
		offset := syncInfoStart + 1 + syncInfoTotalSize
		signersSet = types.ValidatorsBitSet(binary.LittleEndian.Uint32(payload[offset : offset+countSize]))
		offset += countSize
		sig = common.CopyBytes(payload[offset : offset+types.BLSSignatureLength])
		hasProof = signersSet != 0 && len(sig) == types.BLSSignatureLength
	}

	// Sanity check: view should be reasonable (not corrupted data from BLS pubkey)
	// For a valid syncInfo, view should be close to block number
	maxReasonableView := header.Number.Uint64() + 100000 // Allow buffer for future blocks
	if hqcView > maxReasonableView {
		log.Warn("parseSyncInfo: detected unreasonably large view, rejecting as corrupted",
			"blockNumber", header.Number.Uint64(),
			"parsedView", hqcView,
			"maxReasonable", maxReasonableView,
			"syncInfoStart", syncInfoStart,
			"extraLen", len(payload))
		return false, 0, common.Hash{}, 0, 0, nil, false
	}

	return true, hqcView, hqcHash, htcView, signersSet, sig, hasProof
}

// getViewFromHeader extracts the view number from a block header.
// For HotStuff blocks with SyncInfo, the view is (hqcView+1) or htcView+1 if TC is present.
// Falls back to block number if no SyncInfo is present (for backward compatibility).
func getViewFromHeader(header *types.Header) uint64 {
	has, hqcView, _, htcView := parseSyncInfo(header)
	if has {
		// If TC is present, the view is htcView+1
		if htcView > 0 && htcView >= hqcView {
			return htcView + 1
		}
		// Otherwise, the view is hqcView+1
		return hqcView + 1
	}
	// Fallback for blocks without SyncInfo
	return header.Number.Uint64()
}

// embedSyncInfoInHeader encodes minimal SyncInfo (HighQC/HighTC) into header.Extra pre-seal
func (h *Hotstuff) embedSyncInfoInHeader(header *types.Header) error {
	// Layout (append before seal, after validators):
	// [ .. extra .. | HSFLAG(1) | HQC_VIEW(8) | HQC_HASH(32) | HTC_VIEW(8) ]
	// HSFLAG=0xA5 marks presence. HTC_VIEW may be 0.
	log.Info("embedSyncInfoInHeader block number = ", header.Number)

	h.lock.Lock()
	st := h.getHsState()
	if st == nil {
		h.lock.Unlock()
		return errors.New("no hsState")
	}

	// Initialize highQC with parent only for bootstrap. After HotStuff has
	// activated, Seal requires a verified highQC before proposing.
	if st.highQC == nil && header.Number.Uint64() > 0 {
		parentView := header.Number.Uint64() - 1
		st.highQC = &HsQC{
			BlockHash:  header.ParentHash,
			View:       parentView,
			SignersSet: 0,
			Sig:        nil,
		}
		log.Warn("embedSyncInfoInHeader: initialized highQC with parent block (no QC yet)",
			"block", header.Number.Uint64(),
			"parentHash", header.ParentHash.Hex()[:10],
			"parentView", parentView)
	}

	// Copy values under lock, then release
	var hqcView uint64
	var hqcHash common.Hash
	var htcView uint64
	var hqcSignersSet types.ValidatorsBitSet
	var hqcSigLen int
	var hqcSig []byte
	if st.highQC != nil {
		hqcView = st.highQC.View
		hqcHash = st.highQC.BlockHash
		hqcSignersSet = st.highQC.SignersSet
		hqcSigLen = len(st.highQC.Sig)
		hqcSig = common.CopyBytes(st.highQC.Sig)
	}
	htcView = st.highTCView
	hasHighQC := st.highQC != nil

	log.Debug("embedSyncInfoInHeader: current highQC state",
		"block", header.Number.Uint64(),
		"hasHighQC", hasHighQC,
		"hqcView", hqcView,
		"hqcHash", hqcHash.Hex()[:8],
		"hqcSignersSet", hqcSignersSet,
		"hqcSigLen", hqcSigLen,
		"htcView", htcView)

	h.lock.Unlock()

	if !hasHighQC {
		// Only for genesis block (block 0) - no highQC needed
		return nil
	}
	size := syncInfoTotalSize
	flag := hsFlag
	if hasHighQCProof(hqcSignersSet, hqcSigLen) {
		size = syncInfoProofTotalSize
		flag = hsProofFlag
	}
	buf := make([]byte, 1+size)
	buf[0] = flag
	binary.LittleEndian.PutUint64(buf[1:1+viewSize], hqcView)
	copy(buf[1+viewSize:1+viewSize+hashSize], hqcHash[:])
	binary.LittleEndian.PutUint64(buf[1+viewSize+hashSize:1+syncInfoTotalSize], htcView)
	if flag == hsProofFlag {
		offset := 1 + syncInfoTotalSize
		binary.LittleEndian.PutUint32(buf[offset:offset+countSize], uint32(hqcSignersSet))
		offset += countSize
		copy(buf[offset:offset+types.BLSSignatureLength], hqcSig)
	}

	header.Extra = append(header.Extra, buf...)
	return nil
}

func hasHighQCProof(signersSet types.ValidatorsBitSet, sigLen int) bool {
	return signersSet != 0 && sigLen == types.BLSSignatureLength
}

// embedHighQC updates the pre-seal SyncInfo with provided HighQC
func (h *Hotstuff) embedHighQC(header *types.Header, hqcHash common.Hash, hqcView uint64) error {
	payload := header.Extra
	end := len(payload)
	if end >= extraSeal {
		end = end - extraSeal
	}
	hsSize := syncInfoSize(header)
	idx := len(payload) - extraSeal - hsSize
	if hsSize == 0 || idx < 0 {
		// no sync info yet, create minimal one
		buf := make([]byte, 1+syncInfoTotalSize)
		buf[0] = hsFlag
		binary.LittleEndian.PutUint64(buf[1:1+viewSize], hqcView)
		copy(buf[1+viewSize:1+viewSize+hashSize], hqcHash[:])
		header.Extra = append(header.Extra, buf...)
		return nil
	}

	// bounds check for existing block
	if idx >= end || idx+1+syncInfoTotalSize > end || (payload[idx] != hsFlag && payload[idx] != hsProofFlag) {
		return errors.New("malformed syncInfo in header.Extra")
	}
	// update HighQC fields (view+hash) in place
	binary.LittleEndian.PutUint64(header.Extra[idx+1:idx+1+viewSize], hqcView)
	copy(header.Extra[idx+1+viewSize:idx+1+viewSize+hashSize], hqcHash[:])
	return nil
}

// embedHighTC updates the pre-seal SyncInfo with provided HighTC view
func (h *Hotstuff) embedHighTC(header *types.Header, tcView uint64) error {
	payload := header.Extra
	end := len(payload)
	if end >= extraSeal {
		end = end - extraSeal
	}
	for i := end - 1; i >= 0; i-- {
		if payload[i] == hsFlag || payload[i] == hsProofFlag {
			// bounds check
			if i+1+syncInfoTotalSize > end {
				return errors.New("malformed syncInfo in header.Extra")
			}
			// write htcView field (last 8 bytes of syncInfo)
			offset := i + 1 + viewSize + hashSize
			binary.LittleEndian.PutUint64(header.Extra[offset:offset+viewSize], tcView)
			return nil
		}
	}
	// if syncInfo does not exist yet, create it
	buf := make([]byte, 1+syncInfoTotalSize)
	buf[0] = hsFlag
	binary.LittleEndian.PutUint64(buf[1+viewSize+hashSize:1+syncInfoTotalSize], tcView)
	header.Extra = append(header.Extra, buf...)
	return nil
}

// QuorumSize returns the minimum number of validators required for a quorum (2/3 + 1)
func QuorumSize(validatorCount int) int {
	return validatorCount*2/3 + 1
}
