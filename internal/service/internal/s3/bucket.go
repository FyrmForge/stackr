package s3

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// defaultRegion is what an S3-compatible server that ignores regions still
// needs the signer to name.
const defaultRegion = "us-east-1"

// Bucket is an S3-compatible destination.
type Bucket struct {
	Endpoint  string // e.g. https://s3.eu-west-1.amazonaws.com, http://minio:9000
	Region    string // "" = us-east-1
	Bucket    string
	AccessKey string
	SecretKey string
}

func (b Bucket) client() *s3.Client {
	region := b.Region
	if region == "" {
		region = defaultRegion
	}
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(b.Endpoint),
		UsePathStyle: true, // MinIO/RustFS/Garage; real S3 accepts it too
		Credentials:  credentials.NewStaticCredentialsProvider(b.AccessKey, b.SecretKey, ""),
	})
}

// Put is one PutObject with the length known up front.
// ponytail: no multipart, so one archive tops out at S3's 5 GiB single-PUT
// limit; switch to the s3 manager uploader when a volume outgrows it.
func (b Bucket) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	size, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = b.client().PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.Bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	return err
}

func (b Bucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := b.client().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(key),
	})
	if nsk := (*types.NoSuchKey)(nil); errors.As(err, &nsk) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b Bucket) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(b.client(), &s3.ListObjectsV2Input{
		Bucket: aws.String(b.Bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// Delete: S3's DeleteObject already treats a missing key as success.
func (b Bucket) Delete(ctx context.Context, key string) error {
	_, err := b.client().DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(key),
	})
	return err
}
