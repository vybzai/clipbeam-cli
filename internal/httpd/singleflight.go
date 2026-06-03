package httpd

// singleFlight is the capacity-1 /clip gate (PLAN §3.11). It is taken as the LAST
// check (after class/token/CL/ceiling so early returns need no release) and released
// on EVERY teardown path (success, decode error, 413 mid-stream, context cancel,
// client RST) so it can never wedge into a permanent 503. A second concurrent /clip
// gets 503; the Mac sender does not auto-retry 503.
type singleFlight struct {
	slot chan struct{}
}

// newSingleFlight returns a fresh capacity-1 gate.
func newSingleFlight() *singleFlight {
	return &singleFlight{slot: make(chan struct{}, 1)}
}

// tryAcquire takes the slot without blocking. It returns true if the slot was free
// (the caller MUST defer release), false if a transfer is already in flight (→ 503).
func (g *singleFlight) tryAcquire() bool {
	select {
	case g.slot <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns the slot. Safe to call once per successful tryAcquire; idempotency
// against double-release is the caller's responsibility (a single deref guard).
func (g *singleFlight) release() {
	select {
	case <-g.slot:
	default:
	}
}
