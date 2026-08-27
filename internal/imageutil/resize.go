// Package imageutil holds small, dependency-free (pure stdlib, no cgo)
// image resizing helpers shared by more than one caller — today,
// internal/webui/avatar.go (square avatar thumbnails) and
// internal/agent_loop/attachments.go (chat attachment preview thumbnails).
// Kept as its own leaf package rather than duplicated in both call sites:
// per-pixel bilinear interpolation is exactly the kind of code where two
// copies drift apart over time, and neither internal/agent_loop nor
// internal/webui can depend on the other.
package imageutil

import (
	"bytes"
	"image"
	_ "image/gif" // image.Decode format registration
	"image/jpeg"
	"math"
	_ "image/png" // image.Decode format registration
)

// FitDimensions returns the largest width/height that fits srcW x srcH
// within maxPx on both axes while preserving aspect ratio, never upscaling
// — mirrors the web UI's own client-side thumbnail scale computation
// (static/js/screens/chat.js's thumbnailDataURL).
func FitDimensions(srcW, srcH, maxPx int) (w, h int) {
	scale := math.Min(float64(maxPx)/float64(srcW), float64(maxPx)/float64(srcH))
	if scale > 1 {
		scale = 1
	}
	w = int(math.Round(float64(srcW) * scale))
	h = int(math.Round(float64(srcH) * scale))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

// ThumbnailJPEG decodes data as an image (jpeg/png/gif), resizes it to fit
// within maxPx on both axes preserving aspect ratio, and re-encodes it as
// JPEG at the given quality (0-100). Returns an error for undecodable
// data — callers should treat that as "no thumbnail available" and fall
// back to whatever non-image chip they'd render anyway, not a hard failure.
func ThumbnailJPEG(data []byte, maxPx, quality int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := FitDimensions(b.Dx(), b.Dy(), maxPx)
	thumb := ResizeBilinear(src, w, h)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ResizeBilinear resamples src to exactly width x height using bilinear
// interpolation. Hand-rolled rather than pulling in golang.org/x/image/draw
// since every current caller only ever does a modest downscale (a
// center-cropped near-square avatar, or an already-reasonably-sized upload
// photo), where bilinear is indistinguishable from fancier filters — not
// worth a new dependency for.
func ResizeBilinear(src image.Image, width, height int) *image.RGBA {
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	for y := 0; y < height; y++ {
		srcY := (float64(y)+0.5)*float64(srcH)/float64(height) - 0.5
		y0, fy := splitFrac(srcY, srcH)
		for x := 0; x < width; x++ {
			srcX := (float64(x)+0.5)*float64(srcW)/float64(width) - 0.5
			x0, fx := splitFrac(srcX, srcW)

			c00 := rgba64At(src, bounds, x0, y0)
			c10 := rgba64At(src, bounds, x0+1, y0)
			c01 := rgba64At(src, bounds, x0, y0+1)
			c11 := rgba64At(src, bounds, x0+1, y0+1)

			dst.Set(x, y, bilerp(c00, c10, c01, c11, fx, fy))
		}
	}
	return dst
}
