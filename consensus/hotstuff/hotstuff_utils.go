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
	// CRITICAL FIX: Don't scan for hsFlag! It can appear in validator BLS pubkeys!
	// Instead, calculate the exact position where syncInfo should be.
	//
	// header.Extra layout:
	// [extraVanity(32)] | [validatorNum(1)] | [validators(num*validatorBytesLength)] |
	// [turnLength(1, optional)] | [syncInfo(49)] | [extraSeal(65)]

	payload := header.Extra
	if len(payload) < extraVanity+extraSeal {
		return false, 0, common.Hash{}, 0
	}

	if syncInfoSize(header) == 0 {
		return false, 0, common.Hash{}, 0
	}

	// Calculate where syncInfo should start (right before extraSeal).
	syncInfoStart := len(payload) - extraSeal - (1 + syncInfoTotalSize)

	// Parse syncInfo from the exact position
	hqcView = binary.LittleEndian.Uint64(payload[syncInfoStart+1 : syncInfoStart+1+viewSize])
	copy(hqcHash[:], payload[syncInfoStart+1+viewSize:syncInfoStart+1+viewSize+hashSize])
	htcView = binary.LittleEndian.Uint64(payload[syncInfoStart+1+viewSize+hashSize : syncInfoStart+1+syncInfoTotalSize])

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
		return false, 0, common.Hash{}, 0
	}

	return true, hqcView, hqcHash, htcView
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

	// Initialize highQC with genesis/parent block if not already set
	if st.highQC == nil && header.Number.Uint64() > 0 {
		// For block 1+, use parent as initial highQC
		// View equals parent block number for initial blocks
		parentView := header.Number.Uint64() - 1
		st.highQC = &HsQC{
			BlockHash:  header.ParentHash,
			View:       parentView,
			SignersSet: 0,
			Sig:        nil,
		}
		st.qcsByView[parentView] = st.highQC
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
	if st.highQC != nil {
		hqcView = st.highQC.View
		hqcHash = st.highQC.BlockHash
		hqcSignersSet = st.highQC.SignersSet
		hqcSigLen = len(st.highQC.Sig)
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
	const hsFlag byte = 0xA5
	buf := make([]byte, 1+syncInfoTotalSize)
	buf[0] = hsFlag
	binary.LittleEndian.PutUint64(buf[1:1+viewSize], hqcView)
	copy(buf[1+viewSize:1+viewSize+hashSize], hqcHash[:])
	binary.LittleEndian.PutUint64(buf[1+viewSize+hashSize:], htcView)

	header.Extra = append(header.Extra, buf...)
	return nil
}

// embedHighQC updates the pre-seal SyncInfo with provided HighQC
func (h *Hotstuff) embedHighQC(header *types.Header, hqcHash common.Hash, hqcView uint64) error {
	// try to locate HSFLAG backwards; if not found, append a fresh block
	const (
		hsFlag            byte = 0xA5
		viewSize               = 8                              // uint64 size in bytes
		hashSize               = 32                             // common.Hash size in bytes
		syncInfoTotalSize      = viewSize + hashSize + viewSize // hqcView + hqcHash + htcView
	)
	payload := header.Extra
	end := len(payload)
	if end >= extraSeal {
		end = end - extraSeal
	}
	idx := -1
	for i := end - 1; i >= 0; i-- {
		if payload[i] == hsFlag {
			idx = i
			break
		}
	}
	if idx < 0 {
		// no sync info yet, create minimal one
		buf := make([]byte, 1+syncInfoTotalSize)
		buf[0] = hsFlag
		binary.LittleEndian.PutUint64(buf[1:1+viewSize], hqcView)
		copy(buf[1+viewSize:1+viewSize+hashSize], hqcHash[:])
		header.Extra = append(header.Extra, buf...)
		return nil
	}

	// bounds check for existing block
	if idx+1+syncInfoTotalSize > end {
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
		if payload[i] == hsFlag {
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
