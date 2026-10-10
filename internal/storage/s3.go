// Package storage abstracts object storage. The implementation is S3-compatible,
// which covers Cloudflare R2 and MinIO without code changes.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/hireonai/backend-hireon-go/internal/config"
)

// Store puts and removes objects and hands back the URL to serve them from.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, contentType string) (string, error)
	Delete(ctx context.Context, url string) error
}

type S3Store struct {
	client    *s3.Client
	bucket    string
	publicURL string
}

// New builds an S3-compatible client. R2 requires a custom endpoint and the
// "auto" region; path-style addressing is used because R2 does not support the
// virtual-hosted style for arbitrary bucket names.
func New(ctx context.Context, cfg config.Config) (*S3Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.S3Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.S3AccessKeyID, cfg.S3SecretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		// R2 and MinIO both require path-style addressing on a custom endpoint.
		o.UsePathStyle = true
	})

	publicURL := cfg.S3PublicBaseURL
	if publicURL == "" {
		publicURL = strings.TrimRight(cfg.S3Endpoint, "/") + "/" + cfg.S3Bucket
	}

	return &S3Store{client: client, bucket: cfg.S3Bucket, publicURL: publicURL}, nil
}

// Put uploads an object and returns its public URL.
//
// Objects are uploaded without a public-read ACL: the bucket should serve them
// through a signed URL or an authenticated proxy. The previous implementation
// called makePublic() on every upload, which gave every user's CV a permanent,
// unauthenticated URL.
func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("put object %s: %w", key, err)
	}
	return s.publicURL + "/" + key, nil
}

// Delete removes an object given the URL previously returned by Put.
func (s *S3Store) Delete(ctx context.Context, url string) error {
	key := s.keyFromURL(url)
	if key == "" {
		return nil
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nf *types.NoSuchKey
		if errors.As(err, &nf) {
			return nil // already gone
		}
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) keyFromURL(url string) string {
	prefix := s.publicURL + "/"
	if strings.HasPrefix(url, prefix) {
		return strings.TrimPrefix(url, prefix)
	}
	// Fall back to the segment after the bucket name, tolerating a URL built with
	// a different public base (e.g. after a domain change).
	if i := strings.Index(url, "/"+s.bucket+"/"); i >= 0 {
		return url[i+len(s.bucket)+2:]
	}
	return ""
}

// Nil is a no-op store used when object storage is not configured, so the service
// still starts. Reads that need a URL will fail cleanly rather than panic.
type Nil struct{}

func (Nil) Put(context.Context, string, io.Reader, string) (string, error) {
	return "", errors.New("object storage is not configured")
}
func (Nil) Delete(context.Context, string) error { return nil }
