package storage

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Exported for testing. Do not use outside tests.

// ExportValidateKey exposes validateKey for direct unit testing.
var ExportValidateKey = validateKey

// ExportValidateTenantID exposes validateTenantID for direct unit testing.
var ExportValidateTenantID = validateTenantID

// ExportXdgDataHome exposes xdgDataHome for direct unit testing.
var ExportXdgDataHome = xdgDataHome

// ExportS3API mirrors the unexported s3API interface for mock injection in
// external test packages.
type ExportS3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// ExportNewS3WithClient creates an S3 provider with a pre-injected client,
// bypassing AWS SDK configuration. The returned Provider can be used for
// unit testing with a mock S3 client.
func ExportNewS3WithClient(client ExportS3API, bucket, tenantID string) Provider {
	return newS3WithClient(client, bucket, tenantID)
}
