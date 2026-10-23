package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

type UploadOptions struct {
	// RequireAbsent applies an atomic condition at publication, not a prior HEAD.
	RequireAbsent bool
	// BeforePublish runs after the stream succeeds, before each PUT or multipart
	// completion request attempt (including retries). An error prevents that request.
	BeforePublish func() error
}

// Upload consumes and closes body. The producer must return EOF only after all
// processing succeeds; a read error aborts the upload without publishing a key.
func (s *S3) Upload(ctx context.Context, key string, body io.ReadCloser, options UploadOptions) error {
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
	client := &streamUploadClient{Client: s.client, cancel: cancel, options: options}
	limit := s.uploadPartSize * s.maxUploadParts
	reader := &uploadLimitReader{body: body, remaining: limit, limit: limit, partSize: s.uploadPartSize}
	_, err := s.transfer.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: reader,
	}, func(o *transfermanager.Options) {
		o.S3 = client
		o.PartSizeBytes = s.uploadPartSize
		o.MultipartUploadThreshold = s.uploadPartSize
		o.Concurrency = 2
		o.MaxUploadParts = s.maxUploadParts
	})
	if cause := context.Cause(ctx); cause != nil {
		err = errors.Join(cause, err)
	}
	if err != nil {
		return fmt.Errorf("upload S3 object %q: %w", key, err)
	}
	return nil
}

// Unlike io.LimitReader, this reader must not turn excess data or a late
// producer failure into EOF: only a successful producer may publish the key.
type uploadLimitReader struct {
	body        io.Reader
	remaining   int64
	limit       int64
	partSize    int64
	terminalErr error
}

func (r *uploadLimitReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.body.Read(p)
		r.remaining -= int64(n)
		// SDK chunk reads can discard an error returned with a full buffer.
		// Deliver that error on the next read so it cannot publish a failed dump.
		if n > 0 && err != nil {
			r.terminalErr = err
			return n, nil
		}
		return n, err
	}
	// Probe one byte so an exact-size stream can finish, but an oversized
	// stream fails immediately without buffering another complete part.
	var probe [1]byte
	n, err := r.body.Read(probe[:])
	if n > 0 {
		return 0, fmt.Errorf("S3 upload exceeds the per-object limit of %d bytes (%.3f GiB) with S3_UPLOAD_PART_SIZE_MB=%d; increase S3_UPLOAD_PART_SIZE_MB (maximum 5120 MiB) or reduce the backup object size before retrying; larger parts use more memory",
			r.limit, float64(r.limit)/(1024*1024*1024), r.partSize/(1024*1024))
	}
	return 0, err
}

type streamUploadClient struct {
	*s3.Client
	cancel  context.CancelCauseFunc
	options UploadOptions
}

func (c *streamUploadClient) publicationOptions(opts []func(*s3.Options)) []func(*s3.Options) {
	if c.options.BeforePublish == nil {
		return opts
	}
	return append(opts, func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			// Last in Finalize places the check inside the SDK retry loop and as
			// close as possible to sending the request that publishes the key.
			return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("CheckBackupClock",
				func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
					if err := c.options.BeforePublish(); err != nil {
						return middleware.FinalizeOutput{}, middleware.Metadata{}, err
					}
					return next.HandleFinalize(ctx, in)
				}), middleware.After)
		})
	})
}

func (c *streamUploadClient) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if c.options.RequireAbsent {
		in.IfNoneMatch = aws.String("*")
	}
	return c.Client.PutObject(ctx, in, c.publicationOptions(opts)...)
}

func (c *streamUploadClient) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if c.options.RequireAbsent {
		in.IfNoneMatch = aws.String("*")
	}
	return c.Client.CompleteMultipartUpload(ctx, in, c.publicationOptions(opts)...)
}

func (c *streamUploadClient) UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	out, err := c.Client.UploadPart(ctx, in, opts...)
	if err != nil {
		// After SDK retries fail, interrupt a producer stalled between parts.
		// Otherwise the transfer manager can remain blocked reading the next part.
		c.cancel(err)
	}
	return out, err
}
