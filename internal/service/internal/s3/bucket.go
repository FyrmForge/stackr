package s3

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
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
		// The SDK's default trailing checksums are refused by older
		// S3-compatible servers; none is sent unless an operation needs one.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials:                credentials.NewStaticCredentialsProvider(b.AccessKey, b.SecretKey, ""),
	})
}

// partSize: S3 allows 10000 parts, so 16 MiB parts lift one object to about
// 160 GB; a stream holds concurrency x partSize in memory (80 MiB).
const partSize = 16 << 20

// Put is a multipart upload through the manager, so the length need not be
// known and one archive is not capped at S3's 5 GiB single-PUT limit. A
// failed upload is aborted, leaving no object and no stray parts.
func (b Bucket) Put(ctx context.Context, key string, body io.Reader) error {
	up := manager.NewUploader(b.client(), func(u *manager.Uploader) { //nolint:staticcheck // feature/s3/manager is deprecated for transfermanager; the agreed design names the manager
		u.PartSize = partSize
		u.LeavePartsOnError = false
	})
	_, err := up.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // same
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(key),
		Body:   body,
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
