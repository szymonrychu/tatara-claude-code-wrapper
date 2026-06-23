// Package storage persists Claude Code conversation transcripts to an
// S3-compatible object store so a fresh pod can resume a prior conversation
// (issue #114). One client targets both AWS S3 and Ceph RGW / MinIO via a
// configurable endpoint and a path-style toggle.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// Store is the minimal object-store surface the wrapper needs for conversation
// persistence. The S3 Client implements it; tests use MemStore. The operator
// (a separate module) mirrors this shape for its GC pass.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Config is the S3 connection config, sourced from the wrapper env (which the
// operator injects from a k8s secret + ConfigMap).
type Config struct {
	Endpoint       string // S3_ENDPOINT; empty uses AWS default endpoint resolution
	Bucket         string // S3_BUCKET
	Region         string // S3_REGION
	KeyPrefix      string // S3_KEY_PREFIX; prepended to every key
	ForcePathStyle bool   // S3_FORCE_PATH_STYLE; true for Ceph RGW / MinIO
	AccessKeyID    string // AWS_ACCESS_KEY_ID; empty falls back to the default chain (e.g. IRSA)
	SecretKey      string // AWS_SECRET_ACCESS_KEY
}

// Enabled reports whether conversation persistence is configured. With no
// bucket the feature is off and the wrapper behaves exactly as before.
func (c Config) Enabled() bool { return c.Bucket != "" }

// Client is the S3-backed Store.
type Client struct {
	s3     *s3.Client
	bucket string
	prefix string
}

var _ Store = (*Client)(nil)

// New builds an S3 client from cfg. When AccessKeyID and SecretKey are both set
// it uses static credentials (Ceph RGW or explicit keys); otherwise it falls
// back to the AWS default credential chain (env / IRSA). It returns an error if
// cfg has no bucket. Construction makes no network call.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if !cfg.Enabled() {
		return nil, errors.New("storage: no bucket configured")
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage: load aws config: %w", err)
	}
	s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})
	return &Client{s3: s3c, bucket: cfg.Bucket, prefix: cfg.KeyPrefix}, nil
}

// fullKey joins the configured prefix and the logical key with a single slash.
func (c *Client) fullKey(key string) string {
	p := strings.Trim(c.prefix, "/")
	k := strings.TrimLeft(key, "/")
	if p == "" {
		return k
	}
	return p + "/" + k
}

// Put stores body under key. The body is buffered into a seekable reader so the
// request is retryable and carries a content length; conversation transcripts
// are bounded (a few MB), so the buffer cost is acceptable.
func (c *Client) Put(ctx context.Context, key string, body io.Reader) error {
	buf, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("storage put %s: read body: %w", key, err)
	}
	if _, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.fullKey(key)),
		Body:   bytes.NewReader(buf),
	}); err != nil {
		return fmt.Errorf("storage put %s: %w", key, err)
	}
	return nil
}

// Get returns the object at key. The caller must close the returned reader.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.fullKey(key)),
	})
	if err != nil {
		return nil, fmt.Errorf("storage get %s: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes the object at key. Deleting a missing key is not an error
// (S3 DeleteObject is idempotent).
func (c *Client) Delete(ctx context.Context, key string) error {
	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.fullKey(key)),
	}); err != nil {
		return fmt.Errorf("storage delete %s: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present. A 404 (typed NotFound or a generic
// NoSuchKey/NotFound API error from an S3-compatible server) maps to (false,
// nil); any other error is returned.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	if _, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(c.fullKey(key)),
	}); err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NotFound", "NoSuchKey", "404":
				return false, nil
			}
		}
		return false, fmt.Errorf("storage exists %s: %w", key, err)
	}
	return true, nil
}
