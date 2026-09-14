package managedtiles

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// s3Region is fixed, RustFS ignores it but the S3 SDK requires one.
const s3Region = "us-east-1"

// s3Provision cuts a bucket for the consumer in a RustFS instance and publishes
// the S3 connection secrets. shared-root creds (the instance's own
// access/secret key), one bucket per consumer is the isolation boundary until
// RustFS's admin API is proven enough for scoped keys.
func s3Provision(s *Service, ctx context.Context, instance, consumer *repo.Tile, name string, public bool) (*repo.Provision, error) {
	// A public bucket is only useful if browsers can reach the instance, which
	// means it must carry a public domain, the source of the public base URL.
	if public && s.instancePublicBase(ctx, instance) == "" {
		return nil, fmt.Errorf("instance %s needs a public domain before a public bucket (add one with `tile <instance> domain add`)", instance.Slug)
	}
	endpoint, err := s.reachInstance(ctx, instance)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	base := consumer.Slug
	if name != "" {
		base = name
	}
	bucket := uniqueSliceName(base, existing, "-")
	if err := s.createBucket(ctx, endpoint, instance, bucket); err != nil {
		return nil, err
	}
	if public {
		if err := s.putPublicReadPolicy(ctx, endpoint, instance, bucket); err != nil {
			return nil, err
		}
	}
	p := &repo.Provision{
		ID:             uuid.NewString(),
		InstanceTileID: instance.ID,
		ConsumerTileID: consumer.ID,
		EnvID:          consumer.EnvironmentID,
		DBName:         bucket,              // the bucket
		DBUser:         instance.DBUser,     // shared root access key (v1)
		DBPassword:     instance.DBPassword, // shared root secret key (v1)
		SecretName:     sliceSecret(instance, consumer),
		Status:         "active",
		Public:         public,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.store.CreateProvision(ctx, p); err != nil {
		return nil, err
	}
	if err := s.SyncResource(ctx, instance, p); err != nil {
		return nil, err
	}
	return p, nil
}

// s3Drop empties the bucket and removes it. S3 refuses to delete a bucket that
// still holds objects, and this is the explicit "destroy this bucket" action,
// so its contents go with it.
// s3Fork copies every object of a bucket into one ForkSlice has already
// created empty on the same instance. Server-side: CopyObject keeps the bytes
// inside the storage server rather than pulling them through stackr, and the
// shared-root credential model means there is no per-role work either side.
func s3Fork(s *Service, ctx context.Context, instance *repo.Tile, src, dst *repo.Provision) error {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return err
	}
	pages := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(src.DBName)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing %q: %w", src.DBName, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			// CopySource is bucket and key as one path that the server URL-
			// decodes, so the key has to be encoded per segment: escaping the
			// whole string would turn its slashes into %2F, and leaving it raw
			// copies a key containing a space to the wrong name.
			source := src.DBName + "/" + (&url.URL{Path: key}).EscapedPath()
			if _, err := cl.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:     aws.String(dst.DBName),
				Key:        aws.String(key),
				CopySource: aws.String(source),
			}); err != nil {
				return fmt.Errorf("copying %s/%s: %w", src.DBName, key, err)
			}
		}
	}
	return nil
}

func s3Drop(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision) error {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return err
	}
	pages := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: aws.String(p.DBName)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing %q: %w", p.DBName, err)
		}
		for _, obj := range page.Contents {
			if _, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(p.DBName), Key: obj.Key}); err != nil {
				return fmt.Errorf("deleting %s/%s: %w", p.DBName, aws.ToString(obj.Key), err)
			}
		}
	}
	if _, err := cl.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(p.DBName)}); err != nil {
		return fmt.Errorf("delete bucket %q: %w", p.DBName, err)
	}
	return nil
}

// s3Ready probes the instance from the outside, the container may not even
// ship a shell.
func s3Ready(s *Service, ctx context.Context, instance *repo.Tile, _ string) error {
	cl, err := s.s3ClientFor(ctx, instance)
	if err != nil {
		return err
	}
	if _, err := cl.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		return fmt.Errorf("s3 not ready: %w", err)
	}
	return nil
}

