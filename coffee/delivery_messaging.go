package coffee

// Spoken handoff for a delivery order. The cart learns the drink is ready by
// polling delivery_machine_ready / delivery_handover_status; this only announces it locally.

import (
	"context"
	"fmt"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"
)

// readyForDelivery announces a delivery order whose drink is in the serving
// area. The caller has already recorded the slot on the delivery ledger.
func (s *beanjaminCoffee) readyForDelivery(ctx context.Context, o order.Order) error {
	drink := speech.SpeakableDrink(o.Drink)
	text := fmt.Sprintf("%s ready for delivery!", drink)
	if name := o.DisplayName(); name != "" {
		text = fmt.Sprintf("%s for %s, ready for delivery!", drink, name)
	}
	return s.speaker.SayAlways(ctx, text)
}
