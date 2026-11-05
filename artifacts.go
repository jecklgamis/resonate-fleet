package main

import (
	"context"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// uploadTimeout bounds the whole upload of one task's artifacts.
const uploadTimeout = 5 * time.Minute

// ArtifactStore copies a finished task's files somewhere durable. Upload returns
// the location they now live under.
type ArtifactStore interface {
	Upload(ctx context.Context, taskID, dir string) (location string, err error)
}

// s3Putter is the slice of the S3 client the uploader uses, so tests can fake it.
type s3Putter interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3Store uploads a task's workspace files to s3://Bucket/Prefix/<taskID>/.
type S3Store struct {
	Bucket string
	Prefix string // optional key prefix, without leading or trailing slash
	client s3Putter
}

// NewS3Store builds a store using the standard AWS credential chain (env vars,
// shared config, IRSA, instance role). endpoint, if set, points the client at an
// S3-compatible service such as MinIO and switches to path-style addressing.
func NewS3Store(ctx context.Context, bucket, prefix, endpoint string) (*S3Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
	return &S3Store{Bucket: bucket, Prefix: strings.Trim(prefix, "/"), client: client}, nil
}

func (s *S3Store) keyPrefix(taskID string) string {
	if s.Prefix == "" {
		return taskID
	}
	return path.Join(s.Prefix, taskID)
}

// Upload puts every regular file directly under dir except the worker's own
// task.json state file: the scenario, uploaded data files, the reports, the
// per-request results and the log.
func (s *S3Store) Upload(ctx context.Context, taskID, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	base := s.keyPrefix(taskID)
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == taskFile || e.Name() == taskFile+".tmp" {
			continue
		}
		if err := s.put(ctx, base+"/"+e.Name(), filepath.Join(dir, e.Name())); err != nil {
			return "", fmt.Errorf("uploading %s: %w", e.Name(), err)
		}
	}
	return "s3://" + s.Bucket + "/" + base + "/", nil
}

func (s *S3Store) put(ctx context.Context, key, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	in := &s3.PutObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key), Body: f}
	if ct := mime.TypeByExtension(filepath.Ext(file)); ct != "" {
		in.ContentType = aws.String(ct)
	}
	_, err = s.client.PutObject(ctx, in)
	return err
}
