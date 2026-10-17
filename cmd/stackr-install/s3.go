package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The S3 destination a restore reads from, by the names the CLI's
// `stackr dest add` already takes its keys from.
const (
	s3EndpointEnv  = "STACKR_BACKUP_ENDPOINT"
	s3RegionEnv    = "STACKR_BACKUP_REGION"
	s3AccessKeyEnv = "STACKR_BACKUP_ACCESS_KEY"
	s3SecretKeyEnv = "STACKR_BACKUP_SECRET_KEY"
)

// parseS3 splits s3://bucket/key. A key that is empty or ends in / is a
// prefix: the newest archive under it.
func parseS3(u string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(u, "s3://")
	if !ok {
		return "", "", fmt.Errorf("%q is not s3://bucket/key", u)
	}
	bucket, key, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("%q names no bucket", u)
	}
	return bucket, key, nil
}

// newestKey is the newest archive among keys: names start with a UTC
// timestamp, so the greatest base name is the newest, whatever trigger dir
// (scheduled, manual, pre-upgrade) it sits in.
func newestKey(keys []string) string {
	best := ""
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			continue
		}
		if best == "" || path.Base(k) > path.Base(best) {
			best = k
		}
	}
	return best
}

// fetchS3 downloads the archive at u into dir (0600) and returns its path
// and the key it read. The keys come from the environment, never a flag.
func fetchS3(ctx context.Context, u, dir string, getenv func(string) string) (string, string, error) {
	bucket, key, err := parseS3(u)
	if err != nil {
		return "", "", err
	}
	endpoint, ak, sk := getenv(s3EndpointEnv), getenv(s3AccessKeyEnv), getenv(s3SecretKeyEnv)
	if endpoint == "" || ak == "" || sk == "" {
		return "", "", fmt.Errorf("set %s, %s and %s to read from S3", s3EndpointEnv, s3AccessKeyEnv, s3SecretKeyEnv)
	}
	c := s3.New(s3.Options{
		Region:       cmp.Or(getenv(s3RegionEnv), "us-east-1"),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true, // MinIO/RustFS/Garage; real S3 accepts it too
		Credentials:  credentials.NewStaticCredentialsProvider(ak, sk, ""),
	})
	if key == "" || strings.HasSuffix(key, "/") {
		var keys []string
		p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return "", "", err
			}
			for _, o := range page.Contents {
				keys = append(keys, aws.ToString(o.Key))
			}
		}
		if key = newestKey(keys); key == "" {
			return "", "", fmt.Errorf("no archive under %s", u)
		}
	}
	obj, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return "", "", fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
	}
	defer func() { _ = obj.Body.Close() }()
	f, err := os.CreateTemp(dir, "restore-*.tar.gz.age")
	if err != nil {
		return "", "", err
	}
	_, err = io.Copy(f, obj.Body)
	err = errors.Join(err, f.Close())
	if err != nil {
		_ = os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), key, nil
}
