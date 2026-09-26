package coffee

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"beanjamin/coffee/order"
)

func TestEscapeSlackMrkdwn(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain name", "Jane Doe", "Jane Doe"},
		{"empty", "", ""},
		{"channel mention", "<!channel>", "&lt;!channel&gt;"},
		{"disguised link", "<https://example.com|Click here>", "&lt;https://example.com|Click here&gt;"},
		// & must become &amp; exactly once, including when it is already part
		// of an entity, or "&lt;" typed at the kiosk would render as "<".
		{"ampersand", "Tom & Jerry", "Tom &amp; Jerry"},
		{"pre-escaped entity", "&lt;", "&amp;lt;"},
		// Formatting characters are not control characters; Slack has no
		// escape for them, and they cannot ping anyone or forge a link.
		{"formatting left alone", "*bold* _it_ ~s~", "*bold* _it_ ~s~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeSlackMrkdwn(tc.in); got != tc.want {
				t.Errorf("escapeSlackMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A kiosk name is free text, so every place it reaches a failure notification
// (the Block Kit fields and the fallback text) must carry it escaped.
func TestSlackFailure_EscapesCustomerControlledValues(t *testing.T) {
	started := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	r := order.Reading{
		Order:      order.Order{ID: "order-1", Drink: "espresso", CustomerName: "<!channel> <https://example.com|Click here>"},
		ExecErr:    errors.New("peer said <!here>"),
		FailedStep: "brewing",
		StartedAt:  started,
		EndedAt:    started.Add(time.Minute),
	}

	raw, err := json.Marshal(slackFailureBlocks(r, "", "", ""))
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	// Decode the JSON escapes back so the assertions see the text Slack gets.
	var blocks []any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	var texts []string
	collectSlackText(blocks, &texts)
	payload := strings.Join(texts, "\n") + "\n" + slackFailureText(r)

	for _, forbidden := range []string{"<!channel>", "<https://example.com|", "<!here>"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("payload contains unescaped %q:\n%s", forbidden, payload)
		}
	}
	for _, want := range []string{"&lt;!channel&gt;", "&lt;!here&gt;"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload is missing escaped %q:\n%s", want, payload)
		}
	}
	// The footer's own <!date> markup is built here, not typed by a customer,
	// so it must survive escaping untouched.
	if !strings.Contains(payload, "<!date^") {
		t.Errorf("payload lost the footer's date markup:\n%s", payload)
	}
}

// collectSlackText gathers every "text" string in a decoded Block Kit payload.
func collectSlackText(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if s, ok := child.(string); ok && k == "text" {
				*out = append(*out, s)
				continue
			}
			collectSlackText(child, out)
		}
	case []any:
		for _, child := range x {
			collectSlackText(child, out)
		}
	}
}
