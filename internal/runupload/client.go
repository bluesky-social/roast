// Package runupload uploads immutable Roast run bundles directly to an
// operator-configured S3-compatible object store.
package runupload

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/bluesky-social/roast/internal/config"
)

type uploadResult struct {
	Key       string
	ETag      string
	VersionID string
}

type objectStore interface {
	Destination() string
	Put(context.Context, io.Reader, int64, string) (uploadResult, error)
}

type s3Store struct {
	client      *minio.Client
	bucket      string
	prefix      string
	destination string
}

// NewStore constructs a direct S3-compatible uploader using only the
// explicitly resolved access key, secret, and optional session token.
func NewStore(cfg config.ResolvedUpload) (objectStore, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("parse upload endpoint %q", cfg.Endpoint)
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken),
		Secure: u.Scheme == "https", Region: cfg.Region, BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3-compatible client: %w", err)
	}
	destination := strings.TrimSuffix(cfg.Endpoint, "/") + "/" + cfg.Bucket + "/" + strings.Trim(cfg.Prefix, "/")
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: strings.Trim(cfg.Prefix, "/"), destination: destination}, nil
}

func (s *s3Store) Destination() string { return s.destination }

func (s *s3Store) Put(ctx context.Context, body io.Reader, size int64, digest string) (uploadResult, error) {
	key := path.Join(s.prefix, "sha256", digest+".tar.zst")
	info, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: "application/zstd", SendContentMd5: true, DisableContentSha256: true,
		UserMetadata: map[string]string{"roast-bundle-sha256": digest},
	})
	if err != nil {
		return uploadResult{}, err
	}
	return uploadResult{Key: info.Key, ETag: info.ETag, VersionID: info.VersionID}, nil
}
