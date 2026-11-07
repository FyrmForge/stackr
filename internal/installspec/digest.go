package installspec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DigestURL is where a release's digest asset sits (%s = the tag, v0.1.1).
const DigestURL = "https://github.com/FyrmForge/stackr/releases/download/%s/" + DigestAsset

// ErrNoDigest: the release has no digest asset (404).
var ErrNoDigest = errors.New("the release has no digest asset")

// ReleaseDigest reads the digest of release tag from the asset at format
// (DigestURL when ""). ErrNoDigest when there is no asset; any other error is
// a failed fetch or a malformed asset.
func ReleaseDigest(ctx context.Context, format, tag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if format == "" {
		format = DigestURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(format, tag), nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(res.Body, 256))
		if dg := strings.TrimSpace(string(b)); err == nil && ValidDigest(dg) {
			return dg, nil
		}
		return "", errors.New("the digest asset is malformed")
	case http.StatusNotFound:
		return "", ErrNoDigest
	}
	return "", fmt.Errorf("github answered %s", res.Status)
}
