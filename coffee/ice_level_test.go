package coffee

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	rdkutils "go.viam.com/rdk/utils"
)

func loadFixture(t *testing.T, name string) image.Image {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "icelevel", name))
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	return img
}

// TestIceSurfaceRowFixtures pins the measurement against the captured glasses.
// With the shipping band the answer is not a level but a side: a surface found
// below the stop row is "not yet", and no surface at all means either "no ice
// yet" or "risen past the stop row" — which is why the loop, not the
// measurement, carries the latch that tells those apart.
func TestIceSurfaceRowFixtures(t *testing.T) {
	b := iceBandFromConfig(&Config{})
	for _, tc := range []struct {
		name    string
		fixture string
		found   bool
		row     int
	}{
		// Seating A (rim row 277): hand-poured levels.
		{"empty", "fill_0.jpg", false, 0},
		{"30 percent, under the ledge", "fill_30.jpg", false, 0},
		{"40 percent, below the stop row", "fill_40.jpg", true, 595},
		{"80 percent, past the stop row", "fill_80.jpg", false, 0},
		{"full, past the stop row", "fill_100.jpg", false, 0},
		// Seating B (rim row 379, a real fetch_glass grab, 102 px lower). The
		// empty case is the one that matters: the rim is a stronger step than any
		// ice surface, and it must not be mistaken for one.
		{"empty at the low seating", "rim379_empty.jpg", false, 0},
		{"rising, still below the stop row", "rim379_rising.jpg", true, 595},
		{"risen past the stop row", "rim379_passed.jpg", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := iceSurfaceRow(loadFixture(t, tc.fixture), b)
			if err != nil {
				t.Fatalf("iceSurfaceRow: %v", err)
			}
			if got.found != tc.found {
				t.Fatalf("found = %v (row %d, step %.1f), want %v", got.found, got.row, got.step, tc.found)
			}
			if got.found && got.row != tc.row {
				t.Errorf("row = %d, want %d", got.row, tc.row)
			}
			if got.found && got.row < b.y0+b.window {
				t.Errorf("row %d is above the stop row %d — the band must not nominate one", got.row, b.y0+b.window)
			}
		})
	}
}

// TestIceSurfaceRowIgnoresBrightness is the test the design exists for. An
// absolute-brightness cutoff reads a brightly-lit empty glass as full, which
// fails toward serving a drink with a second of ice in it. The contrast step
// must hold across the lighting range.
func TestIceSurfaceRowIgnoresBrightness(t *testing.T) {
	b := iceBandFromConfig(&Config{})
	for _, fixture := range []string{"fill_0.jpg", "rim379_empty.jpg"} {
		for _, scale := range []float64{0.5, 1.4, 1.8} {
			t.Run(fmt.Sprintf("%s at %.1fx", fixture, scale), func(t *testing.T) {
				got, err := iceSurfaceRow(scaleLuminance(loadFixture(t, fixture), scale), b)
				if err != nil {
					t.Fatalf("iceSurfaceRow: %v", err)
				}
				if got.found {
					t.Errorf("empty glass at %.1fx brightness reported ice at row %d (step %.1f), want no surface",
						scale, got.row, got.step)
				}
			})
		}
	}
	// The other half of the same claim: a real surface survives the dim end,
	// where the step shrinks for want of contrast.
	got, err := iceSurfaceRow(scaleLuminance(loadFixture(t, "fill_40.jpg"), 0.5), b)
	if err != nil {
		t.Fatalf("iceSurfaceRow: %v", err)
	}
	if !got.found {
		t.Error("a 40 percent full glass at half brightness reported no surface, want one")
	}
}

