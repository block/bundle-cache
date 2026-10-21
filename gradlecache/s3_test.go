package gradlecache

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

type retryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f retryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type retryBody struct {
	io.Reader
	closed bool
}

func (b *retryBody) Close() error { b.closed = true; return nil }

func TestS3DownloadRetries(t *testing.T) {
	for _, operation := range []string{"head", "get", "range"} {
		for _, tc := range []struct {
			name     string
			status   int
			failures int
			attempts int
			wantErr  bool
		}{
			{"500", 500, 2, 3, false},
			{"502", 502, 2, 3, false},
			{"503", 503, 2, 3, false},
			{"504", 504, 2, 3, false},
			{"exhausted", 503, 3, 3, true},
			{"forbidden", 403, 1, 1, true},
			{"missing", 404, 1, 1, true},
			{"not-implemented", 501, 1, 1, true},
			{"network", 0, 1, 1, true},
			{"truncated", 200, 0, 1, true},
		} {
			if operation == "head" && tc.name == "truncated" {
				continue
			}
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					payload := bytes.Repeat([]byte("abc"), 43)
					var times []time.Time
					var bodies []*retryBody
					client := &s3Client{region: "us-east-1", creds: awsCreds{AccessKeyID: "test-id", SecretAccessKey: "test-secret"}, chunkSize: 1024, dlWorkers: 1}
					if operation == "range" {
						client.chunkSize = 64
					}
					client.http = &http.Client{Transport: retryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-id/") || strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=authorization") {
							t.Error("retry was not freshly signed")
						}
						data, status := payload, http.StatusOK
						first := r.Header.Get("Range") == "" || r.Header.Get("Range") == "bytes=0-63"
						if first {
							times = append(times, time.Now())
							if tc.status == 0 {
								return nil, errors.New("network failure")
							}
						}
						if r.Header.Get("Range") != "" {
							if r.Header.Get("If-Match") != `"etag"` {
								t.Error("range lost its ETag precondition")
							}
							var start, end int
							_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
							data, status = payload[start:end+1], http.StatusPartialContent
						}
						if first && len(times) <= tc.failures {
							data, status = []byte("final server failure"), tc.status
						}
						body := &retryBody{Reader: bytes.NewReader(data)}
						if tc.name == "truncated" {
							body.Reader = io.MultiReader(bytes.NewReader(data[:len(data)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
						}
						bodies = append(bodies, body)
						return &http.Response{StatusCode: status, ContentLength: int64(len(data)), Body: body, Header: http.Header{"Etag": {`"etag"`}, "X-Amz-Meta-Kind": {"bundle"}}}, nil
					})}
					var err error
					if operation == "head" {
						var info s3ObjInfo
						info, err = client.stat(context.Background(), "bucket", "key")
						if err == nil && (info.Size != int64(len(payload)) || info.ETag != `"etag"` || info.Metadata["kind"] != "bundle") {
							t.Fatalf("incorrect metadata: %+v", info)
						}
					} else {
						var r io.ReadCloser
						r, err = client.get(context.Background(), "bucket", "key", s3ObjInfo{Size: int64(len(payload)), ETag: `"etag"`})
						if err == nil {
							var got []byte
							got, err = io.ReadAll(r)
							_ = r.Close()
							if err == nil && !bytes.Equal(got, payload) {
								t.Fatal("incorrect downloaded bytes")
							}
						}
					}
					if (err != nil) != tc.wantErr || len(times) != tc.attempts {
						t.Fatalf("error=%v, attempts=%d, want error=%v and attempts=%d", err, len(times), tc.wantErr, tc.attempts)
					}
					if tc.name == "exhausted" && operation != "head" && !strings.Contains(err.Error(), "final server failure") {
						t.Fatalf("lost final response: %v", err)
					}
					for i := 1; i < len(times); i++ {
						elapsed := times[i].Sub(times[i-1])
						lower, upper := 125*time.Millisecond, 250*time.Millisecond
						if i == 2 {
							lower, upper = 250*time.Millisecond, 500*time.Millisecond
						}
						if elapsed < lower || elapsed > upper {
							t.Errorf("retry %d waited %v, want [%v, %v]", i, elapsed, lower, upper)
						}
					}
					for _, body := range bodies {
						if !body.closed {
							t.Error("response body was not closed")
						}
					}
				})
			})
		}
	}
}

func TestS3DownloadRetryCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		client := &s3Client{http: &http.Client{Transport: retryRoundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("busy"))}, nil
		})}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := client.stat(ctx, "bucket", "key"); done <- err }()
		synctest.Wait() // The request finished and the retry is sleeping.
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || attempts != 1 {
				t.Fatalf("error=%v attempts=%d, want cancellation after one attempt", err, attempts)
			}
		default:
			t.Fatal("retry did not stop on cancellation")
		}
	})
}

