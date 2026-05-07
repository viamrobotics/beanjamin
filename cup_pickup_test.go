package beanjamin

import (
	"strings"
	"testing"

	"github.com/golang/geo/r3"
)

func TestSelectCupCentroid_Empty(t *testing.T) {
	_, _, err := selectCupCentroid(nil, r3.Vector{}, 100)
	if err == nil {
		t.Fatalf("expected error on empty input")
	}
}

func TestSelectCupCentroid_SingleInRange(t *testing.T) {
	c := []r3.Vector{{X: 110, Y: 0, Z: 0}}
	got, idx, err := selectCupCentroid(c, r3.Vector{X: 100, Y: 0, Z: 0}, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected index 0, got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected centroid %v, got %v", c[0], got)
	}
}

func TestSelectCupCentroid_SingleOutOfRange(t *testing.T) {
	c := []r3.Vector{{X: 1000, Y: 0, Z: 0}}
	_, _, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
}

func TestSelectCupCentroid_PicksClosest(t *testing.T) {
	c := []r3.Vector{
		{X: 200, Y: 0, Z: 0}, // 100mm from target — farther
		{X: 110, Y: 0, Z: 0}, // 10mm from target — closer
		{X: 150, Y: 0, Z: 0}, // 50mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 300)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected centroid %v, got %v", c[1], got)
	}
}

func TestSelectCupCentroid_AllOutOfRange(t *testing.T) {
	c := []r3.Vector{
		{X: 1000, Y: 0, Z: 0},
		{X: 2000, Y: 0, Z: 0},
	}
	_, _, err := selectCupCentroid(c, r3.Vector{}, 100)
	if err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("expected 'within' error, got %v", err)
	}
}

func TestSelectCupCentroid_ZeroMaxMeansNoCutoff(t *testing.T) {
	c := []r3.Vector{
		{X: 1e6, Y: 0, Z: 0},
		{X: 100, Y: 0, Z: 0},
	}
	got, idx, err := selectCupCentroid(c, r3.Vector{}, 0)
	if err != nil {
		t.Fatalf("expected no error with maxDistMm=0, got %v", err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if got != c[1] {
		t.Fatalf("expected closest centroid, got %v", got)
	}
}

func TestSelectCupCentroid_TieBreaksFirst(t *testing.T) {
	c := []r3.Vector{
		{X: 110, Y: 0, Z: 0},
		{X: 90, Y: 0, Z: 0}, // both 10mm from target
	}
	target := r3.Vector{X: 100, Y: 0, Z: 0}
	got, idx, err := selectCupCentroid(c, target, 50)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idx != 0 {
		t.Fatalf("expected first-wins (index 0), got %d", idx)
	}
	if got != c[0] {
		t.Fatalf("expected first centroid, got %v", got)
	}
}
