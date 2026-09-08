package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// defaultOrgID is the viam-dev organization the beanjamin machines live in.
// Override with --org-id (or VIAM_ORG_ID) for another deployment.
const defaultOrgID = "e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb"

// orderSensorComponent is the component_name the coffee service's optional
// order sensor writes its per-attempt readings under (see coffee/order_sensor.go).
const orderSensorComponent = "order-events"

// Outcome labels. An order that isn't ok is either a genuine fault or an
// operator stopping the run, which is a distinction worth seeing in the table.
const (
	outcomeOK        = "OK"
	outcomeFailed    = "FAILED"
	outcomeCancelled = "CANCELLED"
)

// orderEvent is one order-events reading, shaped for the MQL projection in
// buildOrdersQuery rather than the raw tabular document.
type orderEvent struct {
	TimeReceived struct {
		Date time.Time `json:"$date"`
	} `json:"time_received"`
	Readings struct {
		OrderID           string  `json:"order_id"`
		Drink             string  `json:"drink"`
		CustomerName      string  `json:"customer_name"`
		OrderOK           bool    `json:"order_ok"`
		OperatorCancelled bool    `json:"operator_cancelled"`
		FailedStep        string  `json:"failed_step"`
		ErrorMessage      string  `json:"error_message"`
		DurationMs        float64 `json:"duration_ms"`
	} `json:"readings"`
}

// outcome collapses the two booleans into one label.
func (e orderEvent) outcome() string {
	switch {
	case e.Readings.OrderOK:
		return outcomeOK
	case e.Readings.OperatorCancelled:
		return outcomeCancelled
	default:
		return outcomeFailed
	}
}

func runOrders(args []string) error {
	flagSet := flag.NewFlagSet("orders", flag.ExitOnError)
	limit := flagSet.Int("limit", 20, "How many of the most recent orders to show")
	orgID := flagSet.String("org-id", envOr("VIAM_ORG_ID", defaultOrgID),
		"Viam organization ID (or set VIAM_ORG_ID)")
	partID := flagSet.String("part-id", "", "Only show orders from this machine part")
	newestFirst := flagSet.Bool("newest-first", false,
		"List newest first instead of in brew order")
	showErrors := flagSet.Bool("errors", false,
		"Print each unsuccessful order's error message under its row")
	viamBin := flagSet.String("viam", "viam", "Path to the viam CLI binary")

	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if *limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", *limit)
	}
	if *orgID == "" {
		return fmt.Errorf("--org-id is required (or set VIAM_ORG_ID)")
	}

	events, err := queryOrders(*viamBin, *orgID, *partID, *limit)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		scope := "org " + *orgID
		if *partID != "" {
			scope += ", part " + *partID
		}
		return fmt.Errorf("no orders found (%s)", scope)
	}
	// The query returns newest first; brew order is the reverse.
	if !*newestFirst {
		slices.Reverse(events)
	}
	fmt.Print(renderOrders(events, ordersView{
		numbered:   !*newestFirst,
		showErrors: *showErrors,
	}))
	return nil
}

// envOr returns the environment variable's value, or fallback when it is unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// buildOrdersQuery builds the MQL pipeline. It has to be MQL rather than SQL:
// `viam data query tabular sql` cannot resolve nested data.readings.* paths.
func buildOrdersQuery(partID string, limit int) (string, error) {
	match := map[string]any{"component_name": orderSensorComponent}
	if partID != "" {
		match["part_id"] = partID
	}
	pipeline := []map[string]any{
		{"$match": match},
		// Sorting before limiting is what makes this the *last* N orders.
		{"$sort": map[string]any{"time_received": -1}},
		{"$limit": limit},
		{"$project": map[string]any{
			"_id":           0,
			"time_received": 1,
			"readings":      "$data.readings",
		}},
	}
	encoded, err := json.Marshal(pipeline)
	if err != nil {
		return "", fmt.Errorf("encoding MQL pipeline: %w", err)
	}
	return string(encoded), nil
}

