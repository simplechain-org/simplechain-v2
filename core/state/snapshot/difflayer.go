// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package snapshot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	bloomfilter "github.com/holiman/bloomfilter/v2"
)

// getGoroutineID returns the ID of the current goroutine for debugging
func getGoroutineID() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	// Stack trace format: "goroutine 123 [running]:"
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	idx := bytes.IndexByte(b, ' ')
	if idx == -1 {
		return 0
	}
	b = b[:idx]
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

// shouldBlockOnStaleConflict returns true if we should block instead of panic on stale conflict
func shouldBlockOnStaleConflict() bool {
	return os.Getenv("DIFFLAYER_BLOCK_ON_STALE") == "true" || os.Getenv("DIFFLAYER_BLOCK_ON_STALE") == "1"
}

// getBlockDuration returns the duration to block when stale conflict is detected
func getBlockDuration() time.Duration {
	durationStr := os.Getenv("DIFFLAYER_BLOCK_DURATION")
	if durationStr == "" {
		return 60 * time.Second // Default: block for 60 seconds
	}
	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		log.Warn("Invalid DIFFLAYER_BLOCK_DURATION, using default 60s", "error", err, "value", durationStr)
		return 60 * time.Second
	}
	return duration
}

// shouldPanicAfterBlock returns true if we should panic after blocking
func shouldPanicAfterBlock() bool {
	return os.Getenv("DIFFLAYER_PANIC_AFTER_BLOCK") == "true" || os.Getenv("DIFFLAYER_PANIC_AFTER_BLOCK") == "1"
}

// blockOnStaleConflict blocks for a configurable duration when stale conflict is detected,
// allowing observation of all goroutines via pprof or other tools
func blockOnStaleConflict(childRoot, parentRoot common.Hash, currentGoroutineID uint64, blockDuration time.Duration) {
	log.Error("=" + strings.Repeat("=", 100))
	log.Error("DIFFLAYER STALE CONFLICT DETECTED - ENTERING BLOCK MODE FOR DIAGNOSIS")
	log.Error("=" + strings.Repeat("=", 100))
	log.Error("Conflict Details",
		"childRoot", childRoot.Hex()[:8],
		"parentRoot", parentRoot.Hex()[:8],
		"currentGoroutine", currentGoroutineID,
		"blockDuration", blockDuration,
		"timestamp", time.Now().Format(time.RFC3339Nano))

	// Print current goroutine stack
	buf := make([]byte, 1024*64) // 64KB buffer
	n := runtime.Stack(buf, false)
	log.Error("Current Goroutine Stack", "goroutine", currentGoroutineID, "stack", string(buf[:n]))

	// Print all goroutines periodically during block
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	startTime := time.Now()
	iteration := 0

	log.Error("BLOCKING - Use pprof or other tools to observe all goroutines")
	log.Error("Set DIFFLAYER_BLOCK_DURATION to change block duration (default: 60s)")
	log.Error("Set DIFFLAYER_PANIC_AFTER_BLOCK=true to panic after blocking")

	for {
		select {
		case <-ticker.C:
			iteration++
			elapsed := time.Since(startTime)
			log.Error("Still blocking...",
				"iteration", iteration,
				"elapsed", elapsed,
				"remaining", blockDuration-elapsed,
				"goroutine", currentGoroutineID)

			// Print all goroutines every 15 seconds (every 3rd tick)
			if iteration%3 == 0 {
				buf := make([]byte, 1024*1024) // 1MB buffer for all goroutines
				n := runtime.Stack(buf, true)
				log.Error("All Goroutines Stack Dump", "size", n, "stack", string(buf[:n]))
			}

		case <-time.After(blockDuration):
			log.Error("=" + strings.Repeat("=", 100))
			log.Error("BLOCK MODE COMPLETE - Exiting block")
			log.Error("=" + strings.Repeat("=", 100))
			return
		}
	}
}

