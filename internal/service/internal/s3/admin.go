package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Admin cuts and drops buckets and IAM users on an S3-compatible server (a
// operated S3 instance, RustFS) with its root credentials. Bucket is ignored.
// Users go over the RustFS admin API (DECIDE 198), signed like S3.
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

// DropBucket empties the bucket of every version and delete marker, then
// removes it: S3 refuses to delete a bucket that holds objects, and this is
// the explicit destroy. Purging versions is the root key's job alone. It
// lists the first page, deletes it and lists again from the start until
// nothing is left, so no page marker ever points at a deleted entry.
func (a Admin) DropBucket(ctx context.Context, name string) error {
	c := a.c()
	var nb *types.NoSuchBucket
	for {
		out, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(name)})
		if errors.As(err, &nb) {
			return nil
		}
		if err != nil {
			return err
		}
		n := 0
		del := func(key, id *string) error {
			n++
			in := &s3.DeleteObjectInput{Bucket: aws.String(name), Key: key}
			if aws.ToString(id) != "" {
				in.VersionId = id
			}
			_, err := c.DeleteObject(ctx, in)
			return err
		}
		for _, v := range out.Versions {
			if err := del(v.Key, v.VersionId); err != nil {
				return err
			}
		}
		for _, m := range out.DeleteMarkers {
			if err := del(m.Key, m.VersionId); err != nil {
				return err
			}
		}
		if n == 0 {
			break
		}
	}
	_, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
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

var adminHTTP = &http.Client{Timeout: 30 * time.Second}

// admin sends one signed request to the RustFS admin API. The /rustfs/admin/v3
// prefix takes plain JSON bodies. Network errors and 408/429/502/503 retry.
func (a Admin) admin(ctx context.Context, method, op string, q url.Values, body []byte) error {
	u, err := url.Parse(a.Endpoint)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("s3 admin: bad endpoint %q", a.Endpoint)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/rustfs/admin/v3/" + op
	u.RawQuery = q.Encode()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	creds := aws.Credentials{AccessKeyID: a.AccessKey, SecretAccessKey: a.SecretKey}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := signer.SignHTTP(ctx, creds, req, hash, "s3", defaultRegion, time.Now()); err != nil {
			return err
		}
		resp, err := adminHTTP.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		lastErr = &adminError{fmt.Sprintf("%s %s: %d: %s", method, op, resp.StatusCode, msg), resp.StatusCode, string(msg)}
		switch resp.StatusCode {
		case 408, 429, 502, 503:
		default:
			return lastErr
		}
	}
	return lastErr
}

type adminError struct {
	text   string
	status int
	body   string
}

func (e *adminError) Error() string { return e.text }

// gone reports a "nothing there" answer: RustFS 1.0.4 sends a missing policy
// as a 500 InternalError, so the message is the only tell.
func gone(err error) bool {
	var ae *adminError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.status == 404 || strings.Contains(ae.body, "does not exist") || strings.Contains(ae.body, "not found")
}

// AddUser makes an IAM user with that key pair; an existing one is re-keyed.
func (a Admin) AddUser(ctx context.Context, key, secret string) error {
	body, _ := json.Marshal(map[string]string{"secretKey": secret, "status": "enabled"})
	return a.admin(ctx, http.MethodPut, "add-user", url.Values{"accessKey": {key}}, body)
}

// GrantUser writes the user's one policy, named after it and scoped to the
// bucket, and attaches it. Rewriting the policy in place is how a user's
// access changes; RustFS enforces the new document at once.
func (a Admin) GrantUser(ctx context.Context, user, bucket string, write bool) error {
	policy := grantPolicy(bucket, write)
	if err := a.admin(ctx, http.MethodPut, "add-canned-policy", url.Values{"name": {user}}, []byte(policy)); err != nil {
		return err
	}
	attach, _ := json.Marshal(map[string]any{"policies": []string{user}, "user": user})
	return a.admin(ctx, http.MethodPost, "idp/builtin/policy/attach", nil, attach)
}

// grantPolicy never names s3:DeleteObjectVersion or any bucket-level write
// (versioning, lifecycle, CORS, policy): an app key can overwrite and delete
// current objects, which versioning keeps, but cannot purge history or turn
// the protection off. Only stackr's root key does that.
func grantPolicy(bucket string, write bool) string {
	onBucket := `"s3:GetBucketLocation","s3:ListBucket"`
	onObjects := `"s3:GetObject"`
	if write {
		onBucket += `,"s3:ListBucketMultipartUploads"`
		onObjects += `,"s3:PutObject","s3:DeleteObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"`
	}
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":[%s],"Resource":["arn:aws:s3:::%s"]},`+
		`{"Effect":"Allow","Action":[%s],"Resource":["arn:aws:s3:::%s/*"]}]}`,
		onBucket, bucket, onObjects, bucket)
}

// RemoveUser drops the user and its policy; one already gone is success.
func (a Admin) RemoveUser(ctx context.Context, user string) error {
	if err := a.admin(ctx, http.MethodDelete, "remove-user", url.Values{"accessKey": {user}}, nil); err != nil && !gone(err) {
		return err
	}
	if err := a.admin(ctx, http.MethodDelete, "remove-canned-policy", url.Values{"name": {user}}, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}