// s3Conn is what an instance publishes about itself: the root credentials and
// the endpoint, for a consumer that talks to the instance directly instead of
// being cut a bucket.
func s3Conn(d *repo.Tile) []ConnVar {
	return []ConnVar{
		{"S3_ENDPOINT", fmt.Sprintf("http://%s", hostPort(d)), false},
		{"S3_REGION", s3Region, false},
		{"S3_ACCESS_KEY", d.DBUser, true},
		{"S3_SECRET_KEY", d.DBPassword, true},
	}
}

// s3Outputs is the published surface of one bucket. A public bucket also
// publishes the unsigned base URL for direct object links.
func s3Outputs(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision) []repo.ResourceOutput {
	base := s.instancePublicBase(ctx, instance)
	// requires_network: the in-network hostname only resolves on the
	// instance's shared network, so the resolver has to pull the consumer on.
	endpoint, requiresNet := base, false
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", envnet.TileAlias(instance.ID), Engines[instance.Engine].Port)
		requiresNet = true
	}
	out := []repo.ResourceOutput{
		{Name: "S3_ENDPOINT", Value: endpoint, RequiresNetwork: requiresNet},
		{Name: "S3_BUCKET", Value: p.DBName},
		{Name: "S3_REGION", Value: s3Region},
		{Name: "S3_ACCESS_KEY", Value: p.DBUser, Secret: true},
		{Name: "S3_SECRET_KEY", Value: p.DBPassword, Secret: true},
	}
	if p.Public && base != "" { // a public bucket needs a public instance
		out = append(out, repo.ResourceOutput{Name: "S3_PUBLIC_URL", Value: base + "/" + p.DBName})
	}
	return out
}

// instancePublicBase returns the instance's public URL (scheme+host from its
// first non-redirect domain), or "" when it has no public domain.
func (s *Service) instancePublicBase(ctx context.Context, instance *repo.Tile) string {
	doms, err := s.store.ListDomainsByTile(ctx, instance.ID)
	if err != nil {
		return ""
	}
	for i := range doms {
		if doms[i].RedirectTo != "" || doms[i].Host == "" {
			continue
		}
		scheme := "http"
		if doms[i].HTTPS {
			scheme = "https"
		}
		return scheme + "://" + doms[i].Host
	}
	return ""
}

// SetBucketPublic flips an existing bucket's public-read policy and records it
// on the provision, so exposure is a setting rather than a decision frozen at
// creation. Provisioning could already do this; nothing could undo it.
//
// The mirrored managed_resource and its outputs are re-synced, because
// S3_PUBLIC_URL only exists while the bucket is public.
func (s *Service) SetBucketPublic(ctx context.Context, instance *repo.Tile, p *repo.Provision, public bool) error {
	if !Engines[instance.Engine].PublicSlices {
		return fmt.Errorf("%s slices cannot be made public", instance.Engine)
	}
	if public && s.instancePublicBase(ctx, instance) == "" {
		return fmt.Errorf("instance %s needs a public domain before a public bucket", instance.Slug)
	}
	endpoint, err := s.reachInstance(ctx, instance)
	if err != nil {
		return err
	}
	if public {
		err = s.putPublicReadPolicy(ctx, endpoint, instance, p.DBName)
	} else {
		err = s.dropPublicReadPolicy(ctx, endpoint, instance, p.DBName)
	}
	if err != nil {
		return err
	}
	p.Public = public
	if err := s.store.UpdateProvision(ctx, p); err != nil {
		return err
	}
	return s.SyncResource(ctx, instance, p)
}

// dropPublicReadPolicy removes the bucket policy entirely, which returns the
// bucket to credentials-only access. A bucket that never had one deletes
// cleanly too, so this is safe to call unconditionally.
func (s *Service) dropPublicReadPolicy(ctx context.Context, endpoint string, instance *repo.Tile, bucket string) error {
	cl := s3.New(s3.Options{
		Region:       s3Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(instance.DBUser, instance.DBPassword, ""),
	})
	if _, err := cl.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)}); err != nil {
		return fmt.Errorf("removing public-read policy on %q: %w", bucket, err)
	}
	return nil
}