var (
	// aggregatorMemoryLimit is the maximum size of the bottom-most diff layer
	// that aggregates the writes from above until it's flushed into the disk
	// layer.
	//
	// Note, bumping this up might drastically increase the size of the bloom
	// filters that's stored in every diff layer. Don't do that without fully
	// understanding all the implications.
	aggregatorMemoryLimit = uint64(4 * 1024 * 1024)

	// aggregatorItemLimit is an approximate number of items that will end up
	// in the aggregator layer before it's flushed out to disk. A plain account
	// weighs around 14B (+hash), a storage slot 32B (+hash), a deleted slot
	// 0B (+hash). Slots are mostly set/unset in lockstep, so that average at
	// 16B (+hash). All in all, the average entry seems to be 15+32=47B. Use a
	// smaller number to be on the safe side.
	aggregatorItemLimit = aggregatorMemoryLimit / 42

	// bloomTargetError is the target false positive rate when the aggregator
	// layer is at its fullest. The actual value will probably move around up
	// and down from this number, it's mostly a ballpark figure.
	//
	// Note, dropping this down might drastically increase the size of the bloom
	// filters that's stored in every diff layer. Don't do that without fully
	// understanding all the implications.
	bloomTargetError = 0.02

	// bloomSize is the ideal bloom filter size given the maximum number of items
	// it's expected to hold and the target false positive error rate.
	bloomSize = math.Ceil(float64(aggregatorItemLimit) * math.Log(bloomTargetError) / math.Log(1/math.Pow(2, math.Log(2))))

	// bloomFuncs is the ideal number of bits a single entry should set in the
	// bloom filter to keep its size to a minimum (given it's size and maximum
	// entry count).
	bloomFuncs = math.Round((bloomSize / float64(aggregatorItemLimit)) * math.Log(2))

	// the bloom offsets are runtime constants which determines which part of the
	// account/storage hash the hasher functions looks at, to determine the
	// bloom key for an account/slot. This is randomized at init(), so that the
	// global population of nodes do not all display the exact same behaviour with
	// regards to bloom content
	bloomAccountHasherOffset = 0
	bloomStorageHasherOffset = 0
)

func init() {
	// Init the bloom offsets in the range [0:24] (requires 8 bytes)
	bloomAccountHasherOffset = rand.Intn(25)
	bloomStorageHasherOffset = rand.Intn(25)
}

// diffLayer represents a collection of modifications made to a state snapshot
// after running a block on top. It contains one sorted list for the account trie
// and one-one list for each storage tries.
//
// The goal of a diff layer is to act as a journal, tracking recent modifications
// made to the state, that have not yet graduated into a semi-immutable state.
type diffLayer struct {
	origin *diskLayer // Base disk layer to directly use on bloom misses
	parent snapshot   // Parent snapshot modified by this one, never nil
	memory uint64     // Approximate guess as to how much memory we use

	root  common.Hash // Root hash to which this snapshot diff belongs to
	stale atomic.Bool // Signals that the layer became stale (state progressed)

	accountData map[common.Hash][]byte                 // Keyed accounts for direct retrieval (nil means deleted)
	storageData map[common.Hash]map[common.Hash][]byte // Keyed storage slots for direct retrieval. one per account (nil means deleted)
	accountList []common.Hash                          // List of account for iteration. If it exists, it's sorted, otherwise it's nil
	storageList map[common.Hash][]common.Hash          // List of storage slots for iterated retrievals, one per account. Any existing lists are sorted if non-nil

	diffed *bloomfilter.Filter // Bloom filter tracking all the diffed items up to the disk layer

	lock sync.RWMutex
}

// accountBloomHash is used to convert an account hash into a 64 bit mini hash.
func accountBloomHash(h common.Hash) uint64 {
	return binary.BigEndian.Uint64(h[bloomAccountHasherOffset : bloomAccountHasherOffset+8])
}

// storageBloomHash is used to convert an account hash and a storage hash into a 64 bit mini hash.
func storageBloomHash(h0, h1 common.Hash) uint64 {
	return binary.BigEndian.Uint64(h0[bloomStorageHasherOffset:bloomStorageHasherOffset+8]) ^
		binary.BigEndian.Uint64(h1[bloomStorageHasherOffset:bloomStorageHasherOffset+8])
}

