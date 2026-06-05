package beanjamin

import (
	"context"
	"encoding/json"

	"go.viam.com/rdk/components/sensor"
)

// The usage sensor tracks physical consumables and service health by mirroring
// several counters — one per key (regular_grinds, decaf_grinds, usage,
// cleanings, successful_consecutive_orders) — into a single external sensor
// resource. The sensor replaces its whole readings map on `set`, so every
// update is a best-effort read-modify-write of the *entire* map: read all
// current readings with Readings, change just the one key, then write them all
// back with DoCommand({"set": <full readings map>}). Writing only the changed
// key would erase every other counter.

// incrementSensorReading adds delta to the sensor's `field` counter, leaving
// every other key untouched. A nil sensor (unconfigured) is a no-op. If the
// readings can't be read, or `field` holds a non-numeric value, the write is
// skipped so a transient failure doesn't clobber the real counters.
func (s *beanjaminCoffee) incrementSensorReading(ctx context.Context, sen sensor.Sensor, label, field string, delta float64) {
	if sen == nil {
		return
	}
	readings, ok := s.readSensorFields(ctx, sen, label)
	if !ok {
		return
	}
	current, ok := numericReading(readings[field])
	if !ok {
		s.logger.Warnf("usage sensor %s: %s = %#v is not numeric, skipping update", label, field, readings[field])
		return
	}
	readings[field] = current + delta
	s.setSensorReadings(ctx, sen, label, readings)
}

// setSensorReading sets `field` to value while preserving every other key. Like
// incrementSensorReading it read-modify-writes the full map, because the sensor
// replaces its readings on `set`. A nil sensor is a no-op; a read failure skips
// the write so we don't erase the other counters.
func (s *beanjaminCoffee) setSensorReading(ctx context.Context, sen sensor.Sensor, label, field string, value float64) {
	if sen == nil {
		return
	}
	readings, ok := s.readSensorFields(ctx, sen, label)
	if !ok {
		return
	}
	readings[field] = value
	s.setSensorReadings(ctx, sen, label, readings)
}

// readSensorFields reads the sensor's full Readings map and returns a mutable
// copy the caller may modify freely. The returned ok is false on a hard
// Readings error, in which case the caller must skip the write rather than
// clobber the real counters with a partial map.
func (s *beanjaminCoffee) readSensorFields(ctx context.Context, sen sensor.Sensor, label string) (map[string]interface{}, bool) {
	readings, err := sen.Readings(ctx, nil)
	if err != nil {
		s.logger.Warnf("usage sensor %s: read failed, skipping update: %v", label, err)
		return nil, false
	}
	out := make(map[string]interface{}, len(readings)+1)
	for k, v := range readings {
		out[k] = v
	}
	return out, true
}

// setSensorReadings writes the full readings map back via
// DoCommand({"set": readings}). Failures log a warning and are otherwise
// ignored (best effort).
func (s *beanjaminCoffee) setSensorReadings(ctx context.Context, sen sensor.Sensor, label string, readings map[string]interface{}) {
	_, err := sen.DoCommand(ctx, map[string]interface{}{"set": readings})
	if err != nil {
		s.logger.Warnf("usage sensor %s: set failed: %v", label, err)
	}
}

// numericReading parses a single readings value as a float64. ok is false for
// an unparseable (non-numeric) value so the caller can skip the update. An
// absent field (nil) is treated as 0 with ok=true so the first increment still
// lands. Values arrive as float64 over the wire; int/int64/json.Number are
// accepted defensively.
func numericReading(v interface{}) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case nil:
		return 0, true // field absent / first run
	default:
		return 0, false
	}
}