// TestIceSurfaceRowExcludesTheRim encodes the defense directly: a step as strong
// as any rim, placed above the band, is not reported. Only its position keeps it
// out — rim steps (55-90 in the fixtures) overlap real ice surfaces (33-58), so
// no magnitude test could separate them.
func TestIceSurfaceRowExcludesTheRim(t *testing.T) {
	b := iceBandFromConfig(&Config{})
	for _, edge := range []int{100, 300, b.y0 - 1} {
		img := stepImage(1280, 720, edge, 20, 200)
		got, err := iceSurfaceRow(img, b)
		if err != nil {
			t.Fatalf("iceSurfaceRow: %v", err)
		}
		if got.found {
			t.Errorf("a step at row %d (above the band top %d) was reported at row %d", edge, b.y0, got.row)
		}
	}
	// The same step inside the band is found, so the test above is about where
	// it sits and not about the step being invisible.
	got, err := iceSurfaceRow(stepImage(1280, 720, 600, 20, 200), b)
	if err != nil {
		t.Fatalf("iceSurfaceRow: %v", err)
	}
	if !got.found || got.row != 600 {
		t.Errorf("step inside the band: found=%v row=%d, want true 600", got.found, got.row)
	}
}

// TestIceScanBandReportsTheStopRowFirst pins the identity the band is built
// around: the topmost row the scan can nominate is the stop row itself. Break
// it — by deriving the band top from anything other than the stop row — and the
// scan starts reaching above the stop row, where the glass rim lives.
func TestIceScanBandReportsTheStopRowFirst(t *testing.T) {
	for _, tc := range []struct{ stopRow, window, roiY1 int }{
		{defaultIceStopRowPx, defaultIceContrastWindow, defaultIceROIY1},
		{400, 20, 500},
		{900, 64, 1080},
	} {
		top, bottom, firstRow, lastRow := iceScanBand(tc.stopRow, tc.window, tc.roiY1)
		if firstRow != tc.stopRow {
			t.Errorf("stop row %d, window %d: first reportable row %d, want the stop row", tc.stopRow, tc.window, firstRow)
		}
		if top != tc.stopRow-tc.window || bottom != tc.roiY1 || lastRow != tc.roiY1-tc.window {
			t.Errorf("stop row %d, window %d, y1 %d: band %d..%d reporting %d..%d",
				tc.stopRow, tc.window, tc.roiY1, top, bottom, firstRow, lastRow)
		}
	}

	// The config path has to agree with it, or the exported helper describes a
	// band nothing scans.
	b := iceBandFromConfig(&Config{})
	top, bottom, _, _ := iceScanBand(defaultIceStopRowPx, defaultIceContrastWindow, defaultIceROIY1)
	if b.y0 != top || b.y1 != bottom {
		t.Errorf("config band rows %d..%d, iceScanBand %d..%d", b.y0, b.y1, top, bottom)
	}
}

// TestIceSurfaceRowRejectsWrongResolution: the rows are pixels at the
// resolution they were measured at, so a reconfigured camera has to fail loudly
// rather than measure a different part of the glass.
func TestIceSurfaceRowRejectsWrongResolution(t *testing.T) {
	if _, err := iceSurfaceRow(image.NewRGBA(image.Rect(0, 0, 640, 480)), iceBandFromConfig(&Config{})); err == nil {
		t.Error("measuring a 640x480 frame with 1280x720 bounds succeeded, want an error")
	}
}

func TestStrongestStepAboveBandFindsTheRim(t *testing.T) {
	b := iceBandFromConfig(&Config{})
	for _, tc := range []struct {
		fixture string
		row     int
	}{
		{"fill_0.jpg", 278},
		{"rim379_empty.jpg", 379},
	} {
		got, err := strongestStepAboveBand(loadFixture(t, tc.fixture), b)
		if err != nil {
			t.Fatalf("strongestStepAboveBand: %v", err)
		}
		if !got.found || got.row != tc.row {
			t.Errorf("%s: found=%v row=%d, want true %d", tc.fixture, got.found, got.row, tc.row)
		}
	}
}

// scaleLuminance multiplies every channel and clamps, standing in for brighter
// or dimmer ambient light. Clamping is deliberate: saturated ice clipping is
// why the step shrinks at the bright end too.
func scaleLuminance(src image.Image, scale float64) image.Image {
	bounds := src.Bounds()
	out := image.NewRGBA(bounds)
	clamp := func(v float64) uint8 {
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			out.Set(x, y, color.RGBA{
				R: clamp(float64(r>>8) * scale),
				G: clamp(float64(g>>8) * scale),
				B: clamp(float64(bl>>8) * scale),
				A: uint8(a >> 8),
			})
		}
	}
	return out
}

