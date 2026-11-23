package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/tempfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Stage the finished stream so the part count is known and each SDK retry
// can seek directly into the file without buffering a part in memory.
func (s *S3) uploadFromDisk(ctx context.Context, key string, body io.Reader, condition *string) (err error) {
	f, err := tempfile.Open("", "mariadb-upload-*")
	if err != nil {
		return err
	}
	defer f.Close()
	digest := crc32.NewIEEE()
	size, err := io.Copy(io.MultiWriter(f, digest), body)
	if err != nil {
		return err
	}
	if size == 0 {
		return errors.New("no backup data to upload")
	}
	partSize := max(s.uploadPartSize, (size-1)/10000+1)
	if partSize > 5<<30 {
		return errors.New("backup exceeds S3 multipart object limit")
	}
	bucket, k := aws.String(s.bucket), aws.String(key)
	checksum := aws.String(base64.StdEncoding.EncodeToString(digest.Sum(nil)))
	if size <= partSize {
		out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: bucket, Key: k,
			Body: io.NewSectionReader(f, 0, size), ChecksumCRC32: checksum, IfNoneMatch: condition,
		})
		if err != nil {
			return err
		}
		return checkETag(out.ETag)
	}
	upload, err := checkedMultipartUpload(s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: k,
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeFullObject,
	}))
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, abortErr := s.client.AbortMultipartUpload(cleanup, &s3.AbortMultipartUploadInput{
				Bucket: bucket, Key: k, UploadId: upload.UploadId,
			})
			err = errors.Join(err, abortErr)
		}
	}()
	var parts []types.CompletedPart
	for offset := int64(0); offset < size; offset += partSize {
		num := aws.Int32(int32(len(parts) + 1))
		part, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: k, UploadId: upload.UploadId, PartNumber: num,
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
			Body:              io.NewSectionReader(f, offset, min(partSize, size-offset)),
		})
		if err != nil {
			return fmt.Errorf("upload part %d: %w", *num, err)
		}
		if err := checkETag(part.ETag); err != nil {
			return err
		}
		parts = append(parts, types.CompletedPart{PartNumber: num, ETag: part.ETag, ChecksumCRC32: part.ChecksumCRC32})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: k, UploadId: upload.UploadId, IfNoneMatch: condition,
		ChecksumType: types.ChecksumTypeFullObject, ChecksumCRC32: checksum,
		MpuObjectSize: aws.Int64(size), MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return err
	}
	return checkETag(out.ETag)
}
