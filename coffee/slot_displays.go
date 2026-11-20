package coffee

// Serving-slot displays: one character LCD (viam:character-lcd, a generic
// component, typically on a sub-part) per serving-area slot, matched by index.
// When a drink is set down in a slot, that slot's display shows the customer's
// name and the drink with its backlight on; after slotDisplayClearAfter it is
// blanked and the backlight switched off.

import (
	"context"
	"sync"
	"time"

	genericcomponent "go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

const (
	// slotDisplayClearAfter is how long a ready drink's name stays up.
	slotDisplayClearAfter = 10 * time.Minute
	// slotDisplayCallTimeout bounds each DoCommand to a display, which lives on
	// a sub-part that may be unreachable.
	slotDisplayCallTimeout = 5 * time.Second
)

// slotDisplays drives the serving_slot_displays. A nil *slotDisplays (none
// configured) is a no-op.
type slotDisplays struct {
	logger     logging.Logger
	clearAfter time.Duration
	// displays[i] is serving slot i's display; nil where it did not resolve.
	displays []*slotDisplay
	// ctx is cancelled on close so in-flight writes to an unreachable display
	// don't outlive the service.
	ctx    context.Context
	cancel context.CancelFunc
}

type slotDisplay struct {
	name string
	res  resource.Resource

	// mu serializes the commands sent to the display, so a clear and a show
	// can't interleave their backlight and text writes.
	mu sync.Mutex
	// gen counts the shows; a clear scheduled by an earlier show is skipped
	// once a newer one has put another drink's text up.
	gen   uint64
	timer *time.Timer
}

// newSlotDisplays resolves each configured display. A display that is not
// available is logged and skipped rather than failing the service: the sub-part
// carrying it may be offline, and drinks must still be made. Every resolved
// display is blanked with its backlight off, since a name left up by a previous
// instance (rebuild, crash) has lost the timer that would have cleared it.
func newSlotDisplays(deps resource.Dependencies, names []string, logger logging.Logger) *slotDisplays {
	if len(names) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sd := &slotDisplays{
		logger:     logger,
		clearAfter: slotDisplayClearAfter,
		displays:   make([]*slotDisplay, len(names)),
		ctx:        ctx,
		cancel:     cancel,
	}
	for i, name := range names {
		res, ok := deps[genericcomponent.Named(name)]
		if !ok {
			logger.Warnf("serving_slot_displays[%d] %q not available; slot %d will show no name", i, name, i+1)
			continue
		}
		d := &slotDisplay{name: name, res: res}
		sd.displays[i] = d
		go sd.clear(d, 0)
	}
	return sd
}

// show puts the customer's name on the first line of slot's display and the
// drink on the second, turns the backlight on, and schedules the clear. It
// returns at once; the display is written in the background so a slow or
// unreachable sub-part never holds up the arm.
func (sd *slotDisplays) show(slot int, customer, drink string) {
	if sd == nil || slot < 0 || slot >= len(sd.displays) || sd.displays[slot] == nil {
		return
	}
	d := sd.displays[slot]
	go func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if sd.ctx.Err() != nil {
			return
		}
		d.gen++
		gen := d.gen
		if d.timer != nil {
			d.timer.Stop()
		}
		// The timer is armed even if a write fails, so a partly written display
		// still goes dark.
		d.timer = time.AfterFunc(sd.clearAfter, func() { sd.clear(d, gen) })

		if err := sd.command(d, map[string]any{"command": "backlight", "on": true}); err != nil {
			sd.logger.Warnf("serving slot %d display %q: turning backlight on: %v", slot+1, d.name, err)
		}
		if err := sd.command(d, map[string]any{"command": "write_lines", "lines": []any{customer, drink}}); err != nil {
			sd.logger.Warnf("serving slot %d display %q: writing %q / %q: %v", slot+1, d.name, customer, drink, err)
		}
	}()
}

// clear blanks d and turns its backlight off, unless a show newer than gen has
// put text up since.
func (sd *slotDisplays) clear(d *slotDisplay, gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gen != gen || sd.ctx.Err() != nil {
		return
	}
	if err := sd.command(d, map[string]any{"command": "clear"}); err != nil {
		sd.logger.Warnf("serving slot display %q: clearing: %v", d.name, err)
	}
	if err := sd.command(d, map[string]any{"command": "backlight", "on": false}); err != nil {
		sd.logger.Warnf("serving slot display %q: turning backlight off: %v", d.name, err)
	}
}

func (sd *slotDisplays) command(d *slotDisplay, cmd map[string]any) error {
	ctx, cancel := context.WithTimeout(sd.ctx, slotDisplayCallTimeout)
	defer cancel()
	_, err := d.res.DoCommand(ctx, cmd)
	return err
}

// close stops the pending clears and abandons in-flight writes. The text left
// up is cleared by the next instance on startup.
func (sd *slotDisplays) close() {
	if sd == nil {
		return
	}
	sd.cancel()
	for _, d := range sd.displays {
		if d == nil {
			continue
		}
		d.mu.Lock()
		if d.timer != nil {
			d.timer.Stop()
		}
		d.mu.Unlock()
	}
}