// stepImage builds a frame that is dark above edge and bright below it — a
// synthetic brightness step at a known row.
func stepImage(w, h, edge int, dark, bright uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		v := dark
		if y >= edge {
			v = bright
		}
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// TestValidateIceVisionRejectsSilentGeometry: every case here would otherwise
// run, log nothing unusual, and either stop the dispense immediately or never
// stop it at all.
func TestValidateIceVisionRejectsSilentGeometry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutfn func(*Config)
		want  string
	}{
		{"transposed x pair", func(c *Config) { c.IceROIX0, c.IceROIX1 = 910, 700 }, "ice_roi_x0"},
		{"stop row above the window", func(c *Config) { c.IceStopRowPx = 10 }, "ice_stop_row_px"},
		{"band bottom inside the window", func(c *Config) { c.IceROIY1 = 600 }, "ice_roi_y1"},
		{"negative contrast", func(c *Config) { c.IceMinContrast = -1 }, "must not be negative"},
		{"negative stop row", func(c *Config) { c.IceStopRowPx = -565 }, "must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCanServeIcedConfig()
			tc.mutfn(cfg)
			_, _, err := cfg.Validate("path")
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestCheckIceLevelRegisteredWithIced(t *testing.T) {
	iced := (&beanjaminCoffee{cfg: &Config{CanServeIced: true}}).actionFuncs()
	if _, ok := iced["check_ice_level"]; !ok {
		t.Error("check_ice_level is not registered on an iced machine")
	}
	plain := (&beanjaminCoffee{cfg: &Config{}}).actionFuncs()
	if _, ok := plain["check_ice_level"]; ok {
		t.Error("check_ice_level is registered on a machine that cannot serve iced drinks")
	}
}

// The band is checked on every machine, not only the ones serving iced drinks:
// ice_roi_* is reachable config either way, and a band that scans nothing has to
// be rejected rather than measuring silently.
func TestValidateIceVisionAppliesWithoutIced(t *testing.T) {
	cfg := validBaseConfig()
	cfg.IceROIX0, cfg.IceROIX1 = 910, 700

	_, _, err := cfg.Validate("path")
	if err == nil {
		t.Fatal("Validate accepted a transposed ice band on a machine without can_serve_iced")
	}
	if !strings.Contains(err.Error(), "ice_roi_x0") {
		t.Errorf("error %q does not mention ice_roi_x0", err)
	}
}

// The defaults have to survive their own validator, and the band they derive
// has to be able to nominate the stop row itself.
func TestValidateIceVisionAcceptsDefaults(t *testing.T) {
	if _, _, err := validCanServeIcedConfig().Validate("path"); err != nil {
		t.Fatalf("Validate rejected the defaults: %v", err)
	}
	b := iceBandFromConfig(&Config{})
	if b.y0+b.window != defaultIceStopRowPx {
		t.Errorf("band top %d + window %d = %d, want the stop row %d", b.y0, b.window, b.y0+b.window, defaultIceStopRowPx)
	}
	if b.y0+2*b.window >= b.y1 {
		t.Errorf("band y %d..%d cannot reach the stop row with window %d", b.y0, b.y1, b.window)
	}
}

// TestFirstDecodableImageNeedsTheColorSource: the depth payload reads as a
// perfectly good brightness profile, so picking it would produce a stop row out
// of nothing. A camera serving one stream is the ordinary case and is taken
// whatever it calls it; only an ambiguous response is refused.
func TestFirstDecodableImageNeedsTheColorSource(t *testing.T) {
	named := func(t *testing.T, source string) camera.NamedImage {
		t.Helper()
		ni, err := camera.NamedImageFromImage(stepImage(16, 16, 8, 20, 200), source, rdkutils.MimeTypePNG, data.Annotations{})
		if err != nil {
			t.Fatalf("named image: %v", err)
		}
		return ni
	}

	for _, tc := range []struct {
		name    string
		images  []string
		wantErr bool
	}{
		{"one unnamed stream", []string{""}, false},
		{"color among several", []string{"depth", iceColorSourceName}, false},
		{"several, none of them color", []string{"depth", "rgb"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			images := make([]camera.NamedImage, 0, len(tc.images))
			for _, source := range tc.images {
				images = append(images, named(t, source))
			}
			_, err := firstDecodableImage(context.Background(), images)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("firstDecodableImage = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}