// queryOrders runs the MQL query through the viam CLI, which owns the
// credentials, and decodes its JSON-lines output newest-first.
func queryOrders(viamBin, orgID, partID string, limit int) ([]orderEvent, error) {
	mql, err := buildOrdersQuery(partID, limit)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(viamBin, "data", "query", "tabular", "mql",
		"--org-id", orgID, "--mql", mql)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// The CLI writes its update-check warning to stderr; pass it through rather
	// than mixing it into the rows we parse.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s data query tabular mql: %w", viamBin, err)
	}
	return parseOrders(stdout.Bytes())
}

// parseOrders decodes the CLI's JSON-lines output. Non-JSON lines are skipped:
// the CLI is free to interleave notices with the rows.
func parseOrders(out []byte) ([]orderEvent, error) {
	var events []orderEvent
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event orderEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decoding order event %q: %w", line, err)
		}
		events = append(events, event)
	}
	return events, nil
}

// ordersView is what the table shows beyond the default columns.
type ordersView struct {
	// numbered prefixes a 1-based brew index, which only means anything when
	// the rows are in brew order.
	numbered   bool
	showErrors bool
}

// formatDuration keeps short attempts legible. An order that died in 126ms and
// one that ran a full second are very different diagnostics, but both round to
// "0s" — and the instant ones are exactly the interesting failures.
func formatDuration(ms float64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	case ms < 10000:
		return fmt.Sprintf("%.1fs", ms/1000)
	default:
		return fmt.Sprintf("%.0fs", ms/1000)
	}
}

// renderOrders formats the table. When numbered, rows carry a 1-based brew
// index so the reader can refer to "#12" the way the timestamps don't allow.
func renderOrders(events []orderEvent, view ordersView) string {
	// The order ID is a column rather than an option: it is what makes a row
	// actionable (`fetch-order <id>`), and it is never truncated — a short
	// order ID matches no data, which looks exactly like an order having none.
	header := []string{"When (UTC)", "Result", "Drink", "Customer", "Dur", "Failed step", "Order ID"}
	if view.numbered {
		header = append([]string{"#"}, header...)
	}
	rows := make([][]string, 0, len(events))
	for i, event := range events {
		r := event.Readings
		customer := r.CustomerName
		if customer == "" {
			// An order placed outside the kiosk (operator or voice) has no name.
			customer = "—"
		}
		row := []string{
			event.TimeReceived.Date.UTC().Format("2006-01-02 15:04"),
			event.outcome(),
			r.Drink,
			customer,
			formatDuration(r.DurationMs),
			r.FailedStep,
			r.OrderID,
		}
		if view.numbered {
			row = append([]string{fmt.Sprint(i + 1)}, row...)
		}
		rows = append(rows, row)
	}

	widths := make([]int, len(header))
	for i, cell := range header {
		widths[i] = len([]rune(cell))
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)))
		}
	}

	var b strings.Builder
	writeRow(&b, header, widths)
	separator := make([]string, len(header))
	for i := range separator {
		separator[i] = strings.Repeat("-", widths[i])
	}
	writeRow(&b, separator, widths)
	for i, row := range rows {
		writeRow(&b, row, widths)
		if view.showErrors {
			if msg := events[i].Readings.ErrorMessage; msg != "" {
				fmt.Fprintf(&b, "    ↳ %s\n", msg)
			}
		}
	}
	fmt.Fprintf(&b, "\n%s\n", summarize(events))
	return b.String()
}

// writeRow writes one padded row, without trailing whitespace.
func writeRow(b *strings.Builder, cells []string, widths []int) {
	var line strings.Builder
	for i, cell := range cells {
		if i > 0 {
			line.WriteString("  ")
		}
		line.WriteString(cell)
		// Pad with the rune count, so the em dash and arrow stay aligned.
		line.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))))
	}
	b.WriteString(strings.TrimRight(line.String(), " "))
	b.WriteByte('\n')
}

// summarize is the one-line tally under the table.
func summarize(events []orderEvent) string {
	counts := map[string]int{}
	for _, event := range events {
		counts[event.outcome()]++
	}
	parts := []string{fmt.Sprintf("%d OK", counts[outcomeOK])}
	if counts[outcomeFailed] > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", counts[outcomeFailed]))
	}
	if counts[outcomeCancelled] > 0 {
		parts = append(parts, fmt.Sprintf("%d operator-cancelled", counts[outcomeCancelled]))
	}
	return strings.Join(parts, " · ")
}
