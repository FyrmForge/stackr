//go:build integration

package s3

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Against a throwaway MinIO container.
func TestMinio(t *testing.T) {
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER=stackr", "-e", "MINIO_ROOT_PASSWORD=stackr-secret",
		"minio/minio", "server", "/data").Output()
	if err != nil {
		t.Fatalf("docker run minio: %v", err)
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
			t.Fatalf("minio never came up: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	roundTrip(t, b)
}
