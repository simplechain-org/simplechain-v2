package hotstuff

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

const hsWALVersion = uint64(1)

var errHotstuffDoubleVote = errors.New("refusing conflicting HotStuff vote")

type hsWALQC struct {
	BlockHash  common.Hash
	View       uint64
	SignersSet types.ValidatorsBitSet
	Sig        []byte
}

type hsWALRecord struct {
	Version       uint64
	GenesisHash   common.Hash
	CurrentView   uint64
	HighTCView    uint64
	HasLastVote   bool
	LastVotedView uint64
	LastVotedHash common.Hash
	LockedQC      *hsWALQC
	HighQC        *hsWALQC
}

func (h *Hotstuff) hsWALKey() []byte {
	return append([]byte("hotstuff-consensus-wal-v1/"), h.genesisHash[:]...)
}

func qcToWAL(qc *HsQC) *hsWALQC {
	if qc == nil {
		return nil
	}
	return &hsWALQC{
		BlockHash: qc.BlockHash, View: qc.View, SignersSet: qc.SignersSet,
		Sig: common.CopyBytes(qc.Sig),
	}
}

func qcFromWAL(qc *hsWALQC) *HsQC {
	if qc == nil {
		return nil
	}
	return &HsQC{
		BlockHash: qc.BlockHash, View: qc.View, SignersSet: qc.SignersSet,
		Sig: common.CopyBytes(qc.Sig),
	}
}

// persistHsWALLocked durably records all safety-critical state. Callers must
// hold h.lock for writing; voting must not happen unless this returns nil.
func (h *Hotstuff) persistHsWALLocked() error {
	if h.db == nil {
		return errors.New("HotStuff WAL database is not initialized")
	}
	st := h.getHsState()
	if st == nil {
		return errors.New("HotStuff state is not initialized")
	}
	record := hsWALRecord{
		Version:     hsWALVersion,
		GenesisHash: h.genesisHash,
		CurrentView: st.currentView,
		// A TC cannot be recovered from its view alone, so it is intentionally
		// rebuilt from network messages after restart.
		HighTCView:    0,
		HasLastVote:   st.hasLastVote,
		LastVotedView: st.lastVotedView,
		LastVotedHash: st.lastVotedHash,
		LockedQC:      qcToWAL(st.lockedQC),
		HighQC:        qcToWAL(st.highQC),
	}
	encoded, err := rlp.EncodeToBytes(&record)
	if err != nil {
		return fmt.Errorf("encode HotStuff WAL: %w", err)
	}
	if err := h.db.Put(h.hsWALKey(), encoded); err != nil {
		return fmt.Errorf("write HotStuff WAL: %w", err)
	}
	if err := h.db.SyncKeyValue(); err != nil {
		return fmt.Errorf("sync HotStuff WAL: %w", err)
	}
	return nil
}

func (h *Hotstuff) loadHsWAL() error {
	if h.db == nil {
		return errors.New("HotStuff WAL database is not initialized")
	}
	key := h.hsWALKey()
	exists, err := h.db.Has(key)
	if err != nil {
		return fmt.Errorf("check HotStuff WAL: %w", err)
	}
	if !exists {
		return nil
	}
	encoded, err := h.db.Get(key)
	if err != nil {
		return fmt.Errorf("read HotStuff WAL: %w", err)
	}
	var record hsWALRecord
	if err := rlp.DecodeBytes(encoded, &record); err != nil {
		return fmt.Errorf("decode HotStuff WAL: %w", err)
	}
	if record.Version != hsWALVersion || record.GenesisHash != h.genesisHash {
		return errors.New("HotStuff WAL belongs to an incompatible chain or version")
	}
	st := h.getHsState()
	if st == nil {
		return errors.New("HotStuff state is not initialized")
	}
	st.currentView = record.CurrentView
	st.highTCView = 0
	st.hasLastVote = record.HasLastVote
	st.lastVotedView = record.LastVotedView
	st.lastVotedHash = record.LastVotedHash
	st.lockedQC = qcFromWAL(record.LockedQC)
	st.highQC = qcFromWAL(record.HighQC)
	if st.lockedQC != nil {
		st.qcsByView[st.lockedQC.View] = st.lockedQC
	}
	if st.highQC != nil {
		st.qcsByView[st.highQC.View] = st.highQC
	}
	return nil
}

func (h *Hotstuff) prepareVoteLocked(view uint64, hash common.Hash) error {
	if h.hsWALError != nil {
		return h.hsWALError
	}
	st := h.getHsState()
	if st == nil {
		return errors.New("HotStuff state is not initialized")
	}
	if st.hasLastVote {
		if view < st.lastVotedView || (view == st.lastVotedView && hash != st.lastVotedHash) {
			return fmt.Errorf("%w: persisted view=%d hash=%s requested view=%d hash=%s",
				errHotstuffDoubleVote, st.lastVotedView, st.lastVotedHash, view, hash)
		}
	}
	if !st.hasLastVote || view > st.lastVotedView {
		st.hasLastVote = true
		st.lastVotedView = view
		st.lastVotedHash = hash
	}
	if err := h.persistHsWALLocked(); err != nil {
		h.hsWALError = err
		return err
	}
	return nil
}

func (h *Hotstuff) checkpointHsStateLocked() error {
	if h.hsWALError != nil {
		return h.hsWALError
	}
	if err := h.persistHsWALLocked(); err != nil {
		h.hsWALError = err
		return err
	}
	return nil
}
