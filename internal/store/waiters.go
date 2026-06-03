package store

import (
	"context"
	"sync"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// agentFIFO is the single in-memory agent inbox: a FIFO of items guarded by a mutex,
// plus a slice of parked waiters. Each parked Recv gets its OWN buffered channel
// (cap 1), so a new item resumes exactly one waiter and FIFO order + exactly-one-per-
// Recv hold under concurrent drainers (PLAN §3.10, mirrors Swift's per-waiter boxed
// continuation).
type agentFIFO struct {
	mu      sync.Mutex
	queue   []wire.AgentItem
	waiters []chan wire.AgentItem
}

// enqueue appends items and hands the front of the queue to parked waiters, one item
// each, in order (Swift enqueueAgentItems, Clipboard.swift:653).
func (f *agentFIFO) enqueue(items ...wire.AgentItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, items...)
	for len(f.waiters) > 0 && len(f.queue) > 0 {
		w := f.waiters[0]
		f.waiters = f.waiters[1:]
		item := f.queue[0]
		f.queue = f.queue[1:]
		w <- item // cap-1 buffered: never blocks
	}
}

// recv dequeues one item immediately if available, else parks a per-waiter cap-1
// channel until an enqueue delivers an item or ctx is done. On ctx-done it removes
// its own waiter (best-effort) and returns (nil, nil) — a normal long-poll timeout.
func (f *agentFIFO) recv(ctx context.Context) (*wire.AgentItem, error) {
	f.mu.Lock()
	if len(f.queue) > 0 {
		item := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return &item, nil
	}
	ch := make(chan wire.AgentItem, 1)
	f.waiters = append(f.waiters, ch)
	f.mu.Unlock()

	select {
	case item := <-ch:
		return &item, nil
	case <-ctx.Done():
		f.removeWaiter(ch)
		// Lost-wakeup guard: an enqueue may have delivered between ctx.Done and
		// removeWaiter; drain non-blockingly so the item is not silently dropped.
		select {
		case item := <-ch:
			return &item, nil
		default:
			return nil, nil
		}
	}
}

// removeWaiter unlists ch if still parked (it may already have been consumed by an
// enqueue, in which case it is absent).
func (f *agentFIFO) removeWaiter(ch chan wire.AgentItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, w := range f.waiters {
		if w == ch {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			return
		}
	}
}

// pathWaiters is the clipboard-channel /wait plumbing: latest-wins, not a queue
// (PLAN §3.10/§8.4 coalescing limitation). Parked WaitForNext callers are all
// resumed with the next saved path.
type pathWaiters struct {
	mu      sync.Mutex
	waiters []chan string
}

// resume hands path to every parked waiter and clears the list (Swift resumeWaiters,
// Clipboard.swift:635).
func (p *pathWaiters) resume(path string) {
	p.mu.Lock()
	parked := p.waiters
	p.waiters = nil
	p.mu.Unlock()
	for _, w := range parked {
		w <- path
	}
}

// wait parks a cap-1 channel until a path is saved or ctx is done (timeout → "").
func (p *pathWaiters) wait(ctx context.Context) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()

	select {
	case path := <-ch:
		return path, nil
	case <-ctx.Done():
		p.remove(ch)
		select {
		case path := <-ch:
			return path, nil
		default:
			return "", nil
		}
	}
}

func (p *pathWaiters) remove(ch chan string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, w := range p.waiters {
		if w == ch {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return
		}
	}
}
