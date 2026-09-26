package order

import "time"

// Reading is the per-attempt observability record the coffee service hands to
// the order sensor after each order finishes or fails. Grouped into a struct
// rather than positional args so new fields don't churn every call site.
type Reading struct {
	Order      Order
	ExecErr    error  // nil on success
	FailedStep string // step the order errored at; "" on success
	// OperatorCancelled is true when the failure was an operator cancel
	// (context.Canceled propagated from cancelCtx), not a genuine fault.
	// Filter these out of step error-rate metrics.
	OperatorCancelled bool
	TraceID           string // OTel trace ID; links the reading to the order's full trace
	// Decaf records whether the order took the decaf grinder branch, to explain
	// why a given step ran (or didn't) without cross-referencing config.
	Decaf     bool
	StartedAt time.Time
	EndedAt   time.Time
}
