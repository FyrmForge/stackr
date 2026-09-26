package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// s3Region is fixed: RustFS ignores it, the SDK needs one.
const s3Region = "us-east-1"

// s3e is RustFS. Its image may ship no shell, so it works over the S3 and
// MinIO admin APIs with the instance's root credentials. A slice is a
// bucket; its own cred is the root one (RootCreds); every consumer gets an
// IAM user with one policy scoped to the bucket at its access (DECIDE 198).
type s3e struct{}

func (s3e) Definition() Definition {
	return Definition{
		Image:   "rustfs/rustfs:latest",
		Command: []string{"--console-enable", "/data"},
		Port:    9000,
		Volumes: []string{"/data"},
		Config: func(i Instance) []string {
			return []string{"RUSTFS_ACCESS_KEY=" + i.AdminUser, "RUSTFS_SECRET_KEY=" + i.AdminPassword}
		},
		PrimaryOutput: "S3_ENDPOINT",
		InjectAll:     true,
		PublicSlices:  true,
		SliceNoun:     "bucket",
		SliceName:     bucketName,
		SliceSep:      "-",
		RootCreds:     true,
	}
}

// bucketName is DNS-safe: lowercase, digits and hyphens, 3 to maxName chars.
func bucketName(slug string) string {
	n := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r == '_':
			return '-'
		}
		return -1
	}, strings.ToLower(slug))
	for len(n) < 3 {
		n += "0"
	}
	return n[:min(len(n), maxName)]
}

var errNoS3 = errors.New("s3 engine: no S3 client")

func (s3e) Ready(ctx context.Context, _ Instance, x Tools) error {
	if x.S3 == nil {
		return errNoS3
	}
	return x.S3.Ping(ctx)
}

func (s3e) Provision(ctx context.Context, i Instance, s Slice, x Tools) error {
	if x.S3 == nil {
		return errNoS3
	}
	if s.Public && i.PublicBase == "" {
		return errors.New("give the instance a public domain before making a bucket public")
	}
	if err := x.S3.CreateBucket(ctx, s.Name); err != nil {
		return err
	}
	return x.S3.SetPublic(ctx, s.Name, s.Public)
}

func (s3e) Drop(ctx context.Context, _ Instance, s Slice, x Tools) error {
	if x.S3 == nil {
		return errNoS3
	}
	return x.S3.DropBucket(ctx, s.Name)
}

// Bind mints the consumer's user on a first grant, then writes its policy;
// a re-grant (prev set) only rewrites the policy.
func (s3e) Bind(ctx context.Context, _ Instance, s Slice, g Grant, prev string, _ []Grant, x Tools) error {
	if x.S3 == nil {
		return errNoS3
	}
	if prev == "" {
		if err := x.S3.AddUser(ctx, g.User, g.Password); err != nil {
			return err
		}
	}
	return x.S3.GrantUser(ctx, g.User, s.Name, g.Access == "write")
}

func (s3e) Unbind(ctx context.Context, _ Instance, _ Slice, g Grant, x Tools) error {
	if x.S3 == nil {
		return errNoS3
	}
	return x.S3.RemoveUser(ctx, g.User)
}

func (s3e) Bindings(i Instance, s Slice) []Binding {
	endpoint, requiresNet := i.PublicBase, false
	if endpoint == "" {
		endpoint, requiresNet = fmt.Sprintf("http://%s:%d", i.Host, i.Port), true
	}
	out := []Binding{
		{"S3_ENDPOINT", endpoint, false, requiresNet},
		{"S3_BUCKET", s.Name, false, false},
		{"S3_REGION", s3Region, false, false},
		{"S3_ACCESS_KEY", s.User, true, false},
		{"S3_SECRET_KEY", s.Password, true, false},
	}
	if s.Public && i.PublicBase != "" {
		out = append(out, Binding{Name: "S3_PUBLIC_URL", Value: i.PublicBase + "/" + s.Name})
	}
	return out
}

// s3 restores by volume, not by dump.
func (s3e) Backup(string, Instance) []string {
	return nil
}

func (s3e) Restore(string, string, Instance) []string {
	return nil
}
