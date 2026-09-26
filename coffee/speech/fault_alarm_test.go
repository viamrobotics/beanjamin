package speech

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
)

func faultReading(execErr error, cancelled bool) order.Reading {
	return order.Reading{
		Order:             order.Order{ID: "order-1", Drink: "espresso", CustomerName: "Sam"},
		ExecErr:           execErr,
		OperatorCancelled: cancelled,
	}
}

func TestReactToOrderFailure_RaisesTransientFaultFlag(t *testing.T) {
	a := NewFaultAlarm(nil, logging.NewTestLogger(t))
	if a.Active() {
		t.Fatal("fault_active raised before any fault")
	}
	a.ReactToOrderFailure(faultReading(errors.New("boom"), false))
	if !a.Active() {
		t.Error("fault_active not raised after a genuine fault")
	}
}

func TestReactToOrderFailure_SkipsSuccessAndOperatorCancel(t *testing.T) {
	for name, r := range map[string]order.Reading{
		"success":          faultReading(nil, false),
		"operator_cancel":  faultReading(errors.New("boom"), true),
		"cancelled_no_err": faultReading(nil, true),
	} {
		t.Run(name, func(t *testing.T) {
			a := NewFaultAlarm(nil, logging.NewTestLogger(t))
			a.ReactToOrderFailure(r)
			if a.Active() {
				t.Error("fault_active raised for a non-fault outcome")
			}
		})
	}
}

func TestReactToOrderFailure_ClearsFlagAfterWindow(t *testing.T) {
	defer func(d time.Duration) { faultWindow = d }(faultWindow)
	faultWindow = 20 * time.Millisecond

	a := NewFaultAlarm(nil, logging.NewTestLogger(t))
	a.ReactToOrderFailure(faultReading(errors.New("boom"), false))
	if !a.Active() {
		t.Fatal("fault_active not raised after a genuine fault")
	}
	time.Sleep(50 * time.Millisecond)
	if a.Active() {
		t.Error("fault_active still raised after the display window lapsed")
	}
}

func TestReactToOrderFailure_SpeaksSnarkOnGenuineFault(t *testing.T) {
	got := make(chan map[string]any, 1)
	res := inject.NewGenericService("speech")
	res.DoFunc = func(ctx context.Context, cmd map[string]any) (map[string]any, error) {
		got <- cmd
		return map[string]any{}, nil
	}
	a := NewFaultAlarm(NewSpeaker(res, false), logging.NewTestLogger(t))
	a.ReactToOrderFailure(faultReading(errors.New("boom"), false))
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

func TestOrderFailed(t *testing.T) {
	named := OrderFailed("decaf_lungo", "Sam")
	if !strings.Contains(named, "decaf lungo") {
		t.Errorf("named line %q does not mention the speakable drink", named)
	}
	if !strings.Contains(named, "Sam") {
		t.Errorf("named line %q does not mention the customer", named)
	}
	anon := OrderFailed("espresso", "")
	if !strings.Contains(anon, "espresso") {
		t.Errorf("anonymous line %q does not mention the drink", anon)
	}
}
