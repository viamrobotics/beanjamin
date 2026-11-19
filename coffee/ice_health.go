package coffee

// Whether the ice machine can be reached. The ice board is usually a remote
// machine's board, and a remote that drops off the network is still handed to
// the service as a resource: every call on it fails with "not connected". A
// reachable board is checked by reading the ice pin's level, which never drives
// the pin, so it is safe to do while a dispense holds it HIGH.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"beanjamin/coffee/order"
)

const (
	// iceProbeInterval is how often the background probe reads the ice pin. The
	// kiosk polls Status every two seconds, so a dropped ice machine greys out the
	// iced drinks within a few seconds of either.
	iceProbeInterval = 5 * time.Second
	// iceProbeTimeout bounds one read. A remote that is reconnecting can hold a
	// call much longer than a healthy one takes.
	iceProbeTimeout = 3 * time.Second
)

// iceHealth is the last probe's result, shared by the probe loop, Status and
// order intake.
type iceHealth struct {
	mu      sync.Mutex
	checked bool
	ok      bool
	reason  string
}

// set records a probe result and reports whether availability changed, so the
// caller logs transitions rather than every probe.
func (h *iceHealth) set(ok bool, reason string) (changed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed = !h.checked || h.ok != ok
	h.checked, h.ok, h.reason = true, ok, reason
	return changed
}

// get returns the last result; checked is false until the first probe lands.
func (h *iceHealth) get() (ok bool, reason string, checked bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ok, h.reason, h.checked
}

// probeIce reads the ice pin once and records the result.
func (s *beanjaminCoffee) probeIce(ctx context.Context) (bool, string) {
	ok, reason := s.readIcePin(ctx)
	if s.iceHealth.set(ok, reason) {
		if ok {
			s.logger.Infof("ice machine reachable: iced drinks available")
		} else {
			s.logger.Warnf("ice machine unreachable, iced drinks unavailable: %s", reason)
		}
	}
	return ok, reason
}

func (s *beanjaminCoffee) readIcePin(ctx context.Context) (bool, string) {
	if s.iceBoard == nil {
		return false, "no ice board configured (set ice_board_name)"
	}
	ctx, cancel := context.WithTimeout(ctx, iceProbeTimeout)
	defer cancel()
	pin, err := s.iceBoard.GPIOPinByName(s.icePinName())
	if err != nil {
		return false, fmt.Sprintf("get ice pin %q: %v", s.icePinName(), err)
	}
	if _, err := pin.Get(ctx, nil); err != nil {
		return false, fmt.Sprintf("read ice pin %q: %v", s.icePinName(), err)
	}
	return true, ""
}

// iceAvailability is the cached probe result for Status. Before the first
// probe lands it reports the ice machine as unavailable, so the kiosk never
// offers an iced drink it has not seen the machine answer for.
func (s *beanjaminCoffee) iceAvailability() (bool, string) {
	ok, reason, checked := s.iceHealth.get()
	if !checked {
		return false, "ice machine not checked yet"
	}
	return ok, reason
}

// requireIce refuses an iced drink while the ice machine cannot be reached. It
// probes afresh rather than trusting the cache: it runs once per order, and the
// cache can be a probe interval stale in either direction.
func (s *beanjaminCoffee) requireIce(ctx context.Context, drink string) error {
	if !order.IsIced(drink) {
		return nil
	}
	if ok, reason := s.probeIce(ctx); !ok {
		return fmt.Errorf("%w: %s", errIceUnavailable, reason)
	}
	return nil
}

// errIceUnavailable marks an iced order refused because the ice machine could
// not be reached.
var errIceUnavailable = errors.New("ice machine unavailable")

// iceHealthLoop probes the ice machine until the service closes. Only started
// when the machine serves iced drinks.
func (s *beanjaminCoffee) iceHealthLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.queueStop
		cancel()
	}()
	ticker := time.NewTicker(iceProbeInterval)
	defer ticker.Stop()
	for {
		s.probeIce(ctx)
		select {
		case <-s.queueStop:
			return
		case <-ticker.C:
		}
	}
}