// newDiffLayer creates a new diff on top of an existing snapshot, whether that's a low
// level persistent database or a hierarchical diff already.
func newDiffLayer(parent snapshot, root common.Hash, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer {
	// Create the new layer with some pre-allocated data segments
	dl := &diffLayer{
		parent:      parent,
		root:        root,
		accountData: accounts,
		storageData: storage,
		storageList: make(map[common.Hash][]common.Hash),
	}

	switch parent := parent.(type) {
	case *diskLayer:
		dl.rebloom(parent)
	case *diffLayer:
		dl.rebloom(parent.origin)
	default:
		panic("unknown parent type")
	}

	// Sanity check that accounts or storage slots are never nil
	for _, blob := range accounts {
		// Determine memory size and track the dirty writes
		dl.memory += uint64(common.HashLength + len(blob))
		snapshotDirtyAccountWriteMeter.Mark(int64(len(blob)))
	}
	for accountHash, slots := range storage {
		if slots == nil {
			panic(fmt.Sprintf("storage %#x nil", accountHash))
		}
		// Determine memory size and track the dirty writes
		for _, data := range slots {
			dl.memory += uint64(common.HashLength + len(data))
			snapshotDirtyStorageWriteMeter.Mark(int64(len(data)))
		}
	}
	return dl
}

// rebloom discards the layer's current bloom and rebuilds it from scratch based
// on the parent's and the local diffs.
func (dl *diffLayer) rebloom(origin *diskLayer) {
	dl.lock.Lock()
	defer dl.lock.Unlock()

	defer func(start time.Time) {
		snapshotBloomIndexTimer.Update(time.Since(start))
	}(time.Now())

	// Inject the new origin that triggered the rebloom
	dl.origin = origin

	// Retrieve the parent bloom or create a fresh empty one
	if parent, ok := dl.parent.(*diffLayer); ok {
		parent.lock.RLock()
		dl.diffed, _ = parent.diffed.Copy()
		parent.lock.RUnlock()
	} else {
		dl.diffed, _ = bloomfilter.New(uint64(bloomSize), uint64(bloomFuncs))
	}
	for hash := range dl.accountData {
		dl.diffed.AddHash(accountBloomHash(hash))
	}
	for accountHash, slots := range dl.storageData {
		for storageHash := range slots {
			dl.diffed.AddHash(storageBloomHash(accountHash, storageHash))
		}
	}
	// Calculate the current false positive rate and update the error rate meter.
	// This is a bit cheating because subsequent layers will overwrite it, but it
	// should be fine, we're only interested in ballpark figures.
	k := float64(dl.diffed.K())
	n := float64(dl.diffed.N())
	m := float64(dl.diffed.M())
	snapshotBloomErrorGauge.Update(math.Pow(1.0-math.Exp((-k)*(n+0.5)/(m-1)), k))
}

// Root returns the root hash for which this snapshot was made.
func (dl *diffLayer) Root() common.Hash {
	return dl.root
}

// Parent returns the subsequent layer of a diff layer.
func (dl *diffLayer) Parent() snapshot {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	return dl.parent
}

// Stale return whether this layer has become stale (was flattened across) or if
// it's still live.
func (dl *diffLayer) Stale() bool {
	return dl.stale.Load()
}

// Account directly retrieves the account associated with a particular hash in
// the snapshot slim data format.
func (dl *diffLayer) Account(hash common.Hash) (*types.SlimAccount, error) {
	data, err := dl.AccountRLP(hash)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 { // can be both nil and []byte{}
		return nil, nil
	}
	account := new(types.SlimAccount)
	if err := rlp.DecodeBytes(data, account); err != nil {
		panic(err)
	}
	return account, nil
}

// Accounts directly retrieves all accounts in current snapshot in
// the snapshot slim data format.
func (dl *diffLayer) Accounts() (map[common.Hash]*types.SlimAccount, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	accounts := make(map[common.Hash]*types.SlimAccount, len(dl.accountData))
	for hash, data := range dl.accountData {
		account := new(types.SlimAccount)
		if err := rlp.DecodeBytes(data, account); err != nil {
			return nil, err
		}
		accounts[hash] = account
	}

	return accounts, nil
}

// AccountRLP directly retrieves the account RLP associated with a particular
// hash in the snapshot slim data format.
//
// Note the returned account is not a copy, please don't modify it.
func (dl *diffLayer) AccountRLP(hash common.Hash) ([]byte, error) {
	// Check staleness before reaching further.
	dl.lock.RLock()
	if dl.Stale() {
		dl.lock.RUnlock()
		return nil, ErrSnapshotStale
	}
	// Check the bloom filter first whether there's even a point in reaching into
	// all the maps in all the layers below
	var origin *diskLayer
	hit := dl.diffed.ContainsHash(accountBloomHash(hash))
	if !hit {
		origin = dl.origin // extract origin while holding the lock
	}
	dl.lock.RUnlock()

	// If the bloom filter misses, don't even bother with traversing the memory
	// diff layers, reach straight into the bottom persistent disk layer
	if origin != nil {
		snapshotBloomAccountMissMeter.Mark(1)
		return origin.AccountRLP(hash)
	}
	// The bloom filter hit, start poking in the internal maps
	return dl.accountRLP(hash, 0)
}

// accountRLP is an internal version of AccountRLP that skips the bloom filter
// checks and uses the internal maps to try and retrieve the data. It's meant
// to be used if a higher layer's bloom filter hit already.
func (dl *diffLayer) accountRLP(hash common.Hash, depth int) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	if dl.Stale() {
		return nil, ErrSnapshotStale
	}
	// If the account is known locally, return it
	if data, ok := dl.accountData[hash]; ok {
		snapshotDirtyAccountHitMeter.Mark(1)
		snapshotDirtyAccountHitDepthHist.Update(int64(depth))
		if n := len(data); n > 0 {
			snapshotDirtyAccountReadMeter.Mark(int64(n))
		} else {
			snapshotDirtyAccountInexMeter.Mark(1)
		}
		snapshotBloomAccountTrueHitMeter.Mark(1)
		return data, nil
	}
	// Account unknown to this diff, resolve from parent
	if diff, ok := dl.parent.(*diffLayer); ok {
		return diff.accountRLP(hash, depth+1)
	}
	// Failed to resolve through diff layers, mark a bloom error and use the disk
	snapshotBloomAccountFalseHitMeter.Mark(1)
	return dl.parent.AccountRLP(hash)
}

