package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// DefaultVersionDays is how long old versions are kept when a slice says
// nothing.
const DefaultVersionDays = 7

// abortDays is how long an incomplete multipart upload may sit before the
// server drops its parts; always on.
const abortDays = 7

// Protect is a bucket's data-protection settings. VersionDays 0 = versioning
// off (suspended; old versions still age out after DefaultVersionDays).
// QuotaBytes 0 = no cap. CORS is the allowed origins, empty = no CORS.
type Protect struct {
	VersionDays int
	QuotaBytes  int64
	CORS        []string
}

// Protect makes the bucket match p: versioning, the lifecycle rules, quota
// and CORS. Every call writes all of it, so it is idempotent and a re-sync
// repairs drift. Only the root key (this Admin) may do any of it; an app key
// never holds the actions (see GrantUser).
func (a Admin) Protect(ctx context.Context, name string, p Protect) error {
	c := a.c()
	status := types.BucketVersioningStatusSuspended
	days := DefaultVersionDays
	if p.VersionDays > 0 {
		status, days = types.BucketVersioningStatusEnabled, p.VersionDays
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(name),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	}); err != nil {
		return fmt.Errorf("versioning: %w", err)
	}
	// One lifecycle document holds all three rules: a PUT replaces the lot.
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(name),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{
			{
				ID: aws.String("abort-incomplete-uploads"), Status: types.ExpirationStatusEnabled,
				Filter:                         &types.LifecycleRuleFilter{Prefix: aws.String("")},
				AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(abortDays)},
			},
			{
				ID: aws.String("expire-old-versions"), Status: types.ExpirationStatusEnabled,
				Filter:                      &types.LifecycleRuleFilter{Prefix: aws.String("")},
				NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(int32(days))},
			},
			{
				ID: aws.String("expire-delete-markers"), Status: types.ExpirationStatusEnabled,
				Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
				Expiration: &types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
			},
		}},
	}); err != nil {
		return fmt.Errorf("lifecycle: %w", err)
	}
	if err := a.setQuota(ctx, name, p.QuotaBytes); err != nil {
		return fmt.Errorf("quota: %w", err)
	}
	if err := a.setCORS(ctx, name, p.CORS); err != nil {
		return fmt.Errorf("cors: %w", err)
	}
	return nil
}

func (a Admin) setQuota(ctx context.Context, name string, bytes int64) error {
	body, _ := json.Marshal(map[string]any{"quota": bytes, "quotatype": "HARD"})
	return a.admin(ctx, http.MethodPut, "set-bucket-quota", url.Values{"bucket": {name}}, body)
}

func (a Admin) setCORS(ctx context.Context, name string, origins []string) error {
	if len(origins) == 0 {
		_, err := a.c().DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(name)})
		var ae interface{ ErrorCode() string }
		if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchCORSConfiguration" {
			return nil
		}
		return err
	}
	_, err := a.c().PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(name),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			AllowedOrigins: origins,
			AllowedMethods: []string{"GET", "PUT", "POST", "DELETE", "HEAD"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(3600),
		}}},
	})
	return err
}

// entry is one version or delete marker of a key.
type entry struct {
	Key      string
	ID       string
	Modified time.Time
	Marker   bool
	Latest   bool
	Size     int64
}

