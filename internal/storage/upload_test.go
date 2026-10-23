package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type failingStream struct {
	io.Reader
	err error
}

func (r failingStream) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = r.err
	}
	return n, err
}

func TestStreamingUploadRejectsProducerFailure(t *testing.T) {
	for _, size := range []int{100, 12 * 1024 * 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var initiated, aborted, published atomic.Bool
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					initiated.Store(true)
					writeXML(w, `<InitiateMultipartUploadResult><UploadId>stream</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
					io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodDelete:
					aborted.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					published.Store(true)
					writeS3Error(w, http.StatusBadRequest, "UnexpectedPublication")
				}
			})
			sourceErr := errors.New("mariadb-dump failed after emitting data")
			body := io.NopCloser(failingStream{Reader: bytes.NewReader(make([]byte, size)), err: sourceErr})
			if err := s.Upload(t.Context(), "latest.dump", body, UploadOptions{}); !errors.Is(err, sourceErr) {
				t.Fatalf("producer failure was lost: %v", err)
			}
			if published.Load() || initiated.Load() != aborted.Load() || initiated.Load() != (size > 8*1024*1024) {
				t.Fatalf("initiated=%v aborted=%v published=%v", initiated.Load(), aborted.Load(), published.Load())
			}
		})
	}
}

// Some readers return their terminal error with the final bytes rather than
// on a separate read. The uploader must preserve both forms of producer failure.
type finalBytesReader struct {
	*bytes.Reader
	err error
}

func (r finalBytesReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.Len() == 0 {
		err = r.err
	}
	return n, err
}

func TestStreamingUploadSizeBoundary(t *testing.T) {
	const partSize = 5 * 1024 * 1024
	const limit = 2 * partSize
	sourceErr := errors.New("dump failed at the size limit")
	for _, test := range []struct {
		name           string
		size           int
		terminalErr    error
		withFinalBytes bool
	}{
		{name: "below limit", size: limit - 1},
		{name: "exact limit", size: limit},
		{name: "exact limit with final EOF", size: limit, terminalErr: io.EOF, withFinalBytes: true},
		{name: "one byte over", size: limit + 1},
		{name: "failure below limit", size: limit - 1, terminalErr: sourceErr},
		{name: "failure at limit", size: limit, terminalErr: sourceErr},
		{name: "failure with final bytes", size: limit, terminalErr: sourceErr, withFinalBytes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var initiated, aborted, published atomic.Bool
			var uploaded atomic.Int64
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				switch {
				case r.Method == http.MethodPost && q.Has("uploads"):
					initiated.Store(true)
					writeXML(w, `<InitiateMultipartUploadResult><UploadId>boundary</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && q.Get("uploadId") == "boundary":
					if q.Get("partNumber") != "1" && q.Get("partNumber") != "2" {
						t.Errorf("sent a part beyond the limit: %s", q.Get("partNumber"))
					}
					n, err := io.Copy(io.Discard, r.Body)
					if err != nil {
						t.Error(err)
					}
					uploaded.Add(n)
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodPost && q.Get("uploadId") == "boundary":
					published.Store(true)
					writeXML(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
				case r.Method == http.MethodDelete && q.Get("uploadId") == "boundary":
					aborted.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					writeS3Error(w, http.StatusBadRequest, "UnexpectedRequest")
				}
			})
			// Exercise the real SDK's multipart completion/abort path with two
			// parts, without sending 78 GiB in a unit test.
			s.uploadPartSize, s.maxUploadParts = partSize, 2
			payload := bytes.NewReader(make([]byte, test.size))
			var reader io.Reader = payload
			if test.withFinalBytes {
				reader = finalBytesReader{Reader: payload, err: test.terminalErr}
			} else if test.terminalErr != nil {
				reader = failingStream{Reader: payload, err: test.terminalErr}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := s.Upload(ctx, "latest.sql", io.NopCloser(reader), UploadOptions{})
			wantFailure := test.size > limit || test.terminalErr == sourceErr
			if (err != nil) != wantFailure || ctx.Err() != nil {
				t.Fatalf("upload error=%v, context=%v, want failure=%v", err, ctx.Err(), wantFailure)
			}
			if test.size > limit {
				for _, hint := range []string{"10485760 bytes", "S3_UPLOAD_PART_SIZE_MB=5", "increase S3_UPLOAD_PART_SIZE_MB"} {
					if !strings.Contains(err.Error(), hint) {
						t.Errorf("error missing %q: %v", hint, err)
					}
				}
			}
			if test.terminalErr == sourceErr && !errors.Is(err, sourceErr) {
				t.Fatalf("producer error was lost: %v", err)
			}
			if !initiated.Load() || aborted.Load() != wantFailure || published.Load() == wantFailure {
				t.Fatalf("initiated=%v aborted=%v published=%v", initiated.Load(), aborted.Load(), published.Load())
			}
			if uploaded.Load() > limit || (!wantFailure && uploaded.Load() != int64(test.size)) {
				t.Fatalf("uploaded %d bytes for %d-byte source", uploaded.Load(), test.size)
			}
		})
	}
}

