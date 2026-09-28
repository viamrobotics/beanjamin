package icevision

// Drawing the ice measurement onto the frame it was taken from.
//
// The measurement is bare pixel rows, and a row number says nothing about which
// way is up: "ice visible at row 606, stop row 595" reads like the glass is
// full when it is 11 px short, because rows grow downward. A picture settles
// that, and it is the only way to check a stop row against a real seating.
//
// One frame is drawn per hand-run ice action that asks for it; see
// SaveFrame. It draws the band the loop actually scanned and the
// readings the loop actually took, so the picture cannot describe a band or a
// measurement that nothing ran.

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Annotation colors. Red is the stop row, green what the contrast step
// measured, cyan the brightness shadow, gray the band's own edges.
var (
	annotStop    = color.RGBA{255, 60, 60, 255}
	annotSurface = color.RGBA{120, 255, 120, 255}
	annotShadow  = color.RGBA{90, 210, 255, 255}
	annotEdge    = color.RGBA{190, 190, 190, 255}
	annotText    = color.RGBA{255, 255, 255, 255}
)

// annotLeftMargin is the gutter every label starts at, and the bound a label
// too wide for the frame is pulled back to.
const annotLeftMargin = 20

// annotateFrame draws a measurement onto a copy of the frame it was taken
// from. It draws only: the readings come from the loop, so the picture and the
// caption beside it can never disagree about what was measured, and a saved
// frame costs no second pass over the pixels.
func annotateFrame(b Band, m Measurement, caption string) (*image.RGBA, error) {
	stopRow := b.Y0 + b.Window
	_, _, _, lastRow := ScanBand(stopRow, b.Window, b.Y1)

	out := image.NewRGBA(m.Frame.Bounds())
	draw.Draw(out, m.Frame.Bounds(), m.Frame, m.Frame.Bounds().Min, draw.Src)
	an := &annotator{img: out, width: out.Bounds().Max.X}
	if err := an.loadFonts(out.Bounds().Max.Y); err != nil {
		return nil, err
	}

	an.drawBand(b, lastRow)
	an.drawRow(stopRow, annotStop, fmt.Sprintf("row %d  STOP ROW — the pin closes once the surface is ABOVE this", stopRow))
	if m.Contrast.Found {
		an.drawReading(b, m.Contrast.Row, annotSurface, rightOfBand,
			fmt.Sprintf("contrast step: row %d (step %.0f)", m.Contrast.Row, m.Contrast.Step))
	}
	if m.Shadow && m.Brightness.Found {
		an.drawReading(b, m.Brightness.Row, annotShadow, leftOfBand,
			fmt.Sprintf("brightness shadow: row %d (%.0f)", m.Brightness.Row, m.Brightness.Step))
	}
	if caption != "" {
		an.label(an.big, annotLeftMargin, 34, caption, annotText)
	}
	return out, nil
}

// annotator draws onto one frame with type scaled to it.
type annotator struct {
	img       *image.RGBA
	width     int
	face, big font.Face
}

// loadFonts scales the type to the frame: the sizes were chosen against
// 1280x720 captures and a fixed size is unreadable on anything larger.
func (a *annotator) loadFonts(height int) error {
	parsed, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return fmt.Errorf("parsing the label font: %w", err)
	}
	for _, f := range []struct {
		dst   *font.Face
		scale float64
	}{{&a.face, 38}, {&a.big, 30}} {
		face, err := opentype.NewFace(parsed, &opentype.FaceOptions{
			Size: float64(height) / f.scale, DPI: 72, Hinting: font.HintingFull,
		})
		if err != nil {
			return fmt.Errorf("sizing the label font: %w", err)
		}
		*f.dst = face
	}
	return nil
}

