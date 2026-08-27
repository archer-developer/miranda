package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // image.Decode format registration
	"image/jpeg"
	_ "image/png" // image.Decode format registration
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/archer-developer/miranda/internal/imageutil"
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

	thumb := imageutil.ResizeBilinear(cropToSquare(src), avatarSize, avatarSize)

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
