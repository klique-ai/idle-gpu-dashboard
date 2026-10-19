package sender

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
)

// Pool spreads reports over several HTTP senders that deliver in parallel.
// Every key (a node) always goes to the same sender, so each node's reports
// still arrive in order. The hub uses it: one sender, one request at a time,
// can't keep up with a large cluster.
type Pool struct {
	senders []*HTTP
}

// NewPool builds n senders sharing opt; opt.QueueSize is the total, split
// between them.
func NewPool(n int, opt HTTPOptions, log *slog.Logger) (*Pool, error) {
	n = max(n, 1)
	opt.QueueSize = max(opt.QueueSize/n, 1)
	p := &Pool{}
	for range n {
		h, err := NewHTTP(opt, log)
		if err != nil {
			return nil, err
		}
		p.senders = append(p.senders, h)
	}
	return p, nil
}

// EnqueueRaw queues an encoded report on the sender that owns key.
func (p *Pool) EnqueueRaw(key, id string, body []byte) {
	f := fnv.New32a()
	_, _ = f.Write([]byte(key))
	p.senders[f.Sum32()%uint32(len(p.senders))].EnqueueRaw(id, body)
}

// Run delivers until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, h := range p.senders {
		wg.Add(1)
		go func() { defer wg.Done(); h.Run(ctx) }()
	}
	wg.Wait()
}

// Stats adds up the senders' counters and keeps the latest success and error.
func (p *Pool) Stats() Stats {
	var total Stats
	for _, h := range p.senders {
		s := h.Stats()
		total.Pending += s.Pending
		total.Sent += s.Sent
		total.Dropped += s.Dropped
		if s.LastSuccess.After(total.LastSuccess) {
			total.LastSuccess = s.LastSuccess
		}
		if s.LastErrorAt.After(total.LastErrorAt) {
			total.LastError, total.LastErrorAt = s.LastError, s.LastErrorAt
		}
	}
	return total
}