// versions walks every version and delete marker under prefix and hands each
// page to fn. A bucket that is gone yields nothing.
func (a Admin) versions(ctx context.Context, name, prefix string, fn func([]entry) error) error {
	p := s3.NewListObjectVersionsPaginator(a.c(), &s3.ListObjectVersionsInput{
		Bucket: aws.String(name), Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		var page []entry
		for _, v := range out.Versions {
			page = append(page, entry{aws.ToString(v.Key), aws.ToString(v.VersionId), aws.ToTime(v.LastModified), false, aws.ToBool(v.IsLatest), aws.ToInt64(v.Size)})
		}
		for _, m := range out.DeleteMarkers {
			page = append(page, entry{aws.ToString(m.Key), aws.ToString(m.VersionId), aws.ToTime(m.LastModified), true, aws.ToBool(m.IsLatest), 0})
		}
		if err := fn(page); err != nil {
			return err
		}
	}
	return nil
}

// Sizes is the bytes of current objects and of old versions, apart.
func (a Admin) Sizes(ctx context.Context, name string) (current, old int64, err error) {
	err = a.versions(ctx, name, "", func(page []entry) error {
		for _, e := range page {
			if e.Latest {
				current += e.Size
			} else {
				old += e.Size
			}
		}
		return nil
	})
	return current, old, err
}

// pick is the version of one key that was current at t: the newest entry at
// or before t. ok is false when there is nothing to copy: the key did not
// exist then (no entry, or a delete marker was newest). ambiguous is a
// version and a delete marker with the same timestamp that neither is the
// latest of: the listing gives no order between them, so the caller leaves
// the key alone. The listed IsLatest flag is only an ordering hint; whether
// the version is still current is checked live before copying. Restore
// never deletes.
func pick(es []entry, t time.Time) (e entry, ok, ambiguous bool) {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].Modified.Equal(es[j].Modified) {
			return es[i].Modified.After(es[j].Modified)
		}
		return es[i].Latest && !es[j].Latest
	})
	for i, c := range es {
		if c.Modified.After(t) {
			continue
		}
		if i+1 < len(es) && es[i+1].Modified.Equal(c.Modified) && es[i+1].Marker != c.Marker && !c.Latest {
			return c, false, true
		}
		return c, !c.Marker, false
	}
	return entry{}, false, false
}

// current is the version id a key serves now; "" when there is none (the key
// is deleted or gone).
func (a Admin) current(ctx context.Context, name, key string) (string, error) {
	out, err := a.c().HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(name), Key: aws.String(key)})
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return "", nil
	}
	return aws.ToString(out.VersionId), err
}

// copyMax is the largest object one CopyObject moves.
const copyMax = 5 << 30

// RestoreResult counts what Restore did. Err is the first failure, if any.
type RestoreResult struct {
	Restored, Skipped, Failed int
	Err                       error
}

// Restore copies the version of every key under prefix that was current at
// t back on top as a new version. exact limits it to the key named prefix
// itself. Keys that did not exist at t are left alone: it never deletes.
// It is per key, not atomic: a failure on one key is counted and the rest
// go on. progress, if set, is told after each key.
// ponytail: collects every version under prefix in memory and copies one key
// at a time; a bucket past a few million versions wants paging by key. An
// object over 5 GiB fails (CopyObject's cap); multipart UploadPartCopy later.
func (a Admin) Restore(ctx context.Context, name, prefix string, exact bool, t time.Time, progress func(RestoreResult)) (RestoreResult, error) {
	by := map[string][]entry{}
	if err := a.versions(ctx, name, prefix, func(page []entry) error {
		for _, e := range page {
			if !exact || e.Key == prefix {
				by[e.Key] = append(by[e.Key], e)
			}
		}
		return nil
	}); err != nil {
		return RestoreResult{}, err
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var r RestoreResult
	c := a.c()
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		e, ok, amb := pick(by[k], t)
		cur := ""
		if ok && !amb {
			var err error
			if cur, err = a.current(ctx, name, k); err != nil {
				r.Failed++
				r.Err = cmpErr(r.Err, fmt.Errorf("%s: %w", k, err))
				continue
			}
		}
		switch {
		case amb:
			r.Failed++
			r.Err = cmpErr(r.Err, fmt.Errorf("%s: a version and a delete marker share a timestamp, left alone", k))
		case !ok, cur == e.ID:
			r.Skipped++
		case e.Size > copyMax:
			r.Failed++
			r.Err = cmpErr(r.Err, fmt.Errorf("%s: over 5 GiB, too large to restore", k))
		default:
			src := (&url.URL{Path: name + "/" + k}).EscapedPath() + "?versionId=" + url.QueryEscape(e.ID)
			if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket: aws.String(name), Key: aws.String(k), CopySource: aws.String(src),
			}); err != nil {
				r.Failed++
				r.Err = cmpErr(r.Err, fmt.Errorf("%s: %w", k, err))
			} else {
				r.Restored++
			}
		}
		if progress != nil {
			progress(r)
		}
	}
	return r, nil
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}
