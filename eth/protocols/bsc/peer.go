package bsc

import (
	"errors"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
)

const (
	// maxKnownVotes is the maximum vote hashes to keep in the known list
	// before starting to randomly evict them.
	maxKnownVotes = 5376

	// maxKnownChunks is the maximum number of chunk identifiers (block hash +
	// chunk index) to remember per peer so that we never resend or re-request
	// the same chunk twice.
	maxKnownChunks = 8192

	// voteBufferSize is the maximum number of batch votes can be hold before sending
	voteBufferSize = 21 * 2

	// used to avoid of DDOS attack
	// It's the max number of received votes per second from one peer
	// 21 validators exist now, so 21 votes will be produced every one block interval
	// so the limit is 28 = 21/0.75, here set it to 40 with a buffer.
	receiveRateLimitPerSecond = 40

	// the time span of one period
	secondsPerPeriod = float64(30)

	chunkQueueSize               = maxShardCount * 2
	chunkRequestQueueSize        = 32
	chunkReceiptQueueSize        = 64
	chunkIngressPerSecond        = maxShardCount * 2
	chunkIngressBytesPerSecond   = 32 * 1024 * 1024
	chunkRepairRequestsPerSecond = 32
	chunkRepairIndexesPerSecond  = maxShardCount * 2
)

// chunkKey uniquely identifies a single chunk of a block on the wire.
type chunkKey struct {
	hash  common.Hash
	root  common.Hash
	index uint
	depth uint8
	route common.Hash
}

func makeChunkKey(pkt *BlockChunkPacket) chunkKey {
	route := make([]byte, 1, 1+len(pkt.RelayTargets)*len(common.Hash{}))
	route[0] = pkt.RelayDepth
	for _, target := range pkt.RelayTargets {
		route = append(route, target[:]...)
	}
	return chunkKey{
		hash:  pkt.BlockHash,
		root:  pkt.ShardRoot,
		index: pkt.ChunkIndex,
		depth: pkt.RelayDepth,
		route: crypto.Keccak256Hash(route),
	}
}

// Peer is a collection of relevant information we have about a `bsc` peer.
type Peer struct {
	id            string                     // Unique ID for the peer, cached
	knownVotes    *knownCache                // Set of vote hashes known to be known by this peer
	voteBroadcast chan []*types.VoteEnvelope // Channel used to queue votes propagation requests
	periodBegin   time.Time                  // Begin time of the latest period for votes counting
	periodCounter uint                       // Votes number in the latest period
	dispatcher    *Dispatcher                // Message request-response dispatcher

	// Bsc4 block chunk propagation state.  `chunkBroadcast` queues outbound
	// chunks to be written asynchronously so the leader path never blocks on
	// a slow peer.  `knownChunks` records chunks already sent / acknowledged
	// to avoid redundant transfers.
	chunkBroadcast  chan *BlockChunkPacket
	chunkRequests   chan *GetBlockChunksPacket
	chunkReceipts   chan *BlockChunkReceiptPacket
	knownChunks     *knownChunkCache
	knownReceipts   *knownChunkReceiptCache
	chunkRateMu     sync.Mutex
	chunkRateAt     time.Time
	chunkRateCount  int
	chunkRateBytes  int
	repairRateAt    time.Time
	repairRateCount int
	repairRateIndex int

	*p2p.Peer                   // The embedded P2P package peer
	rw        p2p.MsgReadWriter // Input/output streams for bsc
	version   uint              // Protocol version negotiated
	logger    log.Logger        // Contextual logger with the peer id injected
	term      chan struct{}     // Termination channel to stop the broadcasters
	termOnce  sync.Once
}

// NewPeer create a wrapper for a network connection and negotiated protocol
// version.
func NewPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter) *Peer {
	id := p.ID().String()
	peer := &Peer{
		id:             id,
		knownVotes:     newKnownCache(maxKnownVotes),
		voteBroadcast:  make(chan []*types.VoteEnvelope, voteBufferSize),
		periodBegin:    time.Now(),
		periodCounter:  0,
		chunkBroadcast: make(chan *BlockChunkPacket, chunkQueueSize),
		chunkRequests:  make(chan *GetBlockChunksPacket, chunkRequestQueueSize),
		chunkReceipts:  make(chan *BlockChunkReceiptPacket, chunkReceiptQueueSize),
		knownChunks:    newKnownChunkCache(maxKnownChunks),
		knownReceipts:  newKnownChunkReceiptCache(maxKnownChunks),
		Peer:           p,
		rw:             rw,
		version:        version,
		logger:         log.New("peer", id[:8]),
		term:           make(chan struct{}),
	}
	peer.dispatcher = NewDispatcher(peer)
	go peer.broadcastVotes()
	if version >= Bsc4 {
		go peer.broadcastChunks()
	}
	return peer
}