// Storage directly retrieves the storage data associated with a particular hash,
// within a particular account. If the slot is unknown to this diff, it's parent
// is consulted.
//
// Note the returned slot is not a copy, please don't modify it.
func (dl *diffLayer) Storage(accountHash, storageHash common.Hash) ([]byte, error) {
	// Check the bloom filter first whether there's even a point in reaching into
	// all the maps in all the layers below
	dl.lock.RLock()
	// Check staleness before reaching further.
	if dl.Stale() {
		dl.lock.RUnlock()
		return nil, ErrSnapshotStale
	}
	var origin *diskLayer
	hit := dl.diffed.ContainsHash(storageBloomHash(accountHash, storageHash))
	if !hit {
		origin = dl.origin // extract origin while holding the lock
	}
	dl.lock.RUnlock()

	// If the bloom filter misses, don't even bother with traversing the memory
	// diff layers, reach straight into the bottom persistent disk layer
	if origin != nil {
		snapshotBloomStorageMissMeter.Mark(1)
		return origin.Storage(accountHash, storageHash)
	}
	// The bloom filter hit, start poking in the internal maps
	return dl.storage(accountHash, storageHash, 0)
}

// storage is an internal version of Storage that skips the bloom filter checks
// and uses the internal maps to try and retrieve the data. It's meant  to be
// used if a higher layer's bloom filter hit already.
func (dl *diffLayer) storage(accountHash, storageHash common.Hash, depth int) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	if dl.Stale() {
		return nil, ErrSnapshotStale
	}
	// If the account is known locally, try to resolve the slot locally
	if storage, ok := dl.storageData[accountHash]; ok {
		if data, ok := storage[storageHash]; ok {
			snapshotDirtyStorageHitMeter.Mark(1)
			//snapshotDirtyStorageHitDepthHist.Update(int64(depth))
			if n := len(data); n > 0 {
				snapshotDirtyStorageReadMeter.Mark(int64(n))
			} else {
				snapshotDirtyStorageInexMeter.Mark(1)
			}
			snapshotBloomStorageTrueHitMeter.Mark(1)
			return data, nil
		}
	}
	// Storage slot unknown to this diff, resolve from parent
	if diff, ok := dl.parent.(*diffLayer); ok {
		return diff.storage(accountHash, storageHash, depth+1)
	}
	// Failed to resolve through diff layers, mark a bloom error and use the disk
	snapshotBloomStorageFalseHitMeter.Mark(1)
	return dl.parent.Storage(accountHash, storageHash)
}

