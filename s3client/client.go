package s3client

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	appconfig "github.com/s3smb-gateway/config"
)

// Client wraps AWS S3 client with helper methods
type Client struct {
	client    *s3.Client
	bucket    string
	prefix    string
	chunkSize int64
}

// ObjectInfo represents S3 object metadata
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
	VersionID    string
	IsDirectory  bool
}

// NewClient creates a new S3 client
// Credential resolution order:
// 1. Environment variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)
// 2. AWS Profile (from config or AWS_PROFILE env var)
// 3. AWS shared credentials file (~/.aws/credentials)
// 4. IAM instance role (for EC2/ECS)
func NewClient(ctx context.Context, cfg *appconfig.Config) (*Client, error) {
	var awsCfg aws.Config
	var err error

	// Configure AWS SDK options
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.S3.Region),
	}

	// Use AWS profile if specified
	if cfg.S3.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(cfg.S3.Profile))
	}

	// Use explicit credentials only if provided via environment variables
	// (config file credentials are no longer supported for security)
	if cfg.S3.AccessKey != "" && cfg.S3.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.S3.AccessKey,
				cfg.S3.SecretKey,
				"",
			),
		))
	}

	// Load AWS configuration (uses default credential chain)
	awsCfg, err = config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client options
	s3Opts := []func(*s3.Options){}

	// Use custom endpoint if provided (for S3-compatible services)
	if cfg.S3.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.S3.Endpoint)
			// Some S3-compatible endpoints require virtual-host style (bucket in hostname).
			// Do not force path-style; allow virtual-host addressing so services that
			// require bucket as host (e.g. Huawei OBS) work correctly.
			o.UsePathStyle = false
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	// Quick sanity check: verify we can access the configured bucket. This
	// surfaces missing credentials (e.g., SDK trying EC2 IMDS) or misconfigured
	// bucket/region/endpoint early with a helpful hint instead of failing later
	// on uploaded PutObject calls.
	if cfg.S3.Bucket != "" {
		headCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, headErr := client.HeadBucket(headCtx, &s3.HeadBucketInput{Bucket: aws.String(cfg.S3.Bucket)})
		if headErr != nil {
			hint := "check S3 credentials (set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or provide AccessKey/SecretKey in config) and verify bucket/region/endpoint"
			if strings.Contains(headErr.Error(), "ec2imds") || strings.Contains(headErr.Error(), "GetMetadata") {
				hint = "no credentials found: SDK attempted EC2 IMDS and failed. Set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY or add AccessKey/SecretKey to the gateway config"
			}
			return nil, fmt.Errorf("failed to access bucket %s: %w; hint: %s", cfg.S3.Bucket, headErr, hint)
		}
	}

	chunkSize := cfg.ChunkSize
	if chunkSize == 0 {
		chunkSize = appconfig.ChunkSize
	}

	return &Client{
		client:    client,
		bucket:    cfg.S3.Bucket,
		prefix:    cfg.S3.Prefix,
		chunkSize: chunkSize,
	}, nil
}

// GetFullKey returns the full S3 key with prefix
func (c *Client) GetFullKey(key string) string {
	if c.prefix == "" {
		return key
	}
	if key == "" {
		return c.prefix
	}
	return c.prefix + "/" + key
}

// HeadObject retrieves object metadata without downloading content
func (c *Client) HeadObjectInfo(ctx context.Context, key string) (*ObjectInfo, error) {
	fullKey := c.GetFullKey(key)

	resp, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to head object %s: %w", fullKey, err)
	}

	contentType := ""
	if resp.ContentType != nil {
		contentType = *resp.ContentType
	}

	versionID := ""
	if resp.VersionId != nil {
		versionID = *resp.VersionId
	}

	etag := ""
	if resp.ETag != nil {
		etag = *resp.ETag
	}

	return &ObjectInfo{
		Key:          key,
		Size:         *resp.ContentLength,
		ETag:         etag,
		LastModified: *resp.LastModified,
		ContentType:  contentType,
		VersionID:    versionID,
		IsDirectory:  false,
	}, nil
}

// GetObjectChunk downloads a specific byte range of an object
func (c *Client) GetObjectChunk(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	fullKey := c.GetFullKey(key)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object chunk %s [%d-%d]: %w", fullKey, offset, offset+length-1, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read object chunk: %w", err)
	}

	return data, nil
}

// GetObject downloads an entire object
func (c *Client) GetObject(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error) {
	fullKey := c.GetFullKey(key)

	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get object %s: %w", fullKey, err)
	}

	contentType := ""
	if resp.ContentType != nil {
		contentType = *resp.ContentType
	}

	versionID := ""
	if resp.VersionId != nil {
		versionID = *resp.VersionId
	}

	etag := ""
	if resp.ETag != nil {
		etag = *resp.ETag
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         *resp.ContentLength,
		ETag:         etag,
		LastModified: *resp.LastModified,
		ContentType:  contentType,
		VersionID:    versionID,
		IsDirectory:  false,
	}

	return resp.Body, info, nil
}

// PutObject uploads an object to S3
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, contentType string) error {
	fullKey := c.GetFullKey(key)

	input := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
		Body:   body,
	}

	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}

	_, err := c.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to put object %s: %w", fullKey, err)
	}

	return nil
}

