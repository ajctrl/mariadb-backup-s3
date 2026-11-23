// Package storage implements the S3 operations needed by backup and restore.
package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/tempfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type Object struct {
	Key          string
	LastModified time.Time
	ETag         string
}

type Store interface {
	ManagesRetention() bool
	VersioningEnabled(context.Context) (bool, error)
	Upload(ctx context.Context, key string, body io.ReadCloser, createOnly bool) error
	Download(ctx context.Context, key, versionID string) (*os.File, error)
	List(ctx context.Context, prefix string, visit func(Object, time.Time) error) error
	Delete(ctx context.Context, objects []Object) error
}

type Config struct {
	Bucket              string
	Region              string
	Endpoint            string
	AccessKeyID         string
	SecretAccessKey     string
	SessionToken        string
	UploadPartSizeBytes int64
	AllowHTTP           bool
	DiskUpload          bool
}

type S3 struct {
	bucket         string
	client         *s3.Client
	transfer       *transfermanager.Client
	uploadPartSize int64
	diskUpload     bool
}

var _ Store = (*S3)(nil)

// New preserves the SDK credential chain (environment, profiles, web identity,
// container and instance roles). Explicit credentials and the legacy S3 aliases
// override their respective AWS environment values without mutating the process.
func New(ctx context.Context, cfg Config) (*S3, error) {
	return newS3(ctx, cfg, 60*time.Second)
}

func newS3(ctx context.Context, cfg Config, idleTimeout time.Duration) (*S3, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("S3 bucket must not be empty")
	}
	if cfg.UploadPartSizeBytes == 0 {
		cfg.UploadPartSizeBytes = 8 * 1024 * 1024
	}
	if cfg.UploadPartSizeBytes < 5*1024*1024 || cfg.UploadPartSizeBytes > 5*1024*1024*1024 {
		return nil, fmt.Errorf("S3 upload part size must be between 5 MiB and 5 GiB")
	}
	if !cfg.DiskUpload && cfg.UploadPartSizeBytes > 64<<20 {
		return nil, fmt.Errorf("part sizes over 64 MiB require S3_UPLOAD_BUFFER=disk")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Scheme == "http" && !cfg.AllowHTTP) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("S3_ENDPOINT requires HTTPS (HTTP requires S3_ALLOW_HTTP=true) and a host without credentials, query, or fragment")
		}
	}
	options := []func(*config.LoadOptions) error{}
	if cfg.Region != "" {
		options = append(options, config.WithRegion(cfg.Region))
	}
	accessKey := firstNonempty(cfg.AccessKeyID, os.Getenv("S3_ACCESS_KEY_ID"))
	secretKey := firstNonempty(cfg.SecretAccessKey, os.Getenv("S3_SECRET_ACCESS_KEY"))
	token := firstNonempty(cfg.SessionToken, os.Getenv("S3_SESSION_TOKEN"))
	if accessKey != "" || secretKey != "" || token != "" {
		accessKey = firstNonempty(accessKey, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_ACCESS_KEY"))
		secretKey = firstNonempty(secretKey, os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SECRET_KEY"))
		token = firstNonempty(token, os.Getenv("AWS_SESSION_TOKEN"))
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("S3 credential overrides require both an access key ID and secret access key (S3_* or AWS_* variables)")
		}
		options = append(options, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, token)))
	}
	awsConfig, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if awsConfig.Region == "" {
		awsConfig.Region = "us-west-1"
	}
	client := s3.NewFromConfig(awsConfig, func(o *s3.Options) {
		o.HTTPClient = withIdleTimeout(o.HTTPClient, idleTimeout, cfg.AllowHTTP)
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// S3-compatible endpoints often lack wildcard DNS/certificates.
			o.UsePathStyle = true
		}
	})
	transfer := transfermanager.New(client, func(o *transfermanager.Options) {
		// Failed/canceled uploads must still be able to abort uploaded parts.
		o.FailTimeout = 10 * time.Second
		// Respect AWS_REQUEST_CHECKSUM_CALCULATION and profile configuration.
		o.RequestChecksumCalculation = awsConfig.RequestChecksumCalculation
	})
	return &S3{bucket: cfg.Bucket, client: client, transfer: transfer, uploadPartSize: cfg.UploadPartSizeBytes, diskUpload: cfg.DiskUpload}, nil
}

