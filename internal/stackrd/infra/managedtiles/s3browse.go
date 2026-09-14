package managedtiles

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// s3ClientFor dials the instance with its root creds, the browse-side
// counterpart of createBucket's inline client.
func (s *Service) s3ClientFor(ctx context.Context, instance *repo.Tile) (*s3.Client, error) {
	endpoint, err := s.reachInstance(ctx, instance)
	if err != nil {
		return nil, err
	}
	return s3.New(s3.Options{
		Region:       s3Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(instance.DBUser, instance.DBPassword, ""),
	}), nil
}

// S3Object is one entry of a bucket listing at a given prefix.
type S3Object struct {
	Name    string // last path segment
	Dir     bool   // a common prefix ("folder")
	Size    int64
	ModTime time.Time
}

// s3Prefix turns a browser dir path ("/", "/a/b") into an S3 key prefix
// ("", "a/b/").
func s3Prefix(dir string) string {
	p := strings.Trim(dir, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// S3List lists the objects and pseudo-folders directly under dir in the
// bucket, folders first.
func (s *Service) S3List(ctx context.Context, instance *repo.Tile, bucket, dir string) ([]S3Object, error) {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return nil, err
	}
	prefix := s3Prefix(dir)
	var out []S3Object
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", dir, err)
		}
		for _, cp := range page.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), prefix), "/")
			out = append(out, S3Object{Name: name, Dir: true})
		}
		for _, o := range page.Contents {
			name := strings.TrimPrefix(aws.ToString(o.Key), prefix)
			if name == "" {
				continue // the folder marker for the prefix itself
			}
			out = append(out, S3Object{Name: name, Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// S3Get streams one object out of the bucket.
func (s *Service) S3Get(ctx context.Context, instance *repo.Tile, bucket, key string) (io.ReadCloser, error) {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return nil, err
	}
	obj, err := cl.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(strings.Trim(key, "/")),
	})
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	return obj.Body, nil
}

// S3Put writes src as an object. src should be seekable (an uploaded form
// file is) so the SDK can size and sign the payload.
func (s *Service) S3Put(ctx context.Context, instance *repo.Tile, bucket, key string, src io.Reader) error {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return err
	}
	_, err = cl.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(strings.Trim(key, "/")), Body: src,
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// S3Delete removes one object, or a whole "folder" (key ending in /),
// object storage has no directories, so a folder delete is a prefix sweep.
func (s *Service) S3Delete(ctx context.Context, instance *repo.Tile, bucket, key string, dir bool) error {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return err
	}
	k := strings.Trim(key, "/")
	if !dir {
		if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(k)}); err != nil {
			return fmt.Errorf("delete %q: %w", key, err)
		}
		return nil
	}
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(k + "/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("delete %q: %w", key, err)
		}
		// one DeleteObject per key, DeleteObjects batching matters
		// only for huge folders, and RustFS support for it is unproven.
		for _, o := range page.Contents {
			if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err != nil {
				return fmt.Errorf("delete %q: %w", aws.ToString(o.Key), err)
			}
		}
	}
	return nil
}
