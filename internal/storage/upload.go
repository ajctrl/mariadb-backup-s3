package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	transfertypes "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Upload consumes and closes body. The producer must return EOF only after all
// processing succeeds; a read error aborts the upload without publishing a key.
func (s *S3) Upload(ctx context.Context, key string, body io.ReadCloser, createOnly bool) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { body.Close() }) }
	defer closeBody()
	if err := ctx.Err(); err != nil {
		return err
	}
	// SDK reads from unknown-length streams can block independently of HTTP.
	stop := context.AfterFunc(ctx, closeBody)
	defer stop()
	var condition *string
	if createOnly {
		condition = aws.String("*")
	}
	var err error
	if s.diskUpload {
		err = s.uploadFromDisk(ctx, key, body, condition)
	} else {
		err = s.uploadStream(ctx, cancel, key, body, condition)
	}
	if cause := context.Cause(ctx); cause != nil {
		err = errors.Join(cause, err)
	}
	if err != nil {
		return fmt.Errorf("upload S3 object %q: %w", key, err)
	}
	return nil
}

func (s *S3) uploadStream(ctx context.Context, cancel context.CancelCauseFunc, key string, body io.Reader, condition *string) error {
	digest := crc32.NewIEEE()
	client := &streamUploadClient{Client: s.client, cancel: cancel, digest: digest}
	out, err := s.transfer.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: io.TeeReader(body, digest), IfNoneMatch: condition,
		ChecksumAlgorithm: transfertypes.ChecksumAlgorithmCrc32, ChecksumType: transfertypes.ChecksumTypeFullObject,
	}, func(o *transfermanager.Options) {
		o.S3 = client
		o.PartSizeBytes = s.uploadPartSize
		o.MultipartUploadThreshold = s.uploadPartSize
		o.Concurrency = 2
	})
	if err != nil {
		return err
	}
	return checkETag(out.ETag)
}

func checkETag(tag *string) error {
	if value := strings.Trim(aws.ToString(tag), "\" "); value == "" || value == "*" {
		return errors.New("S3 upload response is missing a specific ETag")
	}
	return nil
}

type streamUploadClient struct {
	*s3.Client
	cancel context.CancelCauseFunc
	digest hash.Hash32
}

func checkedMultipartUpload(out *s3.CreateMultipartUploadOutput, err error) (*s3.CreateMultipartUploadOutput, error) {
	if err != nil {
		return nil, err
	}
	if out == nil || strings.TrimSpace(aws.ToString(out.UploadId)) == "" {
		return nil, errors.New("S3 multipart response is missing an upload ID")
	}
	return out, nil
}

func (c *streamUploadClient) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return checkedMultipartUpload(c.Client.CreateMultipartUpload(ctx, in, opts...))
}

func (c *streamUploadClient) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	// The transfer manager buffers the whole body for a single PUT. Multipart
	// uploads already have a full first part before they are initiated.
	size, err := transfertypes.ReadSeekCloser(in.Body).GetLen()
	if err != nil {
		return nil, fmt.Errorf("measure buffered backup: %w", err)
	}
	if size < 0 {
		return nil, errors.New("unknown buffered backup size")
	}
	if size == 0 {
		return nil, errors.New("no backup data to upload")
	}
	return c.Client.PutObject(ctx, in, opts...)
}

// The producer has reached EOF and all parts have finished before completion.
func (c *streamUploadClient) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	in.ChecksumCRC32 = aws.String(base64.StdEncoding.EncodeToString(c.digest.Sum(nil)))
	return c.Client.CompleteMultipartUpload(ctx, in, opts...)
}

func (c *streamUploadClient) UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	out, err := c.Client.UploadPart(ctx, in, opts...)
	if err == nil {
		err = checkETag(out.ETag)
	}
	if err != nil {
		// After SDK retries fail, interrupt a producer stalled between parts.
		// Otherwise the transfer manager can remain blocked reading the next part.
		c.cancel(err)
	}
	return out, err
}
