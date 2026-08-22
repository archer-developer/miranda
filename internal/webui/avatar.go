package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // image.Decode format registration
	"image/jpeg"
	_ "image/png" // image.Decode format registration
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

const (
	// maxAvatarUploadBytes is generous for an unedited phone photo — the
	// browser's own crop widget (static/js/avatar-crop.js) already shrinks
	// what it sends, this is just the server-side backstop.
	maxAvatarUploadBytes = 8 << 20
	// avatarSize is the fixed square dimension every uploaded avatar is
	// resized to on disk, regardless of what the client cropped to — see
	// handlePostAvatar.
	avatarSize = 128
)

// avatarFilePattern matches exactly the filenames handlePostAvatar writes:
// "<username>-<10 hex chars>.jpg". The greedy (.+) still isolates the
// username correctly even when it contains hyphens itself, since a regex
// engine backtracks (.+) until the fixed "-<hex>.jpg" suffix matches — that
// also keeps this from false-matching one user's file against a different
// user whose name happens to be a prefix (e.g. "bob" vs "bob-smith").
var avatarFilePattern = regexp.MustCompile(`^(.+)-[0-9a-f]{10}\.jpg$`)

// resolveAvatarFile returns the filename (not full path) of username's
// uploaded avatar in dir, or "" if none exists. Called on every page render
// (currentUserView) — dir only ever holds a handful of files, one per user
// (handlePostAvatar deletes the previous file on re-upload), so a directory
// scan is cheap enough to skip maintaining an index anywhere else.
func resolveAvatarFile(dir, username string) string {
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := avatarFilePattern.FindStringSubmatch(e.Name())
		if m != nil && m[1] == username {
			return e.Name()
		}
	}
	return ""
}

// handlePostAvatar decodes a multipart-uploaded image, center-crops it to a
// square, resizes it to avatarSize x avatarSize, and writes it into
// h.avatarsDir as "<username>-<content-hash>.jpg" — replacing any previous
// avatar file for this user. Resizing happens here rather than trusting the
// browser's own crop (static/js/avatar-crop.js) so a request that bypasses
// that UI entirely can never write something oversized or non-square to
// disk. The content hash (not a timestamp) both gives re-uploads a new URL
// — busting any browser cache of the old one — and is what
// avatarFilePattern/resolveAvatarFile key off of.
func (h *Handler) handlePostAvatar(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)

	if h.avatarsDir == "" {
		http.Error(w, "avatars disabled", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarUploadBytes)
	if err := r.ParseMultipartForm(maxAvatarUploadBytes); err != nil {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	file, _, err := r.FormFile("avatar")
	if err != nil {
		http.Error(w, "missing avatar file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	src, _, err := image.Decode(file)
	if err != nil {
		http.Error(w, "unrecognized image format", http.StatusBadRequest)
		return
	}

	thumb := resizeBilinear(cropToSquare(src), avatarSize, avatarSize)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 90}); err != nil {
		h.logger.Error("avatar: encode failed", "user", user.Username, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sum := sha256.Sum256(buf.Bytes())
	filename := fmt.Sprintf("%s-%s.jpg", user.Username, hex.EncodeToString(sum[:])[:10])

	if err := writeAvatarFile(h.avatarsDir, user.Username, filename, buf.Bytes()); err != nil {
		h.logger.Error("avatar: write failed", "user", user.Username, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"avatar_url": avatarURL(filename)})
}

// writeAvatarFile removes any existing avatar file(s) for username in dir,
// then writes data to filename via a temp-file-plus-rename so a concurrent
// page load (resolveAvatarFile scanning the directory, or the static file
// server reading the old file) never observes a partially written file.
func writeAvatarFile(dir, username, filename string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := avatarFilePattern.FindStringSubmatch(e.Name())
			if m != nil && m[1] == username {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	tmp, err := os.CreateTemp(dir, ".avatar-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, filename))
}

// cropToSquare returns the largest centered square crop of img. Uses
// SubImage where available (all of image/jpeg, image/png, image/gif's
// decoded types implement it) to avoid an extra full-image copy; the
// image/draw fallback only exists for exotic image.Image implementations
// that don't.
func cropToSquare(img image.Image) image.Image {
	b := img.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	rect := image.Rect(x0, y0, x0+side, y0+side)

	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := img.(subImager); ok {
		return si.SubImage(rect)
	}
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
	return dst
}

// resizeBilinear resamples src to exactly width x height using bilinear
// interpolation. Deliberately hand-rolled rather than pulling in
// golang.org/x/image/draw: the input is already center-cropped to a square
// close to avatarSize (client-side cropping in avatar-crop.js typically
// exports ~480px), so this only ever does a modest downscale, where
// bilinear is indistinguishable from fancier filters — not worth a new
// dependency for.
func resizeBilinear(src image.Image, width, height int) *image.RGBA {
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
