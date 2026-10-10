package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Store wraps an S3 client.
type S3Store struct {
	client *s3.Client
	bucket string
	// presigner signs the URLs clients download from. It is built for the endpoint clients can reach
	// (which may differ from the one this service uses), because the host is part of the signature.
	presigner *s3.PresignClient
}

// NewS3Store creates a new S3 store.
//
// publicEndpoint is the S3 address clients use to download payloads; it defaults to endpoint.
func NewS3Store(endpoint, publicEndpoint, region, bucket, accessKey, secretKey string, forcePathStyle bool) (*S3Store, error) {
	slog.Debug("NewS3Store", "bucket", bucket, "region", region, "endpoint", endpoint)
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	newClient := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			if endpoint != "" {
				// Use custom endpoint for local/S3-compatible storage
				o.BaseEndpoint = aws.String(endpoint)
				// For local Garage, we need path style
				o.UsePathStyle = forcePathStyle
			}
		})
	}
	if publicEndpoint == "" {
		publicEndpoint = endpoint
	}

	return &S3Store{
		client:    newClient(endpoint),
		bucket:    bucket,
		presigner: s3.NewPresignClient(newClient(publicEndpoint)),
	}, nil
}

// PresignGet returns a URL from which the payload stored under key can be downloaded without credentials
// until it expires after ttl, and the time it expires. Signing is local: the payload is not touched.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	slog.Debug("S3Store.PresignGet", "key", key, "ttl", ttl)
	expires := time.Now().Add(ttl)
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign s3 get object: %w", err)
	}
	return req.URL, expires, nil
}

// Upload stores payload in S3.
func (s *S3Store) Upload(ctx context.Context, key string, data io.Reader, size int64) error {
	slog.Debug("S3Store.Upload", "key", key, "size", size)
	// We need to buffer since aws sdk v2 PutObject needs an io.Reader but may need length
	// For simplicity, if size is known we pass it directly.
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          data,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("s3 put object: %w", err)
	}
	return nil
}

// Download retrieves payload from S3.
func (s *S3Store) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	slog.Debug("S3Store.Download", "key", key)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("s3 get object: %w", err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// Delete removes an object from S3.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	slog.Debug("S3Store.Delete", "key", key)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete object: %w", err)
	}
	return nil
}

// EnsureBucket creates the bucket if it doesn't exist.
func (s *S3Store) EnsureBucket(ctx context.Context) error {
	slog.Debug("S3Store.EnsureBucket", "bucket", s.bucket)
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		// Ignore bucket already exists errors.
		var alreadyExists *types.BucketAlreadyExists
		var alreadyOwned *types.BucketAlreadyOwnedByYou
		if errors.As(err, &alreadyExists) || errors.As(err, &alreadyOwned) {
			return nil
		}
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// HeadBucket checks if the bucket is accessible.
func (s *S3Store) HeadBucket(ctx context.Context) error {
	slog.Debug("S3Store.HeadBucket", "bucket", s.bucket)
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	return err
}

// ReadAll reads the S3 object into a byte slice.
func (s *S3Store) ReadAll(ctx context.Context, key string) ([]byte, error) {
	slog.Debug("S3Store.ReadAll", "key", key)
	rc, _, err := s.Download(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return nil, fmt.Errorf("read s3 object: %w", err)
	}
	return buf.Bytes(), nil
}
