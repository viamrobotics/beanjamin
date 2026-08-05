package coffee

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func faultReading(execErr error, cancelled bool) orderReading {
	return orderReading{
		order:             Order{ID: "order-1", Drink: "espresso", CustomerName: "Sam"},
		execErr:           execErr,
		operatorCancelled: cancelled,
	}
}

func TestReactToOrderFailure_RaisesTransientFaultFlag(t *testing.T) {
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), cfg: &Config{}, queue: NewOrderQueue()}
	if c.faultActive.Load() {
		t.Fatal("fault_active raised before any fault")
	}
	c.reactToOrderFailure(faultReading(errors.New("boom"), false))
	if !c.faultActive.Load() {
		t.Error("fault_active not raised after a genuine fault")
	}
	status, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if active, _ := status["fault_active"].(bool); !active {
		t.Errorf("Status fault_active = %v, want true", status["fault_active"])
	}
}

func TestReactToOrderFailure_SkipsSuccessAndOperatorCancel(t *testing.T) {
	for name, r := range map[string]orderReading{
		"success":          faultReading(nil, false),
		"operator_cancel":  faultReading(errors.New("boom"), true),
		"cancelled_no_err": faultReading(nil, true),
	} {
		t.Run(name, func(t *testing.T) {
			c := &beanjaminCoffee{logger: logging.NewTestLogger(t)}
			c.reactToOrderFailure(r)
			if c.faultActive.Load() {
				t.Error("fault_active raised for a non-fault outcome")
			}
		})
	}
}

func TestReactToOrderFailure_ClearsFlagAfterWindow(t *testing.T) {
	defer func(d time.Duration) { faultWindow = d }(faultWindow)
	faultWindow = 20 * time.Millisecond

	c := &beanjaminCoffee{logger: logging.NewTestLogger(t)}
	c.reactToOrderFailure(faultReading(errors.New("boom"), false))
	if !c.faultActive.Load() {
		t.Fatal("fault_active not raised after a genuine fault")
	}
	time.Sleep(50 * time.Millisecond)
	if c.faultActive.Load() {
		t.Error("fault_active still raised after the display window lapsed")
	}
}

func TestReactToOrderFailure_SpeaksSnarkOnGenuineFault(t *testing.T) {
	got := make(chan map[string]any, 1)
	speech := inject.NewGenericService("speech")
	speech.DoFunc = func(ctx context.Context, cmd map[string]any) (map[string]any, error) {
		got <- cmd
		return map[string]any{}, nil
	}
	c := &beanjaminCoffee{logger: logging.NewTestLogger(t), speech: speech}
	c.reactToOrderFailure(faultReading(errors.New("boom"), false))
	select {
	case cmd := <-got:
		line, ok := cmd["say_async"].(string)
		if !ok || line == "" {
			t.Fatalf("speech DoCommand missing say_async line: %v", cmd)
		}
		if !strings.Contains(line, "espresso") {
			t.Errorf("failure line %q does not mention the drink", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("speech DoCommand never called for a genuine fault")
	}
}

func TestPickOrderFailed(t *testing.T) {
	named := pickOrderFailed("decaf_lungo", "Sam")
	if !strings.Contains(named, "decaf lungo") {
		t.Errorf("named line %q does not mention the speakable drink", named)
	}
	if !strings.Contains(named, "Sam") {
		t.Errorf("named line %q does not mention the customer", named)
	}
	anon := pickOrderFailed("espresso", "")
	if !strings.Contains(anon, "espresso") {
		t.Errorf("anonymous line %q does not mention the drink", anon)
	}
}
