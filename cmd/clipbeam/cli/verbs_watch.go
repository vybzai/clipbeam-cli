package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// watchAgentRecord is one NDJSON line for an agent-channel item (PLAN §8.4).
type watchAgentRecord struct {
	Schema  string  `json:"schema"`
	Event   string  `json:"event"`
	Channel string  `json:"channel"`
	Type    string  `json:"type"`
	Sender  string  `json:"sender"`
	Path    *string `json:"path"`
	Text    *string `json:"text"`
	Cid     *string `json:"cid"`
	TS      string  `json:"ts"`
}

// watchClipboardRecord is one NDJSON line for a clipboard-channel item (PLAN §8.4).
type watchClipboardRecord struct {
	Schema  string `json:"schema"`
	Event   string `json:"event"`
	Channel string `json:"channel"`
	Path    string `json:"path"`
	TS      string `json:"ts"`
}

// runWatch implements `clipbeam watch` (PLAN §8.4): the streaming primitive that
// generalizes wait + recv with ZERO new wire surface. It re-arms the existing loopback
// long-poll endpoints in a loop — GET /recv?timeout=N for the agent channel and the
// fixed-120 s GET /wait for the clipboard channel — emitting one NDJSON record per item
// to stdout (each line flushed) until SIGINT/SIGTERM (exit 0).
//
// Known clipboard-channel limitation (PLAN §8.4): /wait+last_path is latest-wins, NOT a
// queue, so a burst between two /wait re-arms can be COALESCED (only the latest path
// surfaces); the agent FIFO has no such loss. `watch` and a manual `recv` must not run
// concurrently on the same agent channel (they race for FIFO items).
func runWatch(o out, channel string) error {
	switch channel {
	case "agent", "clipboard", "both":
	default:
		return usageError("watch: --channel must be agent | clipboard | both (got %q)", channel)
	}

	c, err := loadControl()
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancels the whole stream → clean exit 0 (PLAN §8.4).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if channel != "both" {
		o.diag("watch: streaming the %s channel; press Ctrl-C to stop", channel)
	} else {
		o.diag("watch: streaming both channels; press Ctrl-C to stop")
		o.diag("watch: clipboard is latest-wins (a burst between re-arms may coalesce); the agent FIFO is not")
	}

	// A single stdout mutex serializes the two re-arm loops so an agent line and a
	// clipboard line never interleave mid-record.
	var mu sync.Mutex
	emit := func(line string) {
		mu.Lock()
		_, _ = io.WriteString(o.stdout, line+"\n")
		if f, ok := o.stdout.(*os.File); ok {
			_ = f.Sync() // best-effort flush so a consumer sees each line promptly
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	if channel == "agent" || channel == "both" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchAgentLoop(ctx, o, c, emit)
		}()
	}
	if channel == "clipboard" || channel == "both" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchClipboardLoop(ctx, o, c, emit)
		}()
	}
	wg.Wait()
	return nil // SIGINT/SIGTERM is a clean exit 0 (PLAN §8.4)
}

// watchAgentLoop re-arms GET /recv in a loop, emitting one agent NDJSON record per
// dequeued item. A 204/timeout simply re-arms; a transport error backs off briefly so
// a dead daemon does not spin.
func watchAgentLoop(ctx context.Context, o out, c *controlClient, emit func(string)) {
	const poll = 120
	for ctx.Err() == nil {
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(poll+20)*time.Second)
		status, body, err := c.get(callCtx, "/recv?timeout="+itoa(poll))
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.trace("watch: agent re-arm error: %v", err)
			backoff(ctx)
			continue
		}
		if status != 200 {
			continue // 204 timeout, or a transient non-200 → re-arm
		}
		item := parseRecvBody(string(body))
		rec := watchAgentRecord{
			Schema:  schemaVersion,
			Event:   "item",
			Channel: "agent",
			Type:    item.Type,
			Sender:  item.Sender,
			Path:    nilIfEmpty(item.Path),
			Text:    textPtr(item),
			Cid:     nilIfEmpty(item.Cid),
			TS:      nowRFC3339(),
		}
		if b, err := json.Marshal(rec); err == nil {
			emit(string(b))
		}
	}
}

// watchClipboardLoop re-arms the fixed-120 s GET /wait in a loop, emitting one
// clipboard NDJSON record per surfaced path. Coalescing is inherent to latest-wins
// /wait (documented in runWatch's header, PLAN §8.4).
func watchClipboardLoop(ctx context.Context, o out, c *controlClient, emit func(string)) {
	for ctx.Err() == nil {
		callCtx, cancel := context.WithTimeout(ctx, controlGetTimeout)
		status, body, err := c.get(callCtx, "/wait")
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.trace("watch: clipboard re-arm error: %v", err)
			backoff(ctx)
			continue
		}
		if status != 200 {
			continue // 204 timeout → re-arm
		}
		rec := watchClipboardRecord{
			Schema:  schemaVersion,
			Event:   "item",
			Channel: "clipboard",
			Path:    string(body),
			TS:      nowRFC3339(),
		}
		if b, err := json.Marshal(rec); err == nil {
			emit(string(b))
		}
	}
}

// backoff sleeps ~1 s (or until ctx is done) so a re-arm error loop does not spin hot.
func backoff(ctx context.Context) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// nowRFC3339 is the CLI LOCAL receive time in RFC3339 UTC (PLAN §8.4: ts is the CLI
// receive time; the wire carries none).
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
