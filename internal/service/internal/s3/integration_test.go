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
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Against a throwaway RustFS container, the S3 server the managed tile runs
// (minio/minio is gone from Docker Hub).
func TestMinio(t *testing.T) {
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::9000",
		"-e", "RUSTFS_ACCESS_KEY=stackr", "-e", "RUSTFS_SECRET_KEY=stackr-secret",
		"rustfs/rustfs:latest").Output()
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
}
