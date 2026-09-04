package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// event builds one order event the way the MQL projection delivers it.
func event(at, drink, customer string, ok, cancelled bool, step, errMsg string, durMs float64) orderEvent {
	var e orderEvent
	parsed, err := time.Parse("2006-01-02 15:04", at)
	if err != nil {
		panic(err)
	}
	e.TimeReceived.Date = parsed
	e.Readings.OrderID = "oid-" + at[len(at)-5:]
	e.Readings.Drink = drink
	e.Readings.CustomerName = customer
	e.Readings.OrderOK = ok
	e.Readings.OperatorCancelled = cancelled
	e.Readings.FailedStep = step
	e.Readings.ErrorMessage = errMsg
	e.Readings.DurationMs = durMs
	return e
}

func TestOutcome(t *testing.T) {
	cases := []struct {
		name          string
		ok, cancelled bool
		want          string
	}{
		{name: "success", ok: true, want: outcomeOK},
		{name: "fault", want: outcomeFailed},
		{name: "operator stopped it", cancelled: true, want: outcomeCancelled},
		// order_ok wins: a cancel that still produced a drink is a success.
		{name: "ok beats cancelled", ok: true, cancelled: true, want: outcomeOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := event("2026-09-04 13:00", "espresso", "", c.ok, c.cancelled, "", "", 1000)
			if got := e.outcome(); got != c.want {
				t.Errorf("outcome = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildOrdersQuery(t *testing.T) {
	// $sort before $limit is what makes the result the *last* N orders rather
	// than an arbitrary N, so assert the stage order, not just the contents.
	raw, err := buildOrdersQuery("", 20)
	if err != nil {
		t.Fatal(err)
	}
	var pipeline []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &pipeline); err != nil {
		t.Fatalf("query is not valid JSON: %v", err)
	}
	var stages []string
	for _, stage := range pipeline {
		for key := range stage {
			stages = append(stages, key)
		}
	}
	want := []string{"$match", "$sort", "$limit", "$project"}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", stages, want)
	}
	if !strings.Contains(raw, `"component_name":"`+orderSensorComponent+`"`) {
		t.Errorf("query does not match on the order sensor: %s", raw)
	}
	if strings.Contains(raw, "part_id") {
		t.Errorf("no part filter was requested, but query has one: %s", raw)
	}

	withPart, err := buildOrdersQuery("part-42", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withPart, `"part_id":"part-42"`) {
		t.Errorf("query missing the part filter: %s", withPart)
	}
	if !strings.Contains(withPart, `"$limit":5`) {
		t.Errorf("query missing the limit: %s", withPart)
	}
}

func TestParseOrders(t *testing.T) {
	// Real CLI output shape, with a notice line the parser must skip and the
	// key order the server actually returns (which is not the struct's).
	out := `Warning: CLI Update Check: Your CLI is out of date.
{"r_unused":1,"time_received":{"$date":"2026-09-04T13:58:53.14Z"},"readings":{"drink":"iced_latte","order_ok":false,"operator_cancelled":true,"failed_step":"Opening fridge","customer_name":"","order_id":"6e1ce9e4-2c0f-4fa0-8bdb-c13e6f12f698","duration_ms":164197.0,"error_message":"add_milk: boom"}}

{"time_received":{"$date":"2026-09-04T13:49:12.14Z"},"readings":{"drink":"espresso","order_ok":true,"customer_name":"Adam","order_id":"47b90796-1d45-4ab8-bf84-7f2886dfacc8","duration_ms":199091.0}}
`
	events, err := parseOrders([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if got := events[0].outcome(); got != outcomeCancelled {
		t.Errorf("event 0 outcome = %q, want %q", got, outcomeCancelled)
	}
	if got := events[0].Readings.OrderID; got != "6e1ce9e4-2c0f-4fa0-8bdb-c13e6f12f698" {
		t.Errorf("event 0 order id = %q", got)
	}
	if got := events[1].Readings.CustomerName; got != "Adam" {
		t.Errorf("event 1 customer = %q, want Adam", got)
	}
	if got := events[1].TimeReceived.Date.UTC().Format("2006-01-02 15:04"); got != "2026-09-04 13:49" {
		t.Errorf("event 1 time = %q", got)
	}
}

func TestParseOrdersRejectsMalformedRow(t *testing.T) {
	// A row that looks like JSON but isn't must not be silently dropped —
	// a short table would read as "these are all the orders".
	if _, err := parseOrders([]byte(`{"time_received":`)); err == nil {
		t.Error("expected an error for a truncated row")
	}
}

func TestRenderOrders(t *testing.T) {
	events := []orderEvent{
		event("2026-09-04 13:16", "espresso", "", true, false, "", "", 201683),
		event("2026-09-04 13:20", "espresso", "", false, false, "Serving", "grab: boom", 121763),
		event("2026-09-04 13:58", "iced_latte", "Adam", false, true, "Opening fridge", "add_milk: boom", 164197),
	}

	got := renderOrders(events, ordersView{numbered: true})
	want := strings.Join([]string{
		"#  When (UTC)        Result     Drink       Customer  Dur   Failed step     Order ID",
		"-  ----------------  ---------  ----------  --------  ----  --------------  ---------",
		"1  2026-09-04 13:16  OK         espresso    —         202s                  oid-13:16",
		"2  2026-09-04 13:20  FAILED     espresso    —         122s  Serving         oid-13:20",
		"3  2026-09-04 13:58  CANCELLED  iced_latte  Adam      164s  Opening fridge  oid-13:58",
		"",
		"1 OK · 1 failed · 1 operator-cancelled",
		"",
	}, "\n")
	if got != want {
		t.Errorf("table:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		// The instant failures are the interesting ones; "0s" would hide them.
		{ms: 126, want: "126ms"},
		{ms: 160, want: "160ms"},
		{ms: 999, want: "999ms"},
		{ms: 1200, want: "1.2s"},
		// 2050ms is a real cancelled-grind duration; 2.05 rounds down in float64.
		{ms: 2050, want: "2.0s"},
		{ms: 11839, want: "12s"},
		{ms: 201683, want: "202s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.ms); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestRenderOrdersNewestFirstDropsIndex(t *testing.T) {
	events := []orderEvent{event("2026-09-04 13:16", "espresso", "", true, false, "", "", 1000)}
	got := renderOrders(events, ordersView{numbered: false})
	if strings.HasPrefix(got, "#") {
		t.Errorf("unnumbered table should not have an index column:\n%s", got)
	}
}

func TestRenderOrdersErrorLines(t *testing.T) {
	events := []orderEvent{
		event("2026-09-04 13:16", "espresso", "", true, false, "", "", 1000),
		event("2026-09-04 13:20", "espresso", "", false, false, "Serving", "grab: boom", 2000),
	}

	got := renderOrders(events, ordersView{numbered: true, showErrors: true})
	if !strings.Contains(got, "↳ grab: boom") {
		t.Errorf("missing the error line:\n%s", got)
	}
	// A successful order has no error message, so it gets no extra line.
	if strings.Count(got, "↳") != 1 {
		t.Errorf("expected exactly one error line:\n%s", got)
	}
}

// TestRenderOrdersAlwaysShowsFullID guards the column that makes a row
// actionable: a truncated order ID matches no data when fed to fetch-order.
func TestRenderOrdersAlwaysShowsFullID(t *testing.T) {
	const id = "975420ff-002c-40d9-8f05-50d97fb044c9"
	e := event("2026-09-04 13:16", "espresso", "", true, false, "", "", 1000)
	e.Readings.OrderID = id
	for _, view := range []ordersView{{numbered: true}, {numbered: false}} {
		got := renderOrders([]orderEvent{e}, view)
		if !strings.Contains(got, "Order ID") || !strings.Contains(got, id) {
			t.Errorf("view %+v dropped or truncated the order ID:\n%s", view, got)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("BEANJAMIN_TEST_ENV", "")
	if got := envOr("BEANJAMIN_TEST_ENV", "fallback"); got != "fallback" {
		t.Errorf("empty env: got %q, want fallback", got)
	}
	t.Setenv("BEANJAMIN_TEST_ENV", "set")
	if got := envOr("BEANJAMIN_TEST_ENV", "fallback"); got != "set" {
		t.Errorf("set env: got %q, want set", got)
	}
}
