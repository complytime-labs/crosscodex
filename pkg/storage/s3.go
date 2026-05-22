package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
)

type s3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

type s3Provider struct {
	client       s3API
	bucket       string
	tenantPrefix string
	tenantID     string
	closed       atomic.Bool
}

type S3Option func(*s3Options)

type s3Options struct {
	region   string
	endpoint string
	creds    aws.CredentialsProvider
}

func WithRegion(region string) S3Option {
	return func(o *s3Options) { o.region = region }
}

func WithEndpoint(endpoint string) S3Option {
	return func(o *s3Options) { o.endpoint = endpoint }
}

func WithCredentials(key, secret string) S3Option {
	return func(o *s3Options) {
		o.creds = credentials.NewStaticCredentialsProvider(key, secret, "")
	}
}

func NewS3(bucket, tenantID string, opts ...S3Option) (Provider, error) {
	if err := validateTenantID(tenantID); err != nil {
		return nil, err
	}
	if bucket == "" {
		return nil, ErrBucketRequired
	}

	options := &s3Options{}
	for _, opt := range opts {
		opt(options)
	}

	var cfgOpts []func(*awsconfig.LoadOptions) error
	if options.region != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(options.region))
	}
	if options.creds != nil {
		cfgOpts = append(cfgOpts, awsconfig.WithCredentialsProvider(options.creds))
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if options.endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(options.endpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(cfg, s3Opts...)

	return &s3Provider{
		client:       client,
		bucket:       bucket,
		tenantPrefix: tenantID + "/",
		tenantID:     tenantID,
	}, nil
}

func newS3WithClient(client s3API, bucket, tenantID string) *s3Provider {
	return &s3Provider{
		client:       client,
		bucket:       bucket,
		tenantPrefix: tenantID + "/",
		tenantID:     tenantID,
	}
}

func (p *s3Provider) fullKey(key string) string {
	return p.tenantPrefix + key
}

func isS3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound" || code == "404"
	}
	return false
}

// wrapS3Error translates S3 not-found errors to ErrNotFound
// and wraps other errors with the given operation label.
func wrapS3Error(err error, op string) error {
	if isS3NotFound(err) {
		return ErrNotFound
	}
	return fmt.Errorf("s3 %s: %w", op, err)
}

func (p *s3Provider) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}

	output, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil {
		return nil, wrapS3Error(err, "get")
	}
	return output.Body, nil
}

func (p *s3Provider) Put(ctx context.Context, key string, data io.Reader) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}
	if err := validateKey(key); err != nil {
		return err
	}

	_, err := p.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}
	return nil
}

func (p *s3Provider) Delete(ctx context.Context, key string) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}
	if err := validateKey(key); err != nil {
		return err
	}

	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	return nil
}

func (p *s3Provider) List(ctx context.Context, prefix string) ([]ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}
	if prefix != "" {
		if err := validateKey(prefix); err != nil {
			return nil, err
		}
	}

	fullPrefix := p.tenantPrefix + prefix
	var result []ObjectMetadata

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(p.bucket),
		Prefix: aws.String(fullPrefix),
	}

	for {
		output, err := p.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}

		for _, obj := range output.Contents {
			key := strings.TrimPrefix(aws.ToString(obj.Key), p.tenantPrefix)
			meta := ObjectMetadata{Key: key}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.LastModified = obj.LastModified.Unix()
			}
			if obj.ETag != nil {
				meta.ETag = *obj.ETag
			}
			result = append(result, meta)
		}

		if output.IsTruncated == nil || !*output.IsTruncated {
			break
		}
		input.ContinuationToken = output.NextContinuationToken
	}

	return result, nil
}

// headObject issues a HeadObject request and translates S3 not-found errors.
// Returns nil output when the key does not exist (with notFoundErr as the error).
func (p *s3Provider) headObject(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	output, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil {
		return nil, wrapS3Error(err, "head")
	}
	return output, nil
}

func (p *s3Provider) Exists(ctx context.Context, key string) (bool, error) {
	if p.closed.Load() {
		return false, ErrProviderClosed
	}
	if err := validateKey(key); err != nil {
		return false, err
	}

	_, err := p.headObject(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *s3Provider) Stat(ctx context.Context, key string) (*ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}

	output, err := p.headObject(ctx, key)
	if err != nil {
		return nil, err
	}

	meta := &ObjectMetadata{Key: key}
	if output.ContentLength != nil {
		meta.Size = *output.ContentLength
	}
	if output.ContentType != nil {
		meta.ContentType = *output.ContentType
	}
	if output.LastModified != nil {
		meta.LastModified = output.LastModified.Unix()
	}
	if output.ETag != nil {
		meta.ETag = *output.ETag
	}
	return meta, nil
}

func (p *s3Provider) Close() error {
	p.closed.Store(true)
	return nil
}