// Update creates a new layer on top of the existing snapshot diff tree with
// the specified data items.
func (dl *diffLayer) Update(blockRoot common.Hash, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer {
	return newDiffLayer(dl, blockRoot, accounts, storage)
}

// flatten pushes all data from this point downwards, flattening everything into
// a single diff at the bottom. Since usually the lowermost diff is the largest,
// the flattening builds up from there in reverse.
func (dl *diffLayer) flatten() snapshot {
	log.Debug("[DiffLayer] flatten ENTER",
		"childRoot", dl.root.Hex()[:8],
		"childMemory", dl.memory,
		"goroutine", getGoroutineID())

	// If the parent is not diff, we're the first in line, return unmodified
	parent, ok := dl.parent.(*diffLayer)
	if !ok {
		log.Debug("[DiffLayer] flatten EXIT - parent is disk layer",
			"childRoot", dl.root.Hex()[:8])
		return dl
	}

	log.Info("[DiffLayer] flatten - parent is diff, recursing",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"parentStale", parent.stale.Load(),
		"goroutine", getGoroutineID())

	// Parent is a diff, flatten it first (note, apart from weird corned cases,
	// flatten will realistically only ever merge 1 layer, so there's no need to
	// be smarter about grouping flattens together).
	// CRITICAL: This recursive call happens WITHOUT holding any lock!
	// If two different Cap operations both reach the same parent through different paths,
	// they could both recursively flatten the same parent concurrently.
	// However, since Cap() holds t.lock, this should not happen in practice.
	// But we add a check here to detect if parent was already flattened by another goroutine.
	log.Info("[DiffLayer] flatten - calling parent.flatten() recursively (NO LOCK HELD)",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"parentStaleBeforeRecurse", parent.stale.Load(),
		"goroutine", getGoroutineID())

	// Check if parent was already flattened by another goroutine (should not happen with t.lock)
	// If parent is already stale, it means another goroutine has already flattened it.
	// In this case, we MUST NOT modify the parent, as it's being used by another goroutine.
	// Instead, we return dl as-is, without flattening into the parent.
	if parent.stale.Load() {
		log.Warn("[DiffLayer] Parent already stale before recursive flatten - another goroutine already flattened it",
			"childRoot", dl.root.Hex()[:8],
			"parentRoot", parent.root.Hex()[:8],
			"goroutine", getGoroutineID(),
			"NOTE", "Parent was already flattened by another goroutine, returning self without flattening")
		// Parent is already flattened and stale, we cannot modify it.
		// Return dl as-is, without flattening into the parent.
		// This is safe because the parent will be cleaned up by the other goroutine.
		return dl
	}

	// Parent is not stale, so we need to flatten it first
	parent = parent.flatten().(*diffLayer)

	log.Info("[DiffLayer] flatten - parent.flatten() returned",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"parentStaleAfterRecurse", parent.stale.Load(),
		"goroutine", getGoroutineID())

	log.Debug("[DiffLayer] flatten - acquiring parent lock",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"parentStale", parent.stale.Load())

	parent.lock.Lock()
	defer func() {
		parent.lock.Unlock()
		log.Debug("[DiffLayer] flatten - released parent lock",
			"childRoot", dl.root.Hex()[:8],
			"parentRoot", parent.root.Hex()[:8])
	}()

	log.Info("[DiffLayer] flatten - acquired parent lock, checking stale",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"parentStaleBeforeSwap", parent.stale.Load(),
		"goroutine", getGoroutineID())

	// Before actually writing all our data to the parent, first ensure that the
	// parent hasn't been 'corrupted' by someone else already flattening into it
	// CRITICAL: This check detects concurrent flatten operations on the same parent
	// If Swap returns true, it means another goroutine already marked this parent as stale
	if parent.stale.Swap(true) {
		currentGoroutineID := getGoroutineID()
		log.Warn("[DiffLayer] STALE CONFLICT DETECTED - parent was already stale after acquiring lock!",
			"childRoot", dl.root.Hex()[:8],
			"parentRoot", parent.root.Hex()[:8],
			"goroutine", currentGoroutineID,
			"NOTE", "Another goroutine flattened this parent between our stale check and lock acquisition")

		// This is a race condition: between our initial stale check (line 530) and acquiring
		// the lock (line 554), another goroutine flattened this parent.
		// We MUST NOT modify the parent data, as it's already been flattened.
		// Return dl as-is, without flattening into the parent.
		log.Warn("[DiffLayer] Returning self without flattening (race condition detected)",
			"childRoot", dl.root.Hex()[:8],
			"parentRoot", parent.root.Hex()[:8],
			"goroutine", currentGoroutineID)

		// Return dl as-is
		return dl
	}

	log.Debug("[DiffLayer] flatten - marked parent as stale, merging data",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"accountsToMerge", len(dl.accountData),
		"storageToMerge", len(dl.storageData))

	maps.Copy(parent.accountData, dl.accountData)
	// Overwrite all the updated storage slots (individually)
	for accountHash, storage := range dl.storageData {
		// If storage didn't exist (or was deleted) in the parent, overwrite blindly
		if _, ok := parent.storageData[accountHash]; !ok {
			parent.storageData[accountHash] = storage
			continue
		}
		// Storage exists in both parent and child, merge the slots
		maps.Copy(parent.storageData[accountHash], storage)
	}

	log.Debug("[DiffLayer] flatten EXIT - merge complete",
		"childRoot", dl.root.Hex()[:8],
		"parentRoot", parent.root.Hex()[:8],
		"resultRoot", dl.root.Hex()[:8])

	// Return the combo parent
	return &diffLayer{
		parent:      parent.parent,
		origin:      parent.origin,
		root:        dl.root,
		accountData: parent.accountData,
		storageData: parent.storageData,
		storageList: make(map[common.Hash][]common.Hash),
		diffed:      dl.diffed,
		memory:      parent.memory + dl.memory,
	}
}

// AccountList returns a sorted list of all accounts in this diffLayer, including
// the deleted ones.
//
// Note, the returned slice is not a copy, so do not modify it.
func (dl *diffLayer) AccountList() []common.Hash {
	// If an old list already exists, return it
	dl.lock.RLock()
	list := dl.accountList
	dl.lock.RUnlock()

	if list != nil {
		return list
	}
	// No old sorted account list exists, generate a new one
	dl.lock.Lock()
	defer dl.lock.Unlock()

	dl.accountList = slices.SortedFunc(maps.Keys(dl.accountData), common.Hash.Cmp)
	dl.memory += uint64(len(dl.accountList) * common.HashLength)
	return dl.accountList
}

// StorageList returns a sorted list of all storage slot hashes in this diffLayer
// for the given account. If the whole storage is destructed in this layer, then
// an additional flag *destructed = true* will be returned, otherwise the flag is
// false. Besides, the returned list will include the hash of deleted storage slot.
// Note a special case is an account is deleted in a prior tx but is recreated in
// the following tx with some storage slots set. In this case the returned list is
// not empty but the flag is true.
//
// Note, the returned slice is not a copy, so do not modify it.
func (dl *diffLayer) StorageList(accountHash common.Hash) []common.Hash {
	dl.lock.RLock()
	if _, ok := dl.storageData[accountHash]; !ok {
		// Account not tracked by this layer
		dl.lock.RUnlock()
		return nil
	}
	// If an old list already exists, return it
	if list, exist := dl.storageList[accountHash]; exist {
		dl.lock.RUnlock()
		return list // the cached list can't be nil
	}
	dl.lock.RUnlock()

	// No old sorted account list exists, generate a new one
	dl.lock.Lock()
	defer dl.lock.Unlock()

	storageList := slices.SortedFunc(maps.Keys(dl.storageData[accountHash]), common.Hash.Cmp)
	dl.storageList[accountHash] = storageList
	dl.memory += uint64(len(dl.storageList)*common.HashLength + common.HashLength)
	return storageList
}
