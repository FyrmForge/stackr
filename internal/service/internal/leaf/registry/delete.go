package registry

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Delete removes one version: the manifest by digest at the registry (which
// drops every tag on it) and its row, together. Deleting what is already gone
// is not an error. Whether an env still uses the version is not decided here:
// the service checks that and warns first. The blobs stay until the next
// garbage collect.
func (l *Leaf) Delete(ctx context.Context, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, l.base+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("the registry did not answer: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("the registry refused the delete: %s", resp.Status)
	}
	return l.images.DeleteByRepoDigest(ctx, repo, digest)
}