// putPublicReadPolicy grants anonymous s3:GetObject on the bucket, RustFS
// honors the standard S3 bucket policy, so objects become fetchable unsigned.
func (s *Service) putPublicReadPolicy(ctx context.Context, endpoint string, instance *repo.Tile, bucket string) error {
	cl := s3.New(s3.Options{
		Region:       s3Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(instance.DBUser, instance.DBPassword, ""),
	})
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::%s/*"}]}`, bucket)
	_, err := cl.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(policy)})
	if err != nil {
		return fmt.Errorf("public-read policy on %q: %w", bucket, err)
	}
	return nil
}

// createBucket makes the bucket if absent (idempotent) using the instance root
// creds, dialing endpoint (the stackr-reachable address from reachInstance).
func (s *Service) createBucket(ctx context.Context, endpoint string, instance *repo.Tile, bucket string) error {
	cl := s3.New(s3.Options{
		Region:       s3Region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(instance.DBUser, instance.DBPassword, ""),
	})
	_, err := cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil // idempotent re-provision
	}
	return fmt.Errorf("create bucket %q: %w", bucket, err)
}

// reachInstance returns an S3 endpoint URL the stackr PROCESS can dial. When
// stackr runs as a container it joins the instance's shared net and uses the
// tile alias; on a dev host it reaches the container by IP.
func (s *Service) reachInstance(ctx context.Context, instance *repo.Tile) (string, error) {
	_ = s.joinSharedNet(ctx, instance)
	if cid, err := s.ContainerID(ctx, instance); err != nil || cid == "" {
		return "", fmt.Errorf("instance %s is not running", instance.Slug)
	}
	port := Engines[instance.Engine].Port
	// runtime.SelfContainerID, not a hostname guess. The panel and the
	// installer both set --hostname stkr-panel so the overlay alias resolves,
	// and under swarm the hostname is the task name, so matching containers by
	// hostname prefix found nothing. It then fell through to the instance's
	// overlay IP, which the panel cannot route to because it is not on that
	// network, and every s3 slice failed with a dial timeout.
	if own := runtime.SelfContainerID(ctx, s.c.Runtime()); own != "" {
		if err := s.c.ConnectContainer(ctx, SharedNet(instance), own, nil); err != nil {
			return "", fmt.Errorf("join shared net: %w", err)
		}
		// By address on that overlay, not by the tile alias.
		//
		// The alias lives in the service spec, and swarm accepts an
		// alias-only spec change without rolling the task, so a freshly
		// attached instance answers to its alias only after its next natural
		// restart. Provisioning cannot wait for that: it happens seconds after
		// the instance is created, and dialling the alias got whatever else
		// was on the overlay, which is "connection refused" at best.
		//
		// Consumers keep using the alias, which is right for them: they are
		// deployed later and their DNS resolves it. This is only the
		// provisioner's own hop.
		if ip, err := s.sharedNetIP(ctx, instance); err == nil && ip != "" {
			return fmt.Sprintf("http://%s:%d", ip, port), nil
		}
		return fmt.Sprintf("http://%s:%d", envnet.TileAlias(instance.ID), port), nil
	}
	ip, err := s.instanceIP(ctx, instance)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

// sharedNetIP is the instance's address on its own shared overlay, which is
// the one the panel has just joined. instanceIP takes whichever network
// happens to be first, and for a database that is its environment's net, which
// the panel is not on.
func (s *Service) sharedNetIP(ctx context.Context, instance *repo.Tile) (string, error) {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return "", fmt.Errorf("instance %s is not running", instance.Slug)
	}
	ip, _, err := s.c.NetworkMemberAddr(ctx, SharedNet(instance), cid)
	return ip, err
}

func (s *Service) instanceIP(ctx context.Context, instance *repo.Tile) (string, error) {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return "", fmt.Errorf("instance %s is not running", instance.Slug)
	}
	// ListAll populates per-network IPs (ListByLabel does not).
	all, err := s.c.ListAll(ctx, s.c.Self(ctx)) // the dev-host path: the instance is local by construction
	if err != nil {
		return "", err
	}
	for i := range all {
		if all[i].ID == cid && len(all[i].IPs) > 0 {
			return all[i].IPs[0], nil
		}
	}
	return "", fmt.Errorf("no reachable IP for instance %s", instance.Slug)
}