// ID retrieves the peer's unique identifier.
func (p *Peer) ID() string {
	return p.id
}

// Version retrieves the peer's negotiated `bsc` protocol version.
func (p *Peer) Version() uint {
	return p.version
}

// Log overrides the P2P logget with the higher level one containing only the id.
func (p *Peer) Log() log.Logger {
	return p.logger
}

// Close signals the broadcast goroutine to terminate. Only ever call this if
// you created the peer yourself via NewPeer. Otherwise let whoever created it
// clean it up!
func (p *Peer) Close() {
	p.termOnce.Do(func() { close(p.term) })
}

// KnownVote returns whether peer is known to already have a vote.
func (p *Peer) KnownVote(hash common.Hash) bool {
	return p.knownVotes.contains(hash)
}

// markVotes marks votes as known for the peer, ensuring that they
// will never be repropagated to this particular peer.
func (p *Peer) markVotes(votes []*types.VoteEnvelope) {
	for _, vote := range votes {
		if !p.knownVotes.contains(vote.Hash()) {
			// If we reached the memory allowance, drop a previously known vote hash
			p.knownVotes.add(vote.Hash())
		}
	}
}

// sendVotes propagates a batch of votes to the remote peer.
func (p *Peer) sendVotes(votes []*types.VoteEnvelope) error {
	// Mark all the votes as known, but ensure we don't overflow our limits
	p.markVotes(votes)
	return p2p.Send(p.rw, VotesMsg, &VotesPacket{votes})
}

// AsyncSendVotes queues a batch of vote hashes for propagation to a remote peer. If
// the peer's broadcast queue is full, the event is silently dropped.
func (p *Peer) AsyncSendVotes(votes []*types.VoteEnvelope) {
	select {
	case p.voteBroadcast <- votes:
	case <-p.term:
		p.Log().Debug("Dropping vote propagation for closed peer", "count", len(votes))
	default:
		p.Log().Debug("Dropping vote propagation for abnormal peer", "count", len(votes))
	}
}

// Step into the next period when secondsPerPeriod seconds passed,
// Otherwise, check whether the number of received votes extra (secondsPerPeriod * receiveRateLimitPerSecond)
func (p *Peer) IsOverLimitAfterReceiving() bool {
	if timeInterval := time.Since(p.periodBegin).Seconds(); timeInterval >= secondsPerPeriod {
		if p.periodCounter > uint(secondsPerPeriod*receiveRateLimitPerSecond) {
			p.Log().Debug("sending votes too much", "secondsPerPeriod", secondsPerPeriod, "count ", p.periodCounter)
		}
		p.periodBegin = time.Now()
		p.periodCounter = 0
		return false
	}
	p.periodCounter += 1
	return p.periodCounter > uint(secondsPerPeriod*receiveRateLimitPerSecond)
}

// broadcastVotes is a write loop that schedules votes broadcasts
// to the remote peer. The goal is to have an async writer that does not lock up
// node internals and at the same time rate limits queued data.
func (p *Peer) broadcastVotes() {
	for {
		select {
		case votes := <-p.voteBroadcast:
			if err := p.sendVotes(votes); err != nil {
				return
			}
			p.Log().Trace("Sent votes", "count", len(votes))

		case <-p.term:
			return
		}
	}
}

// AsyncSendBlockChunk queues a Bsc4 shard. The return value reports whether it
// was accepted by the bounded queue, allowing the caller to fall back to a
// regular full-block broadcast when a peer is congested.
func (p *Peer) AsyncSendBlockChunk(pkt *BlockChunkPacket) bool {
	return p.enqueueBlockChunk(pkt, false)
}

// AsyncSendBlockChunkForce bypasses the known cache for an explicit repair
// response. A previously sent shard may have been lost in transit.
func (p *Peer) AsyncSendBlockChunkForce(pkt *BlockChunkPacket) bool {
	return p.enqueueBlockChunk(pkt, true)
}

