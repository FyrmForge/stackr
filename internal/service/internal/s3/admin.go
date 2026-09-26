package s3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Admin cuts and drops buckets and IAM users on an S3-compatible server (a
// managed S3 instance, RustFS) with its root credentials. Bucket is ignored.
// Users go over the MinIO admin API, which RustFS serves too (DECIDE 198).
type Admin Bucket

func (a Admin) c() *s3.Client { return Bucket(a).client() }

func (a Admin) adm() (*madmin.AdminClient, error) {
	u, err := url.Parse(a.Endpoint)
	if err != nil {
		return nil, err
	}
	return madmin.NewWithOptions(u.Host, &madmin.Options{
		Creds:  credentials.NewStaticV4(a.AccessKey, a.SecretKey, ""),
		Secure: u.Scheme == "https",
	})
}

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

// AddUser makes an IAM user with that key pair; an existing one is re-keyed.
func (a Admin) AddUser(ctx context.Context, key, secret string) error {
	adm, err := a.adm()
	if err != nil {
		return err
	}
	return adm.AddUser(ctx, key, secret)
}

// GrantUser writes the user's one policy, named after it and scoped to the
// bucket, and attaches it. Rewriting the policy in place is how a user's
// access changes; RustFS enforces the new document at once.
func (a Admin) GrantUser(ctx context.Context, user, bucket string, write bool) error {
	adm, err := a.adm()
	if err != nil {
		return err
	}
	onBucket := `"s3:GetBucketLocation","s3:ListBucket"`
	onObjects := `"s3:GetObject"`
	if write {
		onBucket += `,"s3:ListBucketMultipartUploads"`
		onObjects += `,"s3:PutObject","s3:DeleteObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"`
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":[%s],"Resource":["arn:aws:s3:::%s"]},`+
		`{"Effect":"Allow","Action":[%s],"Resource":["arn:aws:s3:::%s/*"]}]}`,
		onBucket, bucket, onObjects, bucket)
	if err := adm.AddCannedPolicy(ctx, user, []byte(policy)); err != nil {
		return err
	}
	_, err = adm.AttachPolicy(ctx, madmin.PolicyAssociationReq{Policies: []string{user}, User: user})
	return err
}

// RemoveUser drops the user and its policy; a user already gone is success.
func (a Admin) RemoveUser(ctx context.Context, user string) error {
	adm, err := a.adm()
	if err != nil {
		return err
	}
	// RustFS answers a missing user with a generic InternalError, so the
	// message is the only tell.
	if err := adm.RemoveUser(ctx, user); err != nil && !strings.Contains(err.Error(), "does not exist") {
		return err
	}
	return adm.RemoveCannedPolicy(ctx, user)
}
