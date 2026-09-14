// Package avatar handles the two uploaded images in stackr, a user's avatar
// and an organization's logo, from the multipart form to the <img> tag.
//
// Deliberately no resize or crop pipeline: browsers scale a 512px image
// perfectly well at the 20-40px these are rendered at, and the alternative is
// an image-decoding dependency plus a whole cropping UI for something nobody
// looks at closely. The size cap is what keeps that honest.
package avatar

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"

	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/google/uuid"
)

// MaxBytes caps an upload. Generous for an icon, small enough that a stray
// RAW photo is rejected rather than stored forever.
const MaxBytes = 2 << 20 // 2 MiB

// allowed maps the accepted content types to the extension we store under.
// Sniffed from the bytes, not trusted from the filename or the browser's
// Content-Type, both are attacker-controlled.
var allowed = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Accept is the input accept attribute, kept next to the allow-list so the two
// can't drift.
const Accept = "image/png,image/jpeg,image/gif,image/webp"

// Save validates and stores an uploaded image, returning its storage path.
// prefix separates the two kinds ("users", "orgs"); id keys the owner.
//
// The stored name carries a random suffix so a replaced image never reuses a
// URL a browser has already cached.
func Save(ctx context.Context, fs storage.FileStorage, prefix, id string, fh *multipart.FileHeader) (string, error) {
	if fh.Size > MaxBytes {
		return "", fmt.Errorf("image is larger than %d MB", MaxBytes>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	// Sniff the first 512 bytes, then rewind: DetectContentType only reads
	// magic numbers, so an .png that is really a script is caught here.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ext, ok := allowed[strings.SplitN(http.DetectContentType(head[:n]), ";", 2)[0]]
	if !ok {
		return "", fmt.Errorf("unsupported image type; use PNG, JPEG, GIF or WebP")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	p := path.Join(prefix, id+"-"+uuid.NewString()[:8]+ext)
	if err := fs.Save(ctx, p, io.LimitReader(f, MaxBytes)); err != nil {
		return "", err
	}
	return p, nil
}

// Replace saves a new image and deletes the one it supersedes. A failed
// delete is not an error, an orphaned file is a wasted inode, while a failed
// upload the user thinks succeeded is a bug.
func Replace(ctx context.Context, fs storage.FileStorage, prefix, id, old string, fh *multipart.FileHeader) (string, error) {
	p, err := Save(ctx, fs, prefix, id, fh)
	if err != nil {
		return "", err
	}
	if old != "" && old != p {
		_ = fs.Delete(ctx, old)
	}
	return p, nil
}

// URL is where a stored path is served from. Empty in, empty out, so callers
// can pass a possibly-unset AvatarPath straight through.
func URL(storagePath string) string {
	if storagePath == "" {
		return ""
	}
	return "/avatars/" + storagePath
}
