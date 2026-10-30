package customerdetector

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// targetMeanLuma is the average brightness (0-255) every face frame is brought
// to, so a dark enrolment and a bright visit, or two people photographed in
// different light, are compared on equal terms. Dark frames of different
// people look alike to the face matcher: on one kiosk's photos, faces under 65
// sat a median 0.20 from other people, against 0.58 for brighter ones.
const targetMeanLuma = 128.0

// Bounds on the gamma exponent, so an almost-black or blown-out frame is
// corrected a limited amount instead of being stretched into noise.
const (
	minGamma = 0.3
	maxGamma = 3.0
)

// gammaEpsilon is how close to 1 an exponent must be to leave the frame alone,
// so a frame already at the target (including one this function produced) is
// returned untouched rather than recompressed.
const gammaEpsilon = 0.02

// normalizeBrightness applies a gamma curve to img, chosen so that the frame's
// average brightness after the curve is targetMeanLuma. It reports whether it
// changed anything; when it did not, the original image is returned.
//
// The exponent is solved for the *result*, not read off the input's average:
// for a frame with shadows and highlights those differ, and a frame left a
// little off target would be corrected again, and recompressed, every time it
// passed through.
func normalizeBrightness(img image.Image) (image.Image, bool) {
	b := img.Bounds()
	if b.Empty() {
		return img, false
	}
	src, ok := img.(*image.RGBA)
	if !ok || src.Rect.Min != (image.Point{}) {
		rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
		src = rgba
	}

	// Per-channel histograms: brightness is linear in the channels, so the mean
	// brightness after any curve follows from them without touching the pixels.
	var hist [3][256]float64
	for y := 0; y < src.Rect.Dy(); y++ {
		row := src.Pix[y*src.Stride : y*src.Stride+src.Rect.Dx()*4]
		for x := 0; x < len(row); x += 4 {
			hist[0][row[x]]++
			hist[1][row[x+1]]++
			hist[2][row[x+2]]++
		}
	}
	pixels := float64(src.Rect.Dx() * src.Rect.Dy())
	weights := [3]float64{0.299, 0.587, 0.114}
	meanAfter := func(gamma float64) float64 {
		lut := gammaLUT(gamma)
		var mean float64
		for c := range hist {
			for v, n := range hist[c] {
				mean += weights[c] * n * float64(lut[v])
			}
		}
		return mean / pixels
	}

	// A frame with no light, or all of it, has nothing to correct against.
	if before := meanAfter(1); before < 1 || before > 254 {
		return img, false
	}

	// meanAfter falls as gamma rises, so bisect for the exponent that hits the
	// target, held within the clamp so a near-black or blown-out frame is
	// corrected a limited amount instead of being stretched into noise.
	lo, hi := minGamma, maxGamma
	for i := 0; i < 24; i++ {
		mid := (lo + hi) / 2
		if meanAfter(mid) > targetMeanLuma {
			lo = mid
		} else {
			hi = mid
		}
	}
	gamma := (lo + hi) / 2
	if math.Abs(gamma-1) < gammaEpsilon {
		return img, false
	}

	lut := gammaLUT(gamma)
	dst := image.NewRGBA(src.Rect)
	for y := 0; y < src.Rect.Dy(); y++ {
		in := src.Pix[y*src.Stride : y*src.Stride+src.Rect.Dx()*4]
		out := dst.Pix[y*dst.Stride : y*dst.Stride+dst.Rect.Dx()*4]
		for x := 0; x < len(in); x += 4 {
			out[x], out[x+1], out[x+2], out[x+3] = lut[in[x]], lut[in[x+1]], lut[in[x+2]], in[x+3]
		}
	}
	return dst, true
}

func gammaLUT(gamma float64) [256]uint8 {
	var lut [256]uint8
	for v := range lut {
		lut[v] = uint8(math.Round(255 * math.Pow(float64(v)/255, gamma)))
	}
	return lut
}

// meanLuma is the average brightness (0-255) of img, for tests.
func meanLuma(img image.Image) float64 {
	b := img.Bounds()
	var sum float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
			sum += 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
		}
	}
	return sum / float64(b.Dx()*b.Dy())
}
