// Package iconsrc is the single source of the redrop mark. It describes the
// mark as a few geometric shapes and derives every icon file from them: the
// SVG text and anti-aliased PNG/ICO rasters, using only the standard library
// so the output is reproducible without external image tools.
//
// It is used by `go generate ./internal/brand` and by the test that checks the
// committed icons are current. The server binary does not import it.
package iconsrc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
)

// viewBox is the side of the square design space.
const viewBox = 64

// shape is a filled region of the design space.
type shape interface {
	contains(x, y float64) bool
	svg() string
}

type layer struct {
	shape shape
	fill  color.NRGBA
}

type roundRect struct{ x, y, w, h, r float64 }

func (s roundRect) contains(x, y float64) bool {
	if x < s.x || y < s.y || x > s.x+s.w || y > s.y+s.h {
		return false
	}
	cx := math.Max(s.x+s.r, math.Min(x, s.x+s.w-s.r))
	cy := math.Max(s.y+s.r, math.Min(y, s.y+s.h-s.r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= s.r*s.r
}

func (s roundRect) svg() string {
	if s.x == 0 && s.y == 0 {
		return fmt.Sprintf(`<rect width="%s" height="%s"%s`, num(s.w), num(s.h), rx(s.r))
	}
	return fmt.Sprintf(`<rect x="%s" y="%s" width="%s" height="%s"%s`, num(s.x), num(s.y), num(s.w), num(s.h), rx(s.r))
}

func rx(r float64) string {
	if r == 0 {
		return ""
	}
	return ` rx="` + num(r) + `"`
}

// drop is a circle of radius r at (cx, cy) joined by tangent lines to a tip
// directly above it at tipY.
type drop struct{ cx, cy, r, tipY float64 }

// tangents returns the points where the sides meet the circle.
func (d drop) tangents() (lx, rx, y float64) {
	theta := math.Acos(d.r / (d.cy - d.tipY))
	dx := d.r * math.Sin(theta)
	return d.cx - dx, d.cx + dx, d.cy - d.r*math.Cos(theta)
}

func (d drop) contains(x, y float64) bool {
	if (x-d.cx)*(x-d.cx)+(y-d.cy)*(y-d.cy) <= d.r*d.r {
		return true
	}
	lx, rx, ty := d.tangents()
	if y < d.tipY || y > ty {
		return false
	}
	// Within the triangle tip-left-right: interpolate the half width at y.
	half := (rx - lx) / 2 * (y - d.tipY) / (ty - d.tipY)
	return math.Abs(x-d.cx) <= half
}

func (d drop) svg() string {
	lx, rx, ty := d.tangents()
	return fmt.Sprintf(`<path d="M%s %sL%s %sA%s %s 0 1 1 %s %sZ"`,
		num(d.cx), num(d.tipY), num(rx), num(ty), num(d.r), num(d.r), num(lx), num(ty))
}

// num formats a coordinate with at most two decimals.
func num(f float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", f), "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func hex(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

var (
	ink   = color.NRGBA{0x17, 0x17, 0x17, 0xff} // dashboard --primary, oklch(0.205 0 0)
	paper = color.NRGBA{0xfa, 0xfa, 0xfa, 0xff} // dashboard --primary-foreground, oklch(0.985 0 0)
	mark  = drop{cx: 32, cy: 32, r: 10, tipY: 12}
	shelf = roundRect{16, 47, 32, 5, 2.5}
)

// design returns the layers of the mark, painted in order: a drop falling
// onto a shelf, on the dashboard's primary tile. rounded selects
// the rounded tile (browser tabs) or a full-bleed square (Apple touch icons,
// which iOS masks itself and which must not be transparent).
func design(rounded bool) []layer {
	radius := 0.0
	if rounded {
		radius = 14
	}
	return []layer{
		{roundRect{0, 0, viewBox, viewBox, radius}, ink},
		{mark, paper},
		{shelf, paper},
	}
}

// SVG renders the mark as a standalone SVG document.
func SVG() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d">`, viewBox, viewBox)
	for _, l := range design(true) {
		fmt.Fprintf(&b, `%s fill="%s"/>`, l.shape.svg(), hex(l.fill))
	}
	b.WriteString("</svg>\n")
	return []byte(b.String())
}

// samples is the supersampling grid per pixel side (samples² per pixel).
const samples = 16

// Raster renders the mark at size×size pixels with supersampled coverage.
func Raster(size int, rounded bool) *image.NRGBA {
	layers := design(rounded)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(viewBox) / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			// Accumulate premultiplied color over the subpixel grid.
			var r, g, b, a float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/samples) * scale
					y := (float64(py) + (float64(sy)+0.5)/samples) * scale
					for i := len(layers) - 1; i >= 0; i-- {
						if layers[i].shape.contains(x, y) {
							c := layers[i].fill
							r, g, b, a = r+float64(c.R), g+float64(c.G), b+float64(c.B), a+1
							break
						}
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(math.Round(r / a)), G: uint8(math.Round(g / a)), B: uint8(math.Round(b / a)),
				A: uint8(math.Round(a / (samples * samples) * 255)),
			})
		}
	}
	return img
}

// PNG encodes Raster(size, rounded).
func PNG(size int, rounded bool) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, Raster(size, rounded)); err != nil {
		panic(err) // encoding to memory cannot fail
	}
	return buf.Bytes()
}

// ICO packs PNG renderings of the rounded mark into one .ico file. PNG
// entries are supported by every browser and by Windows Vista and later.
func ICO(sizes ...int) []byte {
	images := make([][]byte, len(sizes))
	for i, s := range sizes {
		if s < 1 || s > 256 {
			panic(fmt.Sprintf("ico size %d out of range", s))
		}
		images[i] = PNG(s, true)
	}
	var buf bytes.Buffer
	le := binary.LittleEndian
	buf.Write(le.AppendUint16(le.AppendUint16(le.AppendUint16(nil, 0), 1), uint16(len(sizes)))) // ICONDIR
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := byte(s % 256) // 0 means 256
		entry := []byte{dim, dim, 0, 0}
		entry = le.AppendUint16(entry, 1)  // color planes
		entry = le.AppendUint16(entry, 32) // bits per pixel
		entry = le.AppendUint32(entry, uint32(len(images[i])))
		entry = le.AppendUint32(entry, uint32(offset))
		buf.Write(entry)
		offset += len(images[i])
	}
	for _, img := range images {
		buf.Write(img)
	}
	return buf.Bytes()
}

// Files returns every generated icon by file name.
func Files() map[string][]byte {
	return map[string][]byte{
		"favicon.svg":          SVG(),
		"favicon.ico":          ICO(16, 32, 48),
		"apple-touch-icon.png": PNG(180, false),
	}
}