func TestS3UploadRetries(t *testing.T) {
	for _, operation := range []string{"put", "initiate", "part", "complete", "abort"} {
		for _, tc := range []struct {
			name     string
			status   int
			failures int
			attempts int
			wantErr  bool
		}{
			{"recover", 503, 2, 3, false},
			{"exhausted", 500, 3, 3, true},
			{"forbidden", 403, 1, 1, true},
			{"network", 0, 1, 1, true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					payload := []byte("an asymmetric upload payload\nwith a second line")
					var attempts int
					var previous io.ReadCloser
					var checksum string
					client := &s3Client{region: "us-east-1", creds: awsCreds{AccessKeyID: "test-id", SecretAccessKey: "test-secret"}}
					client.http = &http.Client{Transport: retryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						attempts++
						if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-id/") || strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=authorization") {
							t.Error("upload retry was not freshly signed")
						}
						if previous != nil {
							// A transport may still hold the prior request body after an
							// early response. It must not consume the rewound seeker.
							if n, err := previous.Read(make([]byte, 1)); n != 0 || err == nil {
								t.Error("old upload body can still consume the next attempt")
							}
						}
						if operation == "put" || operation == "part" {
							previous = r.Body
							if attempts == 1 {
								checksum = r.Header.Get("X-Amz-Checksum-Crc64nvme")
							}
							if checksum == "" || r.Header.Get("X-Amz-Checksum-Crc64nvme") != checksum || r.ContentLength != int64(len(payload)) {
								t.Error("upload retry lost checksum or content length")
							}
							// Fail after consuming a prefix, not only after the full body.
							var got []byte
							var err error
							if attempts <= tc.failures {
								got, err = io.ReadAll(io.LimitReader(r.Body, 7))
							} else {
								got, err = io.ReadAll(r.Body)
							}
							must(t, err)
							want := payload
							if attempts <= tc.failures {
								want = payload[:7]
							}
							if !bytes.Equal(got, want) {
								t.Fatalf("upload body was not replayed: got %q, want %q", got, want)
							}
						}
						if operation == "put" || operation == "initiate" {
							if r.Header.Get("Content-Type") != "application/zstd" || r.Header.Get("X-Amz-Meta-Base-Commit") != "abc123" {
								t.Error("upload retry lost metadata or content type")
							}
						}
						if operation == "part" || operation == "complete" || operation == "abort" {
							if r.URL.Query().Get("uploadId") != "id+with/slash" {
								t.Error("upload retry changed the upload ID")
							}
						}
						if operation == "complete" {
							var request struct {
								Parts []struct {
									PartNumber int
									ETag       string
								} `xml:"Part"`
							}
							must(t, xml.NewDecoder(r.Body).Decode(&request))
							if len(request.Parts) != 1 || request.Parts[0].PartNumber != 7 || request.Parts[0].ETag != "part-etag" {
								t.Fatalf("completion body changed: %+v", request)
							}
						}
						if tc.status == 0 {
							return nil, errors.New("network failure")
						}
						status, body := 200, ""
						if attempts <= tc.failures {
							status, body = tc.status, "server failure"
						} else if operation == "initiate" {
							body = "<InitiateMultipartUploadResult><UploadId>id+with/slash</UploadId></InitiateMultipartUploadResult>"
						}
						return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Etag": {"part-etag"}}}, nil
					})}
					ctx := context.Background()
					var err error
					switch operation {
					case "put":
						err = client.put(ctx, "bucket", "key", bytes.NewReader(payload), int64(len(payload)), "application/zstd", map[string]string{"base-commit": "abc123"})
					case "initiate":
						_, err = client.createMultipartUpload(ctx, "bucket", "key", "application/zstd", map[string]string{"base-commit": "abc123"})
					case "part":
						_, err = client.uploadPart(ctx, "bucket", "key", "id+with/slash", 7, bytes.NewReader(payload), int64(len(payload)))
					case "complete":
						err = client.completeMultipartUpload(ctx, "bucket", "key", "id+with/slash", []struct {
							PartNumber int
							ETag       string
						}{{7, "part-etag"}})
					case "abort":
						err = client.abortMultipartUpload(ctx, "bucket", "key", "id+with/slash")
					}
					if (err != nil) != tc.wantErr || attempts != tc.attempts {
						t.Fatalf("error=%v attempts=%d, want error=%v attempts=%d", err, attempts, tc.wantErr, tc.attempts)
					}
				})
			})
		}
	}
}

func TestS3UploadRetryKeepsFileOpen(t *testing.T) {
	payload := bytes.Repeat([]byte("file upload\n"), 1000)
	file, err := os.CreateTemp(t.TempDir(), "upload")
	must(t, err)
	defer file.Close() //nolint:errcheck
	_, err = file.Write(payload)
	must(t, err)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("attempt %d: bytes=%d error=%v", attempt, len(got), err)
		}
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	_, err = file.Seek(0, io.SeekStart)
	must(t, err)
	client := newTestS3BundleStore(server).client
	must(t, client.put(context.Background(), "bucket", "key", file, int64(len(payload)), "", nil))
	_, err = file.Seek(0, io.SeekStart)
	must(t, err)
	got, err := io.ReadAll(file)
	must(t, err)
	if attempts.Load() != 2 || !bytes.Equal(got, payload) {
		t.Fatal("file was closed or changed by upload retries")
	}
}
