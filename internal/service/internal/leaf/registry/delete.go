package registry

import (
	"context"
	"fmt"
	"net/http"
)

// Delete removes one version: the manifest by digest at the registry (which
// drops every tag on it) and its row, together; the row stays when the
// registry call fails. Deleting what is already gone is not an error. Whether
// an env still uses the version is not decided here: the service checks that
// and warns first, and Clean asks its inUse. The blobs stay until the next
// garbage collect.
func (l *Leaf) Delete(ctx context.Context, repo, digest string) error {
	resp, err := l.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+digest)
	if err != nil {
		return fmt.Errorf("the registry did not answer: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("the registry refused the delete of %s@%s: %s", repo, digest, resp.Status)
	}
	return l.images.DeleteByRepoDigest(ctx, repo, digest)
}
