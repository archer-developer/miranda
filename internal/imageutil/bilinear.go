package imageutil

import (
	"image"
	"image/color"
	"math"
)

// splitFrac splits a source-space coordinate into a clamped base index and
// its fractional offset (0..1) toward the next index, for bilinear
// sampling at the low/high edges of [0, size).
func splitFrac(v float64, size int) (int, float64) {
	base := int(math.Floor(v))
	frac := v - float64(base)
	if base < 0 {
		base, frac = 0, 0
	}
	if base > size-1 {
		base, frac = size-1, 0
	}
	return base, frac
}

func rgba64At(img image.Image, bounds image.Rectangle, x, y int) color.RGBA64 {
	if x >= bounds.Dx() {
		x = bounds.Dx() - 1
	}
	if y >= bounds.Dy() {
		y = bounds.Dy() - 1
	}
	r, g, b, a := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
}

func bilerp(c00, c10, c01, c11 color.RGBA64, fx, fy float64) color.RGBA {
	lerpU16 := func(a, b uint16, f float64) float64 {
		return float64(a) + (float64(b)-float64(a))*f
	}
	lerpF64 := func(a, b, f float64) float64 {
		return a + (b-a)*f
	}
	r := lerpF64(lerpU16(c00.R, c10.R, fx), lerpU16(c01.R, c11.R, fx), fy)
	g := lerpF64(lerpU16(c00.G, c10.G, fx), lerpU16(c01.G, c11.G, fx), fy)
	b := lerpF64(lerpU16(c00.B, c10.B, fx), lerpU16(c01.B, c11.B, fx), fy)
	a := lerpF64(lerpU16(c00.A, c10.A, fx), lerpU16(c01.A, c11.A, fx), fy)
	return color.RGBA{R: uint8(r / 257), G: uint8(g / 257), B: uint8(b / 257), A: uint8(a / 257)}
}