// drawBand tints the rectangle the profile is averaged over and marks the
// lowest row the contrast step can nominate. That edge is what explains a "no
// surface" frame: a real surface below it is invisible to the step — and
// visible to the shadow, which is the comparison being drawn.
func (a *annotator) drawBand(b Band, lastRow int) {
	a.tint(b.X0, b.Y0, b.X1, b.Y1, color.RGBA{60, 140, 255, 255}, 0.18)
	a.vline(b.X0, b.Y0, b.Y1, annotShadow, 2)
	a.vline(b.X1-2, b.Y0, b.Y1, annotShadow, 2)

	a.hline(b.Y0, 0, a.width, annotEdge, 1, dashed)
	a.label(a.face, annotLeftMargin, b.Y0-8, fmt.Sprintf("row %d  band top (stop row − window)", b.Y0), annotEdge)
	a.hline(lastRow, 0, a.width, annotEdge, 1, dashed)
	a.label(a.face, a.width*3/4, lastRow+20, fmt.Sprintf("row %d  lowest row the step can report", lastRow), annotEdge)
	a.hline(b.Y1, 0, a.width, annotEdge, 1, dashed)
	a.label(a.face, a.width*3/4, b.Y1-8, fmt.Sprintf("row %d  band bottom", b.Y1), annotEdge)
}

// readingSide keeps the two methods' labels on opposite sides of the band, so
// they stay readable when the two rows nearly coincide.
type readingSide int

const (
	rightOfBand readingSide = iota
	leftOfBand
)

func (a *annotator) drawRow(row int, c color.RGBA, text string) {
	a.hline(row, 0, a.width, c, 3, solid)
	a.label(a.big, annotLeftMargin, row-10, text, c)
}

// drawReading marks a measured row inside the band only — it is a claim about
// the band, and a line across the whole frame would read as another threshold.
func (a *annotator) drawReading(b Band, row int, c color.RGBA, s readingSide, text string) {
	a.hline(row, b.X0-30, b.X1+30, c, 3, solid)
	if s == leftOfBand {
		w := font.MeasureString(a.face, text).Ceil()
		a.label(a.face, b.X0-36-w, row+6, text, c)
		return
	}
	a.label(a.face, b.X1+36, row+6, text, c)
}

type stroke bool

const (
	solid  stroke = false
	dashed stroke = true
)

func (a *annotator) hline(y, x0, x1 int, c color.Color, thick int, s stroke) {
	for t := 0; t < thick; t++ {
		for x := x0; x < x1; x++ {
			if s == dashed && (x/10)%2 == 1 {
				continue
			}
			a.img.Set(x, y+t, c)
		}
	}
}

func (a *annotator) vline(x, y0, y1 int, c color.Color, thick int) {
	for t := 0; t < thick; t++ {
		for y := y0; y < y1; y++ {
			a.img.Set(x+t, y, c)
		}
	}
}

func (a *annotator) tint(x0, y0, x1, y1 int, c color.RGBA, alpha float64) {
	blend := func(o, n uint8) uint8 { return uint8(float64(o)*(1-alpha) + float64(n)*alpha) }
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			o := a.img.RGBAAt(x, y)
			a.img.SetRGBA(x, y, color.RGBA{blend(o.R, c.R), blend(o.G, c.G), blend(o.B, c.B), 255})
		}
	}
}

// label draws text with a black outline, since every label crosses both the
// dark machine and the bright glass. A label placed too far right is pulled
// back into the frame rather than clipped: the labels are positioned to dodge
// each other, and their length depends on row numbers that are configuration.
func (a *annotator) label(face font.Face, x, y int, text string, c color.Color) {
	if w := font.MeasureString(face, text).Ceil(); x+w > a.width-annotLeftMargin {
		x = max(annotLeftMargin, a.width-annotLeftMargin-w)
	}
	for _, off := range [][2]int{{1, 1}, {-1, 1}, {1, -1}, {-1, -1}, {2, 2}} {
		d := &font.Drawer{Dst: a.img, Src: image.NewUniform(color.Black), Face: face, Dot: fixed.P(x+off[0], y+off[1])}
		d.DrawString(text)
	}
	d := &font.Drawer{Dst: a.img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, y)}
	d.DrawString(text)
}
