package coffee

import (
	"testing"

	"github.com/golang/geo/r3"
)

// TestContainerDimensionsBoxDims verifies the held-item box extents: a square
// footprint of the diameter, and the container's height.
func TestContainerDimensionsBoxDims(t *testing.T) {
	d := &ContainerDimensions{DiameterMm: 70, HeightMm: 140}
	if got, want := d.boxDims(), (r3.Vector{X: 70, Y: 70, Z: 140}); got != want {
		t.Errorf("boxDims() = %v, want %v", got, want)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault(0, 5); got != 5 {
		t.Errorf("orDefault(0, 5) = %d, want 5", got)
	}
	if got := orDefault(3, 5); got != 3 {
		t.Errorf("orDefault(3, 5) = %d, want 3", got)
	}
	if got := orDefault(-2, 5); got != 5 {
		t.Errorf("orDefault(-2, 5) = %d, want 5 (non-positive falls to default)", got)
	}
	if got := orDefault(2.5, 1.0); got != 2.5 {
		t.Errorf("orDefault(2.5, 1.0) = %v, want 2.5", got)
	}
}