// The resolved SDK option also covers endpoints supplied by AWS profiles and environment.
func (s *S3) ManagesRetention() bool { return s.client.Options().BaseEndpoint == nil }

func (s *S3) VersioningEnabled(ctx context.Context) (bool, error) {
	out, err := s.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return false, fmt.Errorf("get S3 bucket versioning: %w", err)
	}
	return out.Status == types.BucketVersioningStatusEnabled, nil
}

func (s *S3) Download(ctx context.Context, key, versionID string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled}
	if versionID != "" {
		input.VersionId = aws.String(versionID)
	}
	// One full GET reads one generation, including mutable null versions.
	obj, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("download S3 object %q: %w", key, err)
	}
	defer obj.Body.Close()
	if versionID != "" && aws.ToString(obj.VersionId) != versionID {
		return nil, fmt.Errorf("download S3 object %q: response did not confirm the requested version", key)
	}
	resp, _ := middleware.GetRawResponse(obj.ResultMetadata).(*smithyhttp.Response)
	if resp == nil || resp.StatusCode != http.StatusOK || obj.ContentRange != nil || aws.ToInt64(obj.ContentLength) <= 0 {
		return nil, fmt.Errorf("download S3 object %q: expected a nonempty full object with Content-Length", key)
	}
	f, err := tempfile.Open("", "mariadb-download-*")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, obj.Body)
	if err == nil && n != *obj.ContentLength {
		err = fmt.Errorf("received %d of %d bytes", n, *obj.ContentLength)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read S3 object %q: %w", key, err)
	}
	return f, nil
}

func (s *S3) List(ctx context.Context, prefix string, visit func(Object, time.Time) error) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/")})
	tokens := make(map[string]bool)
	var serverTime time.Time
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list S3 objects: %w", err)
		}
		if serverTime.IsZero() {
			serverTime, _ = middleware.GetServerTime(page.ResultMetadata)
		}
		if page.IsTruncated == nil || (!*page.IsTruncated && aws.ToString(page.NextContinuationToken) != "") {
			return fmt.Errorf("S3 list response has an inconsistent page boundary")
		}
		if *page.IsTruncated {
			token := aws.ToString(page.NextContinuationToken)
			if token == "" || tokens[token] {
				return fmt.Errorf("S3 list response contains an invalid continuation token")
			}
			tokens[token] = true
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				return fmt.Errorf("S3 list response contains an object without a key or modification time")
			}
			if err := visit(Object{Key: *obj.Key, LastModified: *obj.LastModified, ETag: strings.Trim(aws.ToString(obj.ETag), `"`)}, serverTime); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, objects []Object) error {
	ids := make([]types.ObjectIdentifier, 0, len(objects))
	for _, obj := range objects {
		etag := strings.Trim(obj.ETag, `"`)
		if etag == "" || etag == "*" {
			return fmt.Errorf("missing specific ETag for %q", obj.Key)
		}
		ids = append(ids, types.ObjectIdentifier{Key: aws.String(obj.Key), ETag: aws.String(etag)})
	}
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.bucket), Delete: &types.Delete{Objects: ids}}, func(o *s3.Options) {
		// General-purpose buckets require Content-MD5 for the serialized XML.
		o.APIOptions = append(o.APIOptions, smithyhttp.AddContentChecksumMiddleware)
	})
	if err != nil {
		return fmt.Errorf("delete expired objects: %w", err)
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return fmt.Errorf("delete %q: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
	}
	pending := make(map[string]int, len(objects))
	for _, obj := range objects {
		pending[obj.Key]++
	}
	for _, obj := range out.Deleted {
		key := aws.ToString(obj.Key)
		if pending[key] == 0 {
			return fmt.Errorf("S3 confirmed an unexpected deletion: %q", key)
		}
		pending[key]--
	}
	if len(out.Deleted) != len(objects) {
		return fmt.Errorf("S3 confirmed %d of %d deletions", len(out.Deleted), len(objects))
	}
	return nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