func (p *Peer) enqueueBlockChunk(pkt *BlockChunkPacket, force bool) bool {
	if p == nil || pkt == nil || p.version < Bsc4 {
		return false
	}
	select {
	case <-p.term:
		return false
	default:
	}
	key := makeChunkKey(pkt)
	if !force && p.knownChunks.contains(key) {
		return true
	}
	select {
	case p.chunkBroadcast <- pkt:
		p.knownChunks.add(key)
		return true
	case <-p.term:
		p.Log().Debug("Dropping chunk propagation for closed peer", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
		BlockChunkShardDropMeter.Mark(1)
		return false
	default:
		p.Log().Debug("Dropping chunk propagation for full peer queue", "hash", pkt.BlockHash, "index", pkt.ChunkIndex)
		BlockChunkShardDropMeter.Mark(1)
		return false
	}
}

// sendBlockChunk writes a single block chunk packet to the remote peer.
func (p *Peer) sendBlockChunk(pkt *BlockChunkPacket) error {
	if err := p2p.Send(p.rw, BlockChunkMsg, pkt); err != nil {
		return err
	}
	BlockChunkShardOutMeter.Mark(1)
	return nil
}

// broadcastChunks is a write loop that schedules block chunk broadcasts to the
// remote peer asynchronously.  It decouples the leader fanout path from slow
// peers, mirroring the behaviour of `broadcastVotes`.
func (p *Peer) broadcastChunks() {
	for {
		select {
		case pkt := <-p.chunkBroadcast:
			if err := p.sendBlockChunk(pkt); err != nil {
				p.Close()
				return
			}
			p.Log().Trace("Sent block chunk", "hash", pkt.BlockHash, "index", pkt.ChunkIndex, "total", pkt.ChunkCount)
		case req := <-p.chunkRequests:
			if err := p2p.Send(p.rw, GetBlockChunksMsg, req); err != nil {
				p.Close()
				return
			}
		case receipt := <-p.chunkReceipts:
			if err := p2p.Send(p.rw, BlockChunkReceiptMsg, receipt); err != nil {
				p.Close()
				return
			}
		case <-p.term:
			return
		}
	}
}

// RequestMissingChunks requests the missing shards for a block from the remote
// peer using the Bsc4 GetBlockChunks message. This is a best-effort fire-and-
// forget call: it does not block waiting for a response.  The reply arrives as
// regular BlockChunkMsg packets which are handled by the message handler.
func (p *Peer) RequestMissingChunks(blockHash, shardRoot common.Hash, indexes []uint) error {
	if p == nil || p.version < Bsc4 {
		return errors.New("peer does not support Bsc4 chunk protocol")
	}
	if shardRoot == (common.Hash{}) || len(indexes) == 0 || len(indexes) > MaxBlockChunkRequestIndexes {
		return errors.New("invalid missing chunk request")
	}
	req := &GetBlockChunksPacket{
		BlockHash:      blockHash,
		ShardRoot:      shardRoot,
		MissingIndexes: append([]uint(nil), indexes...),
	}
	select {
	case <-p.term:
		return errors.New("peer is closed")
	default:
	}
	select {
	case p.chunkRequests <- req:
		BlockChunkMissingReqMeter.Mark(int64(len(indexes)))
		return nil
	case <-p.term:
		return errors.New("peer is closed")
	default:
		return errors.New("chunk repair request queue is full")
	}
}

// AsyncSendBlockChunkReceipt confirms a usable Bsc5 encoding to its producer.
// The receipt is deliberately bounded and deduplicated for the lifetime of the
// authenticated peer connection; repeated confirmation of the same block/root
// is idempotent on that connection.
func (p *Peer) AsyncSendBlockChunkReceipt(blockHash, shardRoot common.Hash) bool {
	if p == nil || p.version < Bsc5 || blockHash == (common.Hash{}) || shardRoot == (common.Hash{}) {
		return false
	}
	select {
	case <-p.term:
		return false
	default:
	}
	key := chunkReceiptKey{hash: blockHash, root: shardRoot}
	if p.knownReceipts.contains(key) {
		return true
	}
	select {
	case p.chunkReceipts <- &BlockChunkReceiptPacket{BlockHash: blockHash, ShardRoot: shardRoot}:
		p.knownReceipts.add(key)
		return true
	case <-p.term:
		return false
	default:
		return false
	}
}

func (p *Peer) allowChunkIngress(bytes int) bool {
	p.chunkRateMu.Lock()
	defer p.chunkRateMu.Unlock()
	if bytes < 0 {
		return false
	}
	now := time.Now()
	if p.chunkRateAt.IsZero() || now.Sub(p.chunkRateAt) >= time.Second {
		p.chunkRateAt = now
		p.chunkRateCount = 0
		p.chunkRateBytes = 0
	}
	if p.chunkRateCount >= chunkIngressPerSecond || p.chunkRateBytes > chunkIngressBytesPerSecond-bytes {
		return false
	}
	p.chunkRateCount++
	p.chunkRateBytes += bytes
	return true
}

func (p *Peer) allowChunkRepairRequest(indexes int) bool {
	p.chunkRateMu.Lock()
	defer p.chunkRateMu.Unlock()
	if indexes <= 0 {
		return false
	}
	now := time.Now()
	if p.repairRateAt.IsZero() || now.Sub(p.repairRateAt) >= time.Second {
		p.repairRateAt = now
		p.repairRateCount = 0
		p.repairRateIndex = 0
	}
	if p.repairRateCount >= chunkRepairRequestsPerSecond ||
		p.repairRateIndex > chunkRepairIndexesPerSecond-indexes {
		return false
	}
	p.repairRateCount++
	p.repairRateIndex += indexes
	return true
}

// knownCache is a cache for known hashes.
type knownCache struct {
	hashes mapset.Set[common.Hash]
	max    int
}

// newKnownCache creates a new knownCache with a max capacity.
func newKnownCache(max int) *knownCache {
	return &knownCache{
		max:    max,
		hashes: mapset.NewSet[common.Hash](),
	}
}

// add adds a list of elements to the set.
func (k *knownCache) add(hashes ...common.Hash) {
	for k.hashes.Cardinality() > max(0, k.max-len(hashes)) {
		k.hashes.Pop()
	}
	for _, hash := range hashes {
		k.hashes.Add(hash)
	}
}

// contains returns whether the given item is in the set.
func (k *knownCache) contains(hash common.Hash) bool {
	return k.hashes.Contains(hash)
}

// knownChunkCache is a cache for chunk keys already sent/received for a peer.
type knownChunkCache struct {
	keys mapset.Set[chunkKey]
	max  int
}

type chunkReceiptKey struct {
	hash common.Hash
	root common.Hash
}

type knownChunkReceiptCache struct {
	keys mapset.Set[chunkReceiptKey]
	max  int
}

func newKnownChunkReceiptCache(max int) *knownChunkReceiptCache {
	return &knownChunkReceiptCache{
		max:  max,
		keys: mapset.NewSet[chunkReceiptKey](),
	}
}

func (k *knownChunkReceiptCache) add(keys ...chunkReceiptKey) {
	for k.keys.Cardinality() > max(0, k.max-len(keys)) {
		k.keys.Pop()
	}
	for _, key := range keys {
		k.keys.Add(key)
	}
}

func (k *knownChunkReceiptCache) contains(key chunkReceiptKey) bool {
	return k.keys.Contains(key)
}

// newKnownChunkCache creates a new knownChunkCache with a max capacity.
func newKnownChunkCache(max int) *knownChunkCache {
	return &knownChunkCache{
		max:  max,
		keys: mapset.NewSet[chunkKey](),
	}
}

// add adds a list of chunk keys to the set.
func (k *knownChunkCache) add(keys ...chunkKey) {
	for k.keys.Cardinality() > max(0, k.max-len(keys)) {
		k.keys.Pop()
	}
	for _, key := range keys {
		k.keys.Add(key)
	}
}

// contains returns whether the given chunk key is in the set.
func (k *knownChunkCache) contains(key chunkKey) bool {
	return k.keys.Contains(key)
}

// RequestBlocksByRange send GetBlocksByRangeMsg by request start block hash
func (p *Peer) RequestBlocksByRange(startHeight uint64, startHash common.Hash, count uint64) ([]*BlockData, error) {
	requestID := p.dispatcher.GenRequestID()
	res, err := p.dispatcher.DispatchRequest(&Request{
		code:      GetBlocksByRangeMsg,
		want:      BlocksByRangeMsg,
		requestID: requestID,
		data: &GetBlocksByRangePacket{
			RequestId:        requestID,
			StartBlockHeight: startHeight,
			StartBlockHash:   startHash,
			Count:            count,
		},
		timeout: 400 * time.Millisecond,
	})
	log.Debug("RequestBlocksByRange result", "requestID", requestID, "ret", res == nil, "err", err)
	if err != nil {
		return nil, err
	}

	// Type assertion to get response object
	ret, ok := res.(*BlocksByRangePacket)
	if !ok {
		return nil, errors.New("unexpected response type")
	}

	return ret.Blocks, nil
}