func TestStreamingUploadPartFailureInterruptsBlockedProducer(t *testing.T) {
	var aborted atomic.Bool
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			writeXML(w, `<InitiateMultipartUploadResult><UploadId>stream</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			writeS3Error(w, http.StatusForbidden, "AccessDenied")
		case r.Method == http.MethodDelete:
			aborted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	reader, writer := io.Pipe()
	defer writer.Close()
	written := make(chan struct{})
	go func() {
		defer close(written)
		writer.Write(make([]byte, 8*1024*1024))
		// Leave the source open: the next read can finish only by cancellation.
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := s.Upload(ctx, "latest.dump", reader, UploadOptions{})
	<-written
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || ctx.Err() != nil || !aborted.Load() {
		t.Fatalf("err=%v outer context=%v aborted=%v", err, ctx.Err(), aborted.Load())
	}
}

func TestStreamingUploadCancellationInterruptsInitialRead(t *testing.T) {
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) { t.Error("empty stalled source accessed S3") })
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Upload(ctx, "key", reader, UploadOptions{}) }()
	if _, err := writer.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled stream remained blocked")
	}
}

func TestStreamingUploadRetriesBufferedPart(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var firstAttempt []byte
	var completed atomic.Bool
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			writeXML(w, `<InitiateMultipartUploadResult><UploadId>retry</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.URL.Query().Get("partNumber") == "1" {
				mu.Lock()
				defer mu.Unlock()
				attempts++
				if attempts == 1 {
					firstAttempt = body
					writeS3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
					return
				}
				if !bytes.Equal(body, firstAttempt) {
					t.Error("retry changed buffered stream data")
				}
			}
			w.Header().Set("ETag", `"part"`)
		case r.Method == http.MethodPost:
			completed.Store(true)
			writeXML(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		default:
			t.Errorf("unexpected retry request: %s %s", r.Method, r.URL)
		}
	})
	s.client = s3.New(s.client.Options(), func(o *s3.Options) {
		o.RetryMaxAttempts = 2
		o.Retryer = retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 2
			o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
		})
	})
	// NopCloser hides Seek: retries must use the SDK's bounded part buffer.
	body := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("data"), 3*1024*1024)))
	if err := s.Upload(t.Context(), "key", body, UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || !completed.Load() {
		t.Fatalf("first part attempts=%d completed=%v", attempts, completed.Load())
	}
}

