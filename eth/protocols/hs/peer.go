package hs

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
)

const (
	voteBufferSize = 64
)

// Peer wraps a remote node on the `hs` protocol.
type Peer struct {
	id      string
	version uint

	*p2p.Peer
	rw     p2p.MsgReadWriter
	logger log.Logger

	term chan struct{}

	// basic anti-spam cache for votes/proposals if needed later
	knownVotes *knownCache

	voteBroadcast chan *VotePacket
	periodBegin   time.Time
	periodCounter int
}

func NewPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter) *Peer {
	id := p.ID().String()
	peer := &Peer{
		id:            id,
		knownVotes:    newKnownCache(4096),
		voteBroadcast: make(chan *VotePacket, voteBufferSize),
		periodBegin:   time.Now(),
		periodCounter: 0,
		Peer:          p,
		rw:            rw,
		version:       version,
		logger:        log.New("peer", id[:8]),
		term:          make(chan struct{}),
	}
	go peer.broadcastVotes()
	return peer
}

func (p *Peer) ID() string      { return p.id }
func (p *Peer) Version() uint   { return p.version }
func (p *Peer) Log() log.Logger { return p.logger }
func (p *Peer) Close()          { close(p.term) }

func (p *Peer) KnownVote(hash common.Hash) bool { return p.knownVotes.contains(hash) }
func (p *Peer) markVotes(votes []*VotePacket) {
	for _, v := range votes {
		if !p.knownVotes.contains(v.BlockHash) {
			p.knownVotes.add(v.BlockHash)
		}
	}
}

// Senders
func (p *Peer) SendProposal(prop *ProposalPacket) error {
	return p2p.Send(p.rw, ProposalMsg, prop)
}
func (p *Peer) SendVote(vote *VotePacket) error {
	p.markVotes([]*VotePacket{vote})
	return p2p.Send(p.rw, VoteMsg, vote)
}
func (p *Peer) SendNewView(nv *NewViewPacket) error {
	return p2p.Send(p.rw, NewViewMsg, nv)
}
func (p *Peer) SendTimeout(to *TimeoutPacket) error {
	return p2p.Send(p.rw, TimeoutMsg, to)
}
func (p *Peer) SendQC(qc *QuorumCertPacket) error {
	return p2p.Send(p.rw, QCMsg, qc)
}

// AsyncSendVotes queues a batch of votes for propagation to the remote peer.
func (p *Peer) AsyncSendVotes(votes []*VotePacket) {
	for _, v := range votes {
		select {
		case p.voteBroadcast <- v:
		default:
			// drop if congested
		}
	}
}

func (p *Peer) broadcastVotes() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.term:
			return
		case <-ticker.C:
			if len(p.voteBroadcast) == 0 {
				continue
			}
			var batch []*VotePacket
			for i := 0; i < voteBufferSize; i++ {
				select {
				case v := <-p.voteBroadcast:
					batch = append(batch, v)
				default:
					i = voteBufferSize
				}
			}
			if len(batch) == 0 {
				continue
			}
			p.markVotes(batch)
			// send as individual votes to minimize decode coupling
			for _, v := range batch {
				_ = p2p.Send(p.rw, VoteMsg, v)
			}
		}
	}
}

// simple LRU-style known cache
// CRITICAL FIX: Added mutex to prevent concurrent map read/write panics
type knownCache struct {
	mu    sync.RWMutex
	order []common.Hash
	set   map[common.Hash]struct{}
	limit int
}

func newKnownCache(limit int) *knownCache {
	return &knownCache{order: make([]common.Hash, 0, limit), set: make(map[common.Hash]struct{}), limit: limit}
}

func (k *knownCache) contains(h common.Hash) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.set[h]
	return ok
}

func (k *knownCache) add(h common.Hash) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.set[h]; ok {
		return
	}
	if len(k.order) >= k.limit {
		oldest := k.order[0]
		k.order = k.order[1:]
		delete(k.set, oldest)
	}
	k.order = append(k.order, h)
	k.set[h] = struct{}{}
}
