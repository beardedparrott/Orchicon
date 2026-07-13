package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Store implements Store over S3-compatible storage (AWS S3, MinIO,
// etc.). The bucket + endpoint are configured via BlobStoreConfig. If
// the endpoint is empty the default AWS S3 endpoint is used.
type S3Store struct {
	client *s3.Client
	bucket string
}

// NewS3Store constructs an S3 store. region + bucket are required;
// endpoint is optional (set for MinIO/other S3-compatible services).
// Static credentials may be provided via the standard AWS env vars
// (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN); when
// absent the default credential chain is used.
func NewS3Store(ctx context.Context, bucket, region, endpoint string) (*S3Store, error) {
	if bucket == "" {
		return nil, errors.New("blobstore: s3 store requires a bucket")
	}
	opts := []func(*awscfg.LoadOptions) error{
		awscfg.WithRegion(region),
	}
	if endpoint != "" {
		opts = append(opts, awscfg.WithBaseEndpoint(endpoint))
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("blobstore: load aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true // works with both AWS and MinIO
		// Anonymous credentials are honored if the env is unset, so
		// the bucket's own policy/IAM applies.
		if cfg.Credentials == nil {
			o.Credentials = aws.AnonymousCredentials{}
		}
	})
	return &S3Store{client: client, bucket: bucket}, nil
}

// Put stores r under ref in the S3 bucket and returns metadata.
func (s *S3Store) Put(ctx context.Context, ref string, mimeType string, r io.Reader) (Blob, error) {
	key, err := s3Key(ref)
	if err != nil {
		return Blob{}, err
	}
	// Tee the reader through a sha256 hasher so we can report the digest
	// without a second copy.
	h := sha256.New()
	tee := io.TeeReader(r, h)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        tee,
		ContentType: aws.String(mimeType),
	})
	if err != nil {
		return Blob{}, fmt.Errorf("blobstore: s3 put: %w", err)
	}
	// Size is unknown without a HEAD; fetch it.
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		// Return what we have; the digest is still correct.
		return Blob{Ref: ref, MimeType: mimeType, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
	}
	size := int64(0)
	if head.ContentLength != nil {
		size = *head.ContentLength
	}
	return Blob{
		Ref:      ref,
		MimeType: mimeType,
		Size:     size,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// Get returns a reader for ref. The caller closes the reader.
func (s *S3Store) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	key, err := s3Key(ref)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore: s3 get: %w", err)
	}
	return out.Body, nil
}

// Delete removes ref. Missing refs are not errors.
func (s *S3Store) Delete(ctx context.Context, ref string) error {
	key, err := s3Key(ref)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blobstore: s3 delete: %w", err)
	}
	return nil
}

// s3Key returns the S3 object key for a ref, sharded by the first 2
// hex chars of the ref's sha256 to keep key cardinality balanced.
func s3Key(ref string) (string, error) {
	safe := sanitizeRef(ref)
	if safe == "" {
		return "", errors.New("blobstore: empty ref")
	}
	h := sha256.Sum256([]byte(safe))
	prefix := hex.EncodeToString(h[:1])
	return prefix + "/" + safe, nil
}
