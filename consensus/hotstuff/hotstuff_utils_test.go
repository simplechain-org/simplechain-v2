package hotstuff

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

func testHotstuffConfig() *params.ChainConfig {
	return &params.ChainConfig{HotstuffBlock: big.NewInt(0), Hotstuff: &params.HotstuffConfig{}}
}

func TestParseSyncInfoWithProof(t *testing.T) {
	config := testHotstuffConfig()
	header := &types.Header{
		Number: big.NewInt(100),
		Extra:  make([]byte, extraVanity),
	}
	hash := common.HexToHash("0x1234")
	sig := make([]byte, types.BLSSignatureLength)
	for i := range sig {
		sig[i] = byte(i + 1)
	}

	buf := make([]byte, 1+syncInfoProofTotalSize)
	buf[0] = hsProofFlag
	binary.LittleEndian.PutUint64(buf[1:1+viewSize], 9)
	copy(buf[1+viewSize:1+viewSize+hashSize], hash[:])
	binary.LittleEndian.PutUint64(buf[1+viewSize+hashSize:1+syncInfoTotalSize], 11)
	offset := 1 + syncInfoTotalSize
	wantSigners := types.ValidatorsBitSet(1<<40 | 7)
	binary.LittleEndian.PutUint64(buf[offset:offset+countSize], uint64(wantSigners))
	offset += countSize
	copy(buf[offset:offset+types.BLSSignatureLength], sig)

	header.Extra = append(header.Extra, buf...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	has, hqcView, hqcHash, htcView, signersSet, gotSig, hasProof := parseSyncInfoWithProof(header, config)
	if !has || !hasProof {
		t.Fatalf("expected sync info with proof, has=%v hasProof=%v", has, hasProof)
	}
	if hqcView != 9 || hqcHash != hash || htcView != 11 {
		t.Fatalf("unexpected sync info: view=%d hash=%s htc=%d", hqcView, hqcHash, htcView)
	}
	if signersSet != wantSigners {
		t.Fatalf("unexpected signers set: have %d want %d", signersSet, wantSigners)
	}
	if len(gotSig) != types.BLSSignatureLength {
		t.Fatalf("unexpected signature length: %d", len(gotSig))
	}
	if view := getViewFromHeader(header, config); view != 12 {
		t.Fatalf("unexpected header view: have %d want 12", view)
	}
}

func TestParseSyncInfoLegacy(t *testing.T) {
	config := testHotstuffConfig()
	header := &types.Header{
		Number: big.NewInt(100),
		Extra:  make([]byte, extraVanity),
	}
	hash := common.HexToHash("0xabcd")

	buf := make([]byte, 1+syncInfoTotalSize)
	buf[0] = hsFlag
	binary.LittleEndian.PutUint64(buf[1:1+viewSize], 8)
	copy(buf[1+viewSize:1+viewSize+hashSize], hash[:])
	binary.LittleEndian.PutUint64(buf[1+viewSize+hashSize:1+syncInfoTotalSize], 0)

	header.Extra = append(header.Extra, buf...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	has, hqcView, hqcHash, htcView, _, _, hasProof := parseSyncInfoWithProof(header, config)
	if !has || hasProof {
		t.Fatalf("expected legacy sync info without proof, has=%v hasProof=%v", has, hasProof)
	}
	if hqcView != 8 || hqcHash != hash || htcView != 0 {
		t.Fatalf("unexpected sync info: view=%d hash=%s htc=%d", hqcView, hqcHash, htcView)
	}
	if view := getViewFromHeader(header, config); view != 9 {
		t.Fatalf("unexpected header view: have %d want 9", view)
	}
}

func TestTimeoutCertAndSyncInfoTrailerRoundTrip(t *testing.T) {
	config := testHotstuffConfig()
	header := &types.Header{Number: big.NewInt(100), Extra: make([]byte, extraVanity)}
	parentHash := common.HexToHash("0xbeef")
	syncInfo := make([]byte, 1+syncInfoProofTotalSize)
	syncInfo[0] = hsProofFlag
	binary.LittleEndian.PutUint64(syncInfo[1:1+viewSize], 9)
	copy(syncInfo[1+viewSize:1+viewSize+hashSize], parentHash[:])
	binary.LittleEndian.PutUint64(syncInfo[1+viewSize+hashSize:1+syncInfoTotalSize], 11)
	offset := 1 + syncInfoTotalSize
	binary.LittleEndian.PutUint64(syncInfo[offset:offset+countSize], uint64(types.ValidatorsBitSet(1<<40|3)))
	for i := 0; i < types.BLSSignatureLength; i++ {
		syncInfo[offset+countSize+i] = byte(i + 1)
	}
	header.Extra = append(header.Extra, syncInfo...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	tc := &hsTimeoutCert{
		View: 11,
		HighQC: &HsQC{
			BlockHash: parentHash, View: 9, SignersSet: types.ValidatorsBitSet(1<<40 | 3),
			Sig: append([]byte(nil), syncInfo[offset+countSize:offset+countSize+types.BLSSignatureLength]...),
		},
		SignerSet: types.ValidatorsBitSet(1<<41 | 5),
	}
	for i := range tc.AggSig {
		tc.AggSig[i] = byte(255 - i)
	}
	h := &Hotstuff{chainConfig: config}
	if err := h.embedTimeoutCert(header, tc); err != nil {
		t.Fatalf("embed TimeoutCert: %v", err)
	}

	parsed, err := h.parseTimeoutCert(header)
	if err != nil {
		t.Fatalf("parse TimeoutCert: %v", err)
	}
	if parsed == nil || parsed.View != tc.View || parsed.SignerSet != tc.SignerSet ||
		parsed.HighQC == nil || parsed.HighQC.SignersSet != tc.HighQC.SignersSet {
		t.Fatalf("unexpected TimeoutCert round trip: %#v", parsed)
	}
	has, qcView, qcHash, tcView, signers, _, proof := parseSyncInfoWithProof(header, config)
	if !has || !proof || qcView != 9 || qcHash != parentHash || tcView != 11 || signers != tc.HighQC.SignersSet {
		t.Fatalf("SyncInfo changed after TC insertion: has=%v proof=%v qc=%d hash=%s tc=%d signers=%d", has, proof, qcView, qcHash, tcView, signers)
	}
	if got := hotstuffExtraSize(header, config); got != (1+syncInfoProofTotalSize)+timeoutCertSize(header, config) {
		t.Fatalf("unexpected HotStuff trailer size: %d", got)
	}
}

func TestSyncInfoIgnoredBeforeHotstuffFork(t *testing.T) {
	config := &params.ChainConfig{HotstuffBlock: big.NewInt(101), Hotstuff: &params.HotstuffConfig{}}
	header := &types.Header{Number: big.NewInt(100), Extra: make([]byte, extraVanity)}
	trailer := make([]byte, 1+syncInfoTotalSize)
	trailer[0] = hsFlag
	binary.LittleEndian.PutUint64(trailer[1:1+viewSize], 8)
	header.Extra = append(header.Extra, trailer...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	if has, _, _, _, _, _, _ := parseSyncInfoWithProof(header, config); has {
		t.Fatal("pre-fork Parlia extra-data was parsed as HotStuff SyncInfo")
	}
	if size := hotstuffExtraSize(header, config); size != 0 {
		t.Fatalf("pre-fork HotStuff trailer size = %d, want 0", size)
	}
	if view := getViewFromHeader(header, config); view != header.Number.Uint64() {
		t.Fatalf("pre-fork view = %d, want block number %d", view, header.Number.Uint64())
	}
}

func TestSnapshotIgnoresAttestationWithoutVoteData(t *testing.T) {
	config := &params.ChainConfig{
		LubanBlock:    big.NewInt(0),
		Hotstuff:      &params.HotstuffConfig{},
		HotstuffBlock: big.NewInt(10),
	}
	encoded, err := rlp.EncodeToBytes(&types.VoteAttestation{})
	if err != nil {
		t.Fatal(err)
	}
	header := &types.Header{Number: big.NewInt(1), Extra: make([]byte, extraVanity)}
	header.Extra = append(header.Extra, encoded...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)
	snapshot := &Snapshot{EpochLength: defaultEpochLength}

	snapshot.updateAttestation(header, config)
	if snapshot.Attestation != nil {
		t.Fatalf("malformed attestation updated snapshot: %#v", snapshot.Attestation)
	}
}

func TestViewTooOldDoesNotOverflow(t *testing.T) {
	if viewTooOld(math.MaxUint64, 10, 2) {
		t.Fatal("future maximum view was treated as stale through uint64 overflow")
	}
	if !viewTooOld(7, 10, 2) {
		t.Fatal("view outside tolerance was accepted")
	}
	if viewTooOld(8, 10, 2) {
		t.Fatal("view at tolerance boundary was rejected")
	}
}
