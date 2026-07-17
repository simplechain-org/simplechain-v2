package hotstuff

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

func TestHotstuffWALPreventsSameViewDoubleVoteAfterRestart(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	genesis := common.HexToHash("0x1234")
	firstHash := common.HexToHash("0x1111")
	conflictingHash := common.HexToHash("0x2222")

	first := &Hotstuff{db: db, genesisHash: genesis}
	first.initHsState()
	first._hs.currentView = 9
	if err := first.prepareVoteLocked(9, firstHash); err != nil {
		t.Fatalf("persist first vote: %v", err)
	}

	restarted := &Hotstuff{db: db, genesisHash: genesis}
	restarted.initHsState()
	if err := restarted.loadHsWAL(); err != nil {
		t.Fatalf("load WAL: %v", err)
	}
	if !restarted._hs.hasLastVote || restarted._hs.lastVotedView != 9 || restarted._hs.lastVotedHash != firstHash {
		t.Fatalf("vote intent was not restored: %#v", restarted._hs)
	}
	if err := restarted.prepareVoteLocked(9, conflictingHash); !errors.Is(err, errHotstuffDoubleVote) {
		t.Fatalf("conflicting same-view vote was not rejected: %v", err)
	}
	if err := restarted.prepareVoteLocked(9, firstHash); err != nil {
		t.Fatalf("idempotent same-view vote was rejected: %v", err)
	}
}

func TestHotstuffWALPreservesQCLockProofs(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	h := &Hotstuff{db: db, genesisHash: common.HexToHash("0xabcd")}
	h.initHsState()
	h._hs.highQC = &HsQC{BlockHash: common.HexToHash("0x10"), View: 10, SignersSet: 1 << 40, Sig: make([]byte, 96)}
	h._hs.lockedQC = &HsQC{BlockHash: common.HexToHash("0x09"), View: 9, SignersSet: 7, Sig: make([]byte, 96)}
	if err := h.persistHsWALLocked(); err != nil {
		t.Fatalf("persist WAL: %v", err)
	}

	restarted := &Hotstuff{db: db, genesisHash: h.genesisHash}
	restarted.initHsState()
	if err := restarted.loadHsWAL(); err != nil {
		t.Fatalf("load WAL: %v", err)
	}
	if restarted._hs.highQC == nil || restarted._hs.highQC.SignersSet != 1<<40 || len(restarted._hs.highQC.Sig) != 96 {
		t.Fatalf("highQC proof was not restored: %#v", restarted._hs.highQC)
	}
	if restarted._hs.lockedQC == nil || restarted._hs.lockedQC.BlockHash != h._hs.lockedQC.BlockHash {
		t.Fatalf("lockedQC was not restored: %#v", restarted._hs.lockedQC)
	}
}
