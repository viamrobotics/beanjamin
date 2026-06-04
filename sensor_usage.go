package beanjamin

import (
	"context"
	"encoding/json"

	"go.viam.com/rdk/components/sensor"
)

// The usage sensor tracks physical consumables and service health by mirroring
// several counters — one per key (regular_grinds, decaf_grinds, usage,
// cleanings, successful_consecutive_orders) — into a single external sensor
// resource. Each update is a best-effort read-modify-write: read the current
// value for a key with Readings, then write the new value with
// DoCommand({"set": {<key>: <value>}}).

// incrementSensorReading adds delta to the sensor's `field` counter. A nil
// sensor (unconfigured) is a no-op. If the current value can't be read, the
// write is skipped so a transient read failure doesn't clobber a real counter.
func (s *beanjaminCoffee) incrementSensorReading(ctx context.Context, sen sensor.Sensor, label, field string, delta float64) {
	if sen == nil {
		return
	}
	current, ok := s.readSensorField(ctx, sen, label, field)
	if !ok {
		return
	}
	s.setSensorReading(ctx, sen, label, field, current+delta)
}

// readSensorField reads one numeric field from the sensor's Readings. The
// returned ok is false on a hard Readings error or an unparseable field value,
// in which case the caller should skip the write rather than clobber a real
// counter. An absent field is treated as 0 with ok=true so the first increment
// still lands. Values arrive as float64 over the wire; int/int64/json.Number
// are accepted defensively.
func (s *beanjaminCoffee) readSensorField(ctx context.Context, sen sensor.Sensor, label, field string) (float64, bool) {
	readings, err := sen.Readings(ctx, nil)
	if err != nil {
		s.logger.Warnf("usage sensor %s: read failed, skipping %s update: %v", label, field, err)
		return 0, false
	}
	switch v := readings[field].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			s.logger.Warnf("usage sensor %s: %s = %q is not numeric, skipping update: %v", label, field, v, err)
			return 0, false
		}
		return f, true
	case nil:
		return 0, true // field absent / first run
	default:
		// Unexpected type: skip the write so we don't reset a real counter.
		s.logger.Warnf("usage sensor %s: %s is %T, not numeric; skipping update", label, field, v)
		return 0, false
	}
}

// setSensorReading writes value to the sensor's `field` via
// DoCommand({"set": {field: value}}). A nil sensor is a no-op; failures log a
// warning and are otherwise ignored.
func (s *beanjaminCoffee) setSensorReading(ctx context.Context, sen sensor.Sensor, label, field string, value float64) {
	if sen == nil {
		return
	}
	_, err := sen.DoCommand(ctx, map[string]interface{}{
		"set": map[string]interface{}{field: value},
	})
	if err != nil {
		s.logger.Warnf("usage sensor %s: set %s=%v failed: %v", label, field, value, err)
	}
}
