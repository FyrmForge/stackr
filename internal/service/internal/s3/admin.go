package s3

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Admin cuts and drops buckets on an S3-compatible server (a managed S3
// instance) with its root credentials. Bucket is ignored.
type Admin Bucket

func (a Admin) c() *s3.Client { return Bucket(a).client() }

// Ping lists buckets: any answer is "ready".
func (a Admin) Ping(ctx context.Context) error {
	_, err := a.c().ListBuckets(ctx, &s3.ListBucketsInput{})
	return err
}

// CreateBucket is idempotent: a bucket that already exists is success.
func (a Admin) CreateBucket(ctx context.Context, name string) error {
	_, err := a.c().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	return err
}

// DropBucket empties the bucket, then removes it: S3 refuses to delete a
// bucket that holds objects, and this is the explicit destroy.
func (a Admin) DropBucket(ctx context.Context, name string) error {
	b := Bucket(a)
	b.Bucket = name
	keys, err := b.List(ctx, "")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := b.Delete(ctx, k); err != nil {
			return err
		}
	}
	_, err = a.c().DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
	var nb *types.NoSuchBucket
	if errors.As(err, &nb) {
		return nil
	}
	return err
}

// SetPublic sets or clears anonymous GetObject on the bucket.
func (a Admin) SetPublic(ctx context.Context, name string, public bool) error {
	if !public {
		_, err := a.c().DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(name)})
		return err
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",`+
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::%s/*"}]}`, name)
	_, err := a.c().PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(name), Policy: aws.String(policy)})
	return err
}
