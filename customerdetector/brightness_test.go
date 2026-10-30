package customerdetector

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// gradient is a size x size image whose brightness runs from 0 to peak, so its
// mean is about peak/2 and it has shadows and highlights to correct.
func gradient(size int, peak uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			v := uint8(float64(peak) * float64(x+y) / float64(2*size-2))
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func solid(size int, v uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
	}
	return img
}

func TestNormalizeBrightnessBringsDarkAndBrightFramesToTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		img  *image.RGBA
	}{
		{"dark", solid(32, 60)},
		{"dim", gradient(32, 120)},
		{"bright", solid(32, 200)},
	} {
		got, changed := normalizeBrightness(tc.img)
		if !changed {
			t.Errorf("%s: frame at %.0f was left alone", tc.name, meanLuma(tc.img))
			continue
		}
		if m := meanLuma(got); math.Abs(m-targetMeanLuma) > 2 {
			t.Errorf("%s: mean brightness %.1f after, want about %.0f", tc.name, m, targetMeanLuma)
		}
	}
}

func TestNormalizeBrightnessLeavesAFrameAtTheTargetAlone(t *testing.T) {
	img := solid(32, 128)
	got, changed := normalizeBrightness(img)
	if changed || got != image.Image(img) {
		t.Errorf("a frame already at the target was changed (changed=%v)", changed)
	}
}

func TestNormalizeBrightnessIsIdempotent(t *testing.T) {
	once, changed := normalizeBrightness(gradient(48, 110))
	if !changed {
		t.Fatal("first pass changed nothing")
	}
	if _, changed := normalizeBrightness(once); changed {
		t.Errorf("a second pass changed an already-normalized frame (mean %.2f)", meanLuma(once))
	}
}

func TestNormalizeBrightnessLeavesBlackAndWhiteAlone(t *testing.T) {
	for _, v := range []uint8{0, 255} {
		if _, changed := normalizeBrightness(solid(16, v)); changed {
			t.Errorf("an all-%d frame was changed; there is nothing to correct against", v)
		}
	}
}

func TestNormalizeBrightnessLimitsHowFarItCorrects(t *testing.T) {
	// A near-black frame needs a huge gamma to reach 128; the clamp stops it
	// being stretched all the way, which would only amplify noise.
	img := solid(16, 3)
	got, changed := normalizeBrightness(img)
	if !changed {
		t.Fatal("dark frame was left alone")
	}
	if m := meanLuma(got); m <= 3 || m >= targetMeanLuma-5 {
		t.Errorf("mean %.1f after: want brighter than 3 but held short of the target by the clamp", m)
	}
}

func TestNormalizeBrightnessHandlesAnImageNotAtTheOrigin(t *testing.T) {
	sub := solid(40, 50).SubImage(image.Rect(10, 10, 30, 30))
	got, changed := normalizeBrightness(sub)
	if !changed || got.Bounds().Dx() != 20 || got.Bounds().Dy() != 20 {
		t.Errorf("changed=%v bounds=%v, want a changed 20x20 image", changed, got.Bounds())
	}
}

func jpegBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNormalizeFacesRewritesDarkPhotosBacksThemUpAndIsRepeatable(t *testing.T) {
	cd := newTestDetector(t)
	recomputes := withVision(cd)
	darkBytes := jpegBytes(t, gradient(64, 150))
	brightBytes := jpegBytes(t, solid(64, 128))
	dir := seedFaces(t, cd, "alice@example.com", "Alice", nil)
	for name, b := range map[string][]byte{"dark.jpeg": darkBytes, "ok.jpeg": brightBytes} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := cd.normalizeFaces(context.Background())
	if err != nil {
		t.Fatalf("normalizeFaces: %v", err)
	}
	if res["normalized"] != 1 || res["unchanged"] != 1 || res["failed"] != 0 {
		t.Fatalf("result = %v, want 1 normalized, 1 unchanged, 0 failed", res)
	}
	if *recomputes != 1 {
		t.Errorf("recompute_embeddings ran %d times, want 1", *recomputes)
	}

	// The dark photo was rewritten at the target brightness...
	data, err := os.ReadFile(filepath.Join(dir, "dark.jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("rewritten photo is not a valid jpeg: %v", err)
	}
	if m := meanLuma(img); math.Abs(m-targetMeanLuma) > 4 {
		t.Errorf("rewritten photo mean brightness %.1f, want about %.0f", m, targetMeanLuma)
	}
	// ...the other was not touched...
	if got, _ := os.ReadFile(filepath.Join(dir, "ok.jpeg")); !bytes.Equal(got, brightBytes) {
		t.Error("a photo already at the target was rewritten")
	}
	// ...and the original is safe in the backup.
	backup := res["backup"].(string)
	if got, err := os.ReadFile(filepath.Join(backup, "alice@example.com", "dark.jpeg")); err != nil || !bytes.Equal(got, darkBytes) {
		t.Errorf("backup of the original missing or altered: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backup, "alice@example.com", "ok.jpeg")); !os.IsNotExist(err) {
		t.Error("an untouched photo was backed up")
	}
	if rel, _ := filepath.Rel(filepath.Join(cd.dataDir, knownFacesDir), backup); !filepath.IsAbs(rel) && rel != "" && rel[0] != '.' {
		t.Errorf("backup %q is inside known_faces, where the matcher would read it", backup)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("known_faces/alice has %d files after, want 2 (no leftover .tmp)", len(entries))
	}

	// A second run finds nothing to do, and rebuilds nothing.
	res, err = cd.normalizeFaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res["normalized"] != 0 {
		t.Errorf("second run normalized %v photos, want 0", res["normalized"])
	}
	if *recomputes != 1 {
		t.Errorf("second run recomputed embeddings (%d total), want it skipped", *recomputes)
	}
}

func TestNormalizeFacesReportsAnUnreadablePhotoAndKeepsGoing(t *testing.T) {
	cd := newTestDetector(t)
	withVision(cd)
	dir := seedFaces(t, cd, "alice@example.com", "Alice", map[string]string{"bad.jpeg": "not a jpeg"})
	if err := os.WriteFile(filepath.Join(dir, "dark.jpeg"), jpegBytes(t, gradient(64, 150)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := cd.normalizeFaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res["failed"] != 1 || res["normalized"] != 1 {
		t.Errorf("result = %v, want 1 failed and 1 normalized", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bad.jpeg")); string(got) != "not a jpeg" {
		t.Error("the unreadable photo was modified")
	}
}
