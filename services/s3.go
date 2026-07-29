package services

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Storage struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	Enabled bool
}

func NewS3Storage(accessKeyID, secretKey, region, bucket string) *S3Storage {
	if accessKeyID == "" || secretKey == "" || region == "" || bucket == "" {
		return &S3Storage{Enabled: false}
	}

	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretKey, ""),
	}
	client := s3.NewFromConfig(cfg)
	return &S3Storage{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  bucket,
		Enabled: true,
	}
}

func (s *S3Storage) PresignGet(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	if !s.Enabled {
		return "", fmt.Errorf("S3 image storage is not configured")
	}
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(lifetime))
	if err != nil {
		return "", fmt.Errorf("S3 PresignGetObject: %w", err)
	}
	return out.URL, nil
}

func (s *S3Storage) Put(ctx context.Context, key, contentType string, body io.Reader, contentLength int64) error {
	if !s.Enabled {
		return fmt.Errorf("S3 image storage is not configured")
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(contentLength),
		CacheControl:  aws.String("public, max-age=3600"),
	})
	if err != nil {
		return fmt.Errorf("S3 PutObject: %w", err)
	}
	return nil
}

func (s *S3Storage) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	if !s.Enabled {
		return nil, fmt.Errorf("S3 image storage is not configured")
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject: %w", err)
	}
	return out, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	if !s.Enabled {
		return fmt.Errorf("S3 image storage is not configured")
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("S3 DeleteObject: %w", err)
	}
	return nil
}

func (s *S3Storage) Check(ctx context.Context) error {
	if !s.Enabled {
		return fmt.Errorf("not configured")
	}
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket: %w", err)
	}
	return nil
}
