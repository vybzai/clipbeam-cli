package store

import (
	"context"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// TestAgentFIFOOrdering asserts items dequeue in FIFO order, exactly one per recv.
func TestAgentFIFOOrdering(t *testing.T) {
	var f agentFIFO
	f.enqueue(
		wire.AgentItem{Type: "text", Text: wire.Ptr("a")},
		wire.AgentItem{Type: "text", Text: wire.Ptr("b")},
		wire.AgentItem{Type: "text", Text: wire.Ptr("c")},
	)
	for _, want := range []string{"a", "b", "c"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		item, err := f.recv(ctx)
		cancel()
		if err != nil || item == nil || item.Text == nil || *item.Text != want {
			t.Fatalf("recv = %+v, %v; want text %q", item, err, want)
		}
	}
}

// TestAgentFIFOTimeout asserts a parked recv returns (nil,nil) on a context timeout
// (a normal long-poll timeout, PLAN §3.10).
func TestAgentFIFOTimeout(t *testing.T) {
	var f agentFIFO
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	item, err := f.recv(ctx)
	if err != nil || item != nil {
		t.Fatalf("recv on empty FIFO past deadline = %+v, %v; want nil,nil", item, err)
	}
}

// TestAgentFIFOResumesParkedWaiter asserts an enqueue resumes a parked recv.
func TestAgentFIFOResumesParkedWaiter(t *testing.T) {
	var f agentFIFO
	done := make(chan *wire.AgentItem, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		item, _ := f.recv(ctx)
		done <- item
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine park
	f.enqueue(wire.AgentItem{Type: "text", Text: wire.Ptr("z")})
	select {
	case item := <-done:
		if item == nil || item.Text == nil || *item.Text != "z" {
			t.Fatalf("parked recv resumed with %+v, want text z", item)
		}
	case <-time.After(time.Second):
		t.Fatal("parked recv was not resumed by enqueue")
	}
}

// TestAgentFIFOConcurrentOnePerRecv asserts that under N concurrent parked drainers,
// N enqueued items resume EXACTLY one waiter each (no item delivered twice, none
// dropped) — the per-waiter cap-1 channel invariant (PLAN §3.10).
func TestAgentFIFOConcurrentOnePerRecv(t *testing.T) {
	var f agentFIFO
	const n = 50
	got := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			item, _ := f.recv(ctx)
			if item != nil && item.Text != nil {
				got <- *item.Text
			} else {
				got <- ""
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let all drainers park
	items := make([]wire.AgentItem, n)
	for i := range items {
		items[i] = wire.AgentItem{Type: "text", Text: wire.Ptr(itoaT(i))}
	}
	f.enqueue(items...)

	seen := map[string]int{}
	for i := 0; i < n; i++ {
		select {
		case v := <-got:
			if v == "" {
				t.Fatal("a drainer timed out instead of receiving an item")
			}
			seen[v]++
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d drainers resumed", i, n)
		}
	}
	if len(seen) != n {
		t.Fatalf("distinct items delivered = %d, want %d (one-per-recv violated)", len(seen), n)
	}
	for v, c := range seen {
		if c != 1 {
			t.Fatalf("item %q delivered %d times, want exactly 1", v, c)
		}
	}
}

// TestPathWaiterResume asserts a parked WaitForNext-style waiter is resumed with the
// next saved path (latest-wins clipboard channel, PLAN §3.10).
func TestPathWaiterResume(t *testing.T) {
	var p pathWaiters
	done := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		path, _ := p.wait(ctx)
		done <- path
	}()
	time.Sleep(20 * time.Millisecond)
	p.resume("/abs/path")
	select {
	case path := <-done:
		if path != "/abs/path" {
			t.Fatalf("wait resumed with %q, want /abs/path", path)
		}
	case <-time.After(time.Second):
		t.Fatal("parked wait was not resumed")
	}
}
