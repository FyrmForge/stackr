//go:build integration

package s3

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Against a throwaway RustFS container, the S3 server the managed tile runs.
func TestRustFS(t *testing.T) {
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::9000",
		"-e", "RUSTFS_ACCESS_KEY=stackr", "-e", "RUSTFS_SECRET_KEY=stackr-secret",
		"rustfs/rustfs:1.0.4").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		t.Fatalf("docker run rustfs: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port, err := exec.Command("docker", "port", id, "9000/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	b := Bucket{
		Endpoint:  "http://" + strings.TrimSpace(strings.Split(string(port), "\n")[0]),
		Bucket:    "backups",
		AccessKey: "stackr",
		SecretKey: "stackr-secret",
	}
	ctx := context.Background()
	for i := 0; ; i++ {
		_, err := b.client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b.Bucket)})
		if err == nil {
			break
		}
		if i == 60 {
			t.Fatalf("rustfs never came up: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	roundTrip(t, b)
	userGrants(t, b)
}

func denied(t *testing.T, what string, err error) {
	t.Helper()
	var re *awshttp.ResponseError
	if !errors.As(err, &re) || re.HTTPStatusCode() != 403 || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("%s: want 403 AccessDenied, got %v", what, err)
	}
}

// userGrants walks one IAM user through grant, re-grant and revoke.
func userGrants(t *testing.T, root Bucket) {
	ctx := context.Background()
	a := Admin(root)
	for _, n := range []string{"bucket-a", "bucket-b"} {
		if err := a.CreateBucket(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.AddUser(ctx, "appuser", "appuser-secret"); err != nil {
		t.Fatal(err)
	}
	if err := a.GrantUser(ctx, "appuser", "bucket-a", false); err != nil {
		t.Fatal(err)
	}
	user := func(bucket string) Bucket {
		return Bucket{Endpoint: root.Endpoint, Bucket: bucket, AccessKey: "appuser", SecretKey: "appuser-secret"}
	}
	ra, rb := root, root
	ra.Bucket, rb.Bucket = "bucket-a", "bucket-b"
	if err := ra.Put(ctx, "seed", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := rb.Put(ctx, "seed", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	ua, ub := user("bucket-a"), user("bucket-b")
	if _, err := ua.Get(ctx, "seed"); err != nil {
		t.Fatalf("read A: %v", err)
	}
	denied(t, "write A", ua.Put(ctx, "w", strings.NewReader("x")))
	_, err := ub.Get(ctx, "seed")
	denied(t, "read B", err)

	if err := a.GrantUser(ctx, "appuser", "bucket-a", true); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if err := ua.Put(ctx, "w", strings.NewReader("x")); err != nil {
		t.Fatalf("write A after re-grant: %v", err)
	}

	if err := a.RemoveUser(ctx, "appuser"); err != nil {
		t.Fatal(err)
	}
	if _, err := ua.Get(ctx, "seed"); err == nil {
		t.Fatal("removed user's key still works")
	}
	if err := a.RemoveUser(ctx, "appuser"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
