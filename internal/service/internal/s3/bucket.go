package s3

import (
	"bytes"
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
		// The SDK's default trailing checksums are refused by older
		// S3-compatible servers; none is sent unless an operation needs one.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials:                credentials.NewStaticCredentialsProvider(b.AccessKey, b.SecretKey, ""),
	})
}

// partSize: S3 allows 10000 parts, so 16 MiB parts lift one object to about
// 160 GB; a stream holds one part (16 MiB) in memory.
const partSize = 16 << 20

// Put streams body up as a multipart upload, so the length need not be known
// and one archive is not capped at S3's 5 GiB single-PUT limit. A body that
// fits in one part is a plain PutObject. A failed upload is aborted, leaving
// no object and no stray parts.
func (b Bucket) Put(ctx context.Context, key string, body io.Reader) (err error) {
	c := b.client()
	buf := make([]byte, partSize)
	n, err := io.ReadFull(body, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		_, err = c.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(b.Bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(buf[:n]),
		})
		return err
	}
	if err != nil {
		return err
	}
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// The upload's own context may be the reason it failed.
			_, _ = c.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.Bucket),
				Key:      aws.String(key),
				UploadId: up.UploadId,
			})
		}
	}()

	var parts []types.CompletedPart
	for num := int32(1); ; num++ {
		out, perr := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(b.Bucket),
			Key:        aws.String(key),
			UploadId:   up.UploadId,
			PartNumber: aws.Int32(num),
			Body:       bytes.NewReader(buf[:n]),
		})
		if perr != nil {
			return perr
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
		if n < partSize {
			break
		}
		n, err = io.ReadFull(body, buf)
		if err == io.EOF {
			err = nil
			break // the body ended exactly on a part boundary
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return err
		}
		err = nil
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.Bucket),
		Key:             aws.String(key),
		UploadId:        up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
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