func TestUploadChecksClockImmediatelyBeforePublication(t *testing.T) {
	for _, size := range []int{100, 6 * 1024 * 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var parts, aborts, publications atomic.Int32
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					writeXML(w, `<InitiateMultipartUploadResult><UploadId>clock</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
					io.Copy(io.Discard, r.Body)
					parts.Add(1)
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodDelete:
					aborts.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					publications.Add(1)
					writeS3Error(w, http.StatusBadRequest, "UnexpectedPublication")
				}
			})
			s.uploadPartSize = 5 * 1024 * 1024
			clockErr := errors.New("clock rolled back before publication")
			calls := 0
			body := bytes.NewBuffer(make([]byte, size)) // unknown length forces stream consumption
			err := s.Upload(t.Context(), "generation.sql", io.NopCloser(body), UploadOptions{
				RequireAbsent: true,
				BeforePublish: func() error {
					calls++
					if body.Len() != 0 {
						t.Error("clock checked before reading the complete stream")
					}
					if size > 5*1024*1024 && parts.Load() != 2 {
						t.Error("clock checked before all parts finished")
					}
					return clockErr
				},
			})
			if !errors.Is(err, clockErr) || calls != 1 || publications.Load() != 0 {
				t.Fatalf("err=%v checks=%d published=%d", err, calls, publications.Load())
			}
			wantAborts := int32(0)
			if size > 5*1024*1024 {
				wantAborts = 1
			}
			if aborts.Load() != wantAborts {
				t.Fatalf("aborts=%d", aborts.Load())
			}
		})
	}
}

func TestUploadConditionalPublication(t *testing.T) {
	for _, size := range []int{100, 6 * 1024 * 1024} {
		for _, mode := range []string{"new generation", "existing generation", "concurrent conflict", "unsupported condition", "fixed name"} {
			t.Run(fmt.Sprintf("%d/%s", size, mode), func(t *testing.T) {
				var commits, aborts atomic.Int32
				var replaced atomic.Bool
				requireAbsent := mode != "fixed name"
				status := map[string]int{"existing generation": 412, "concurrent conflict": 409, "unsupported condition": 501}[mode]
				s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
						writeXML(w, `<InitiateMultipartUploadResult><UploadId>collision</UploadId></InitiateMultipartUploadResult>`)
					case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
						io.Copy(io.Discard, r.Body)
						w.Header().Set("ETag", `"part"`)
					case r.Method == http.MethodDelete:
						aborts.Add(1)
						w.WriteHeader(http.StatusNoContent)
					case r.Method == http.MethodPut || r.Method == http.MethodPost:
						commits.Add(1)
						io.Copy(io.Discard, r.Body)
						want := ""
						if requireAbsent {
							want = "*"
						}
						if got := r.Header.Get("If-None-Match"); got != want {
							t.Errorf("If-None-Match=%q want %q", got, want)
						}
						if status > 0 && r.Header.Get("If-None-Match") == "*" {
							code := map[int]string{412: "PreconditionFailed", 409: "ConditionalRequestConflict", 501: "NotImplemented"}[status]
							writeS3Error(w, status, code)
							return
						}
						replaced.Store(true)
						if r.Method == http.MethodPost {
							writeXML(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
						} else {
							w.Header().Set("ETag", `"complete"`)
						}
					default:
						t.Errorf("unexpected request %s", r.Method)
						w.WriteHeader(http.StatusBadRequest)
					}
				})
				s.uploadPartSize = 5 * 1024 * 1024
				err := s.Upload(t.Context(), "same-key.sql", io.NopCloser(bytes.NewBuffer(make([]byte, size))), UploadOptions{RequireAbsent: requireAbsent})
				if (err != nil) != (status > 0) || replaced.Load() != (status == 0) {
					t.Fatalf("err=%v replaced=%v", err, replaced.Load())
				}
				if commits.Load() < 1 {
					t.Fatal("publication not attempted")
				}
				wantAborts := int32(0)
				if size > 5*1024*1024 && status > 0 {
					wantAborts = 1
				}
				if aborts.Load() != wantAborts {
					t.Fatalf("aborts=%d", aborts.Load())
				}
			})
		}
	}
}

func TestUploadRechecksClockBeforePublicationRetry(t *testing.T) {
	for _, size := range []int{100, 6 * 1024 * 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var attempts, aborts atomic.Int32
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					writeXML(w, `<InitiateMultipartUploadResult><UploadId>retry-clock</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
					io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"part"`)
				case r.Method == http.MethodDelete:
					aborts.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					io.Copy(io.Discard, r.Body)
					attempts.Add(1)
					writeS3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable")
				}
			})
			s.uploadPartSize = 5 * 1024 * 1024
			s.client = s3.New(s.client.Options(), func(o *s3.Options) {
				o.RetryMaxAttempts = 2
				o.Retryer = retry.NewStandard(func(o *retry.StandardOptions) {
					o.MaxAttempts = 2
					o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
				})
			})
			clockErr := errors.New("clock rolled back during retry backoff")
			checks := 0
			err := s.Upload(t.Context(), "generation.sql", io.NopCloser(bytes.NewBuffer(make([]byte, size))), UploadOptions{
				RequireAbsent: true,
				BeforePublish: func() error {
					checks++
					if attempts.Load() > 0 {
						return clockErr
					}
					return nil
				},
			})
			if !errors.Is(err, clockErr) || checks != 2 || attempts.Load() != 1 {
				t.Fatalf("err=%v checks=%d attempts=%d", err, checks, attempts.Load())
			}
			wantAborts := int32(0)
			if size > 5*1024*1024 {
				wantAborts = 1
			}
			if aborts.Load() != wantAborts {
				t.Fatalf("aborts=%d", aborts.Load())
			}
		})
	}
}