// PutObjectFromReader uploads an object from an io.Reader with known size
// This is more efficient for large files as it sets ContentLength
func (c *Client) PutObjectFromReader(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	fullKey := c.GetFullKey(key)

	input := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(fullKey),
		Body:          reader,
		ContentLength: aws.Int64(size),
	}

	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}

	_, err := c.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to put object %s: %w", fullKey, err)
	}

	return nil
}

// DeleteObject deletes an object from S3
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	fullKey := c.GetFullKey(key)

	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object %s: %w", fullKey, err)
	}

	return nil
}

// CopyObject copies an object within S3 (for rename operations)
func (c *Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	fullSrcKey := c.GetFullKey(srcKey)
	fullDstKey := c.GetFullKey(dstKey)

	// CopySource MUST be URL-encoded per the S3 API spec.
	// Without this, paths with non-ASCII chars (e.g. 'Almacén') cause
	// silent copy failures on Huawei OBS — the server returns success
	// but the destination object may be empty or contain wrong data.
	encodedSrc := c.bucket + "/" + encodePath(fullSrcKey)

	_, err := c.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		CopySource: aws.String(encodedSrc),
		Key:        aws.String(fullDstKey),
	})
	if err != nil {
		return fmt.Errorf("failed to copy object %s to %s: %w", fullSrcKey, fullDstKey, err)
	}

	return nil
}

// encodePath URL-encodes each segment of an S3 key path while preserving slashes.
func encodePath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}

// ListObjects lists objects with a given prefix (for directory listing)
func (c *Client) ListObjects(ctx context.Context, prefix string, delimiter string, maxKeys int32, continuationToken string) ([]ObjectInfo, []string, string, error) {
	fullPrefix := c.GetFullKey(prefix)
	// Only add trailing slash for non-empty prefixes
	if fullPrefix != "" && fullPrefix[len(fullPrefix)-1] != '/' {
		fullPrefix += "/"
	}

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		MaxKeys: aws.Int32(maxKeys),
	}

	// Only set prefix if non-empty
	if fullPrefix != "" {
		input.Prefix = aws.String(fullPrefix)
	}

	if delimiter != "" {
		input.Delimiter = aws.String(delimiter)
	}

	if continuationToken != "" {
		input.ContinuationToken = aws.String(continuationToken)
	}

	resp, err := c.client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to list objects: %w", err)
	}

	// Convert to ObjectInfo slice
	objects := make([]ObjectInfo, 0, len(resp.Contents))
	for _, obj := range resp.Contents {
		// Skip the prefix directory itself
		if *obj.Key == fullPrefix {
			continue
		}

		etag := ""
		if obj.ETag != nil {
			etag = *obj.ETag
		}

		// Get accurate size from HeadObject if ListObjectsV2 returns suspicious size
		// Huawei OBS sometimes returns incorrect large sizes (~824GB) in ListObjectsV2
		size := *obj.Size
		if size > 1024*1024*1024 { // If size > 1GB, verify with HeadObject
			headResp, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(c.bucket),
				Key:    obj.Key,
			})
			if err == nil && headResp.ContentLength != nil {
				// Use the accurate size from HeadObject
				size = *headResp.ContentLength
			}
		}

		objects = append(objects, ObjectInfo{
			Key:          *obj.Key,
			Size:         size,
			ETag:         etag,
			LastModified: *obj.LastModified,
			IsDirectory:  false,
		})
	}

	// Get common prefixes (directories)
	dirs := make([]string, 0, len(resp.CommonPrefixes))
	for _, prefix := range resp.CommonPrefixes {
		dirs = append(dirs, *prefix.Prefix)
	}

	// Get next continuation token
	nextToken := ""
	if resp.NextContinuationToken != nil {
		nextToken = *resp.NextContinuationToken
	}

	return objects, dirs, nextToken, nil
}

// ObjectExists checks if an object exists
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("key cannot be empty")
	}
	_, err := c.HeadObjectInfo(ctx, key)
	if err != nil {
		// Check if it's a not found error
		var notFound *types.NotFound
		if err.Error() == "NotFound" || err.Error() == "NoSuchKey" {
			return false, nil
		}
		// Try to detect not found from error
		if _, ok := err.(*types.NotFound); ok {
			return false, nil
		}
		if notFound != nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// BucketExists verifies the bucket is accessible by listing objects
func (c *Client) BucketExists(ctx context.Context) error {
	_, err := c.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("failed to access bucket %s: %w", c.bucket, err)
	}
	return nil
}

// CreateDirectory creates a directory marker in S3
func (c *Client) CreateDirectory(ctx context.Context, key string) error {
	fullKey := c.GetFullKey(key)
	if fullKey != "" && fullKey[len(fullKey)-1] != '/' {
		fullKey += "/"
	}

	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(fullKey),
		Body:        nil,
		ContentType: aws.String("application/x-directory"),
	})
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %w", fullKey, err)
	}

	return nil
}

// GetChunkSize returns the configured chunk size
func (c *Client) GetChunkSize() int64 {
	return c.chunkSize
}

// GetBucket returns the bucket name
func (c *Client) GetBucket() string {
	return c.bucket
}

// ListObjectsV2 wraps the S3 ListObjectsV2 API for compatibility with metadata.S3Client interface
func (c *Client) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return c.client.ListObjectsV2(ctx, params, optFns...)
}

// HeadObject wraps the S3 HeadObject API for compatibility with metadata.S3Client interface
func (c *Client) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return c.client.HeadObject(ctx, params, optFns...)
}
