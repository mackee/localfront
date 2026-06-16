package origin

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureTransport is a RoundTripper that records the outgoing request and
// returns a canned response.
type captureTransport struct {
	captured *http.Request
	status   int
	headers  http.Header
	body     string
	err      error
}

func (ct *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.captured = req.Clone(req.Context())
	if ct.err != nil {
		return nil, ct.err
	}
	code := ct.status
	if code == 0 {
		code = 200
	}
	resp := &http.Response{
		StatusCode: code,
		Header:     ct.headers.Clone(),
		Body:       io.NopCloser(strings.NewReader(ct.body)),
		Request:    req,
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	return resp, nil
}

// ─────────────────────────────────────────────────
// A2: NewS3Client validation
// ─────────────────────────────────────────────────

func TestNewS3Client_EmptyEndpoint(t *testing.T) {
	_, err := NewS3Client("", "us-east-1", "ak", "sk", nil)
	if err == nil {
		t.Fatal("expected error for empty endpoint, got nil")
	}
}

func TestNewS3Client_NoScheme(t *testing.T) {
	_, err := NewS3Client("localhost:9000", "us-east-1", "ak", "sk", nil)
	if err == nil {
		t.Fatal("expected error for endpoint without scheme, got nil")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("error message does not mention expected URL form: %v", err)
	}
}

func TestNewS3Client_ValidEndpoint(t *testing.T) {
	c, err := NewS3Client("http://localhost:9000", "us-east-1", "ak", "sk", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewS3Client_EmptyRegionDefaultsToUSEast1(t *testing.T) {
	ct := &captureTransport{status: 200, body: ""}
	c, err := NewS3Client("http://localhost:9000", "", "ak", "sk", ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp, err := c.Fetch(context.Background(), &Request{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_ = resp.Body.Close()

	auth := ct.captured.Header.Get("Authorization")
	if !strings.Contains(auth, "/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization header does not contain /us-east-1/s3/aws4_request: %q", auth)
	}
}

// ─────────────────────────────────────────────────
// A3: Fetch signing & headers
// ─────────────────────────────────────────────────

func TestFetch_SigningHeaders(t *testing.T) {
	ct := &captureTransport{status: 200, body: ""}
	c, err := NewS3Client("http://store.example.test:9000", "ap-northeast-1", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", ct)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}

	req := &Request{
		Bucket:  "mybucket",
		Key:     "path/to/file.txt",
		Method:  "GET",
		Headers: http.Header{"Range": []string{"bytes=0-99"}, "If-None-Match": []string{`"abc123"`}},
	}
	resp, err := c.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	_ = resp.Body.Close()

	out := ct.captured

	// method
	if out.Method != "GET" {
		t.Errorf("method = %q, want GET", out.Method)
	}

	// path-style URL
	wantPath := "/mybucket/path/to/file.txt"
	if out.URL.Path != wantPath {
		t.Errorf("URL path = %q, want %q", out.URL.Path, wantPath)
	}

	// Authorization header
	auth := out.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization does not start with AWS4-HMAC-SHA256: %q", auth)
	}
	if !strings.Contains(auth, "/ap-northeast-1/s3/aws4_request") {
		t.Errorf("Authorization missing /ap-northeast-1/s3/aws4_request: %q", auth)
	}

	// X-Amz-Content-Sha256
	if got := out.Header.Get("X-Amz-Content-Sha256"); got != emptyPayloadHash {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, emptyPayloadHash)
	}

	// X-Amz-Date present
	if out.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}

	// forwarded headers present
	if out.Header.Get("Range") != "bytes=0-99" {
		t.Errorf("Range = %q, want bytes=0-99", out.Header.Get("Range"))
	}
	if out.Header.Get("If-None-Match") != `"abc123"` {
		t.Errorf("If-None-Match = %q, want \"abc123\"", out.Header.Get("If-None-Match"))
	}
}

func TestFetch_HEADMethod(t *testing.T) {
	ct := &captureTransport{status: 200, body: ""}
	c, err := NewS3Client("http://store.example.test:9000", "us-east-1", "ak", "sk", ct)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	resp, err := c.Fetch(context.Background(), &Request{Bucket: "b", Key: "k", Method: "HEAD"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	_ = resp.Body.Close()
	if ct.captured.Method != "HEAD" {
		t.Errorf("method = %q, want HEAD", ct.captured.Method)
	}
}

func TestFetch_EmptyMethodDefaultsToGET(t *testing.T) {
	ct := &captureTransport{status: 200, body: ""}
	c, err := NewS3Client("http://store.example.test:9000", "us-east-1", "ak", "sk", ct)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	resp, err := c.Fetch(context.Background(), &Request{Bucket: "b", Key: "k", Method: ""})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	_ = resp.Body.Close()
	if ct.captured.Method != "GET" {
		t.Errorf("method = %q, want GET", ct.captured.Method)
	}
}

// ─────────────────────────────────────────────────
// A4: Fetch response passthrough & error handling
// ─────────────────────────────────────────────────

func TestFetch_ResponsePassthrough(t *testing.T) {
	h := make(http.Header)
	h.Set("Content-Range", "bytes 0-99/200")
	ct := &captureTransport{
		status:  206,
		headers: h,
		body:    "partial content",
	}
	c, err := NewS3Client("http://store.example.test:9000", "us-east-1", "ak", "sk", ct)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	resp, err := c.Fetch(context.Background(), &Request{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 206 {
		t.Errorf("StatusCode = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 0-99/200" {
		t.Errorf("Content-Range = %q, want bytes 0-99/200", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "partial content" {
		t.Errorf("body = %q, want partial content", string(body))
	}
}

func TestFetch_TransportError(t *testing.T) {
	ct := &captureTransport{err: errors.New("connection refused")}
	c, err := NewS3Client("http://store.example.test:9000", "us-east-1", "ak", "sk", ct)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	_, err = c.Fetch(context.Background(), &Request{Bucket: "my-bucket", Key: "k"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "my-bucket") {
		t.Errorf("error does not mention bucket name: %v", err)
	}
}

// ─────────────────────────────────────────────────
// A5: CollectForwardedHeaders
// ─────────────────────────────────────────────────

func TestCollectForwardedHeaders(t *testing.T) {
	tests := []struct {
		name   string
		input  http.Header
		want   http.Header // nil means expect nil result
		noKeys []string    // keys that must NOT be present
	}{
		{
			name: "picks up all 5 forwarded headers (mixed case input)",
			input: http.Header{
				"Range":               []string{"bytes=0-499"},
				"If-Match":            []string{`"etag1"`},
				"If-None-Match":       []string{`"etag2"`},
				"If-Modified-Since":   []string{"Mon, 01 Jan 2024 00:00:00 GMT"},
				"If-Unmodified-Since": []string{"Tue, 02 Jan 2024 00:00:00 GMT"},
			},
			want: http.Header{
				"Range":               []string{"bytes=0-499"},
				"If-Match":            []string{`"etag1"`},
				"If-None-Match":       []string{`"etag2"`},
				"If-Modified-Since":   []string{"Mon, 01 Jan 2024 00:00:00 GMT"},
				"If-Unmodified-Since": []string{"Tue, 02 Jan 2024 00:00:00 GMT"},
			},
		},
		{
			name: "drops non-forwarded headers",
			input: http.Header{
				"Range":         []string{"bytes=0-99"},
				"Authorization": []string{"Bearer token"},
				"Cookie":        []string{"session=abc"},
				"User-Agent":    []string{"curl/7.0"},
				"Host":          []string{"example.com"},
			},
			want:   http.Header{"Range": []string{"bytes=0-99"}},
			noKeys: []string{"Authorization", "Cookie", "User-Agent", "Host"},
		},
		{
			name: "returns nil when no forwarded headers present",
			input: http.Header{
				"Authorization": []string{"Bearer token"},
				"Cookie":        []string{"session=abc"},
			},
			want: nil,
		},
		{
			name:  "empty input returns nil",
			input: http.Header{},
			want:  nil,
		},
		{
			name: "lowercase input key canonicalized via http.Header.Add",
			input: func() http.Header {
				h := make(http.Header)
				h.Add("range", "bytes=0-9") // Add() canonicalizes the key to "Range"
				return h
			}(),
			want: http.Header{"Range": []string{"bytes=0-9"}},
		},
		{
			name: "preserves multiple values for a header",
			input: http.Header{
				"If-None-Match": []string{`"etag1"`, `"etag2"`, `"etag3"`},
			},
			want: http.Header{"If-None-Match": []string{`"etag1"`, `"etag2"`, `"etag3"`}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CollectForwardedHeaders(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			for key, wantVals := range tc.want {
				gotVals := got[key]
				if len(gotVals) != len(wantVals) {
					t.Errorf("header %q: got %v, want %v", key, gotVals, wantVals)
					continue
				}
				for i, v := range wantVals {
					if gotVals[i] != v {
						t.Errorf("header %q[%d]: got %q, want %q", key, i, gotVals[i], v)
					}
				}
			}
			for _, k := range tc.noKeys {
				if _, present := got[k]; present {
					t.Errorf("header %q should not be present in forwarded headers", k)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────
// Optional: live httptest.Server round-trip
// ─────────────────────────────────────────────────

func TestFetch_LiveRoundTrip(t *testing.T) {
	var capturedPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "live response")
	}))
	defer ts.Close()

	// transport=nil → uses http.DefaultTransport (real network to test server)
	c, err := NewS3Client(ts.URL, "us-east-1", "ak", "sk", nil)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}

	resp, err := c.Fetch(context.Background(), &Request{Bucket: "mybucket", Key: "mykey.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "live response") {
		t.Errorf("unexpected body: %q", string(body))
	}
	if capturedPath != "/mybucket/mykey.txt" {
		t.Errorf("server saw path %q, want /mybucket/mykey.txt", capturedPath)
	}
}

// A gzip-compressed object must be forwarded verbatim. When transport is nil
// the client uses a clone of http.DefaultTransport, which (unless compression is
// disabled) would auto-add Accept-Encoding: gzip and transparently decompress
// the response, replacing the object's stored bytes.
func TestFetch_GzipObjectForwardedVerbatim(t *testing.T) {
	var sawAcceptEncoding string
	var gzipped bytes.Buffer
	gw := gzip.NewWriter(&gzipped)
	_, _ = io.WriteString(gw, "compressed payload")
	_ = gw.Close()
	stored := gzipped.Bytes()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stored)
	}))
	defer ts.Close()

	// transport=nil → real DefaultTransport clone with compression disabled.
	c, err := NewS3Client(ts.URL, "us-east-1", "ak", "sk", nil)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	resp, err := c.Fetch(context.Background(), &Request{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if sawAcceptEncoding != "" {
		t.Errorf("origin saw Accept-Encoding %q, want empty (transport must not inject gzip)", sawAcceptEncoding)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip (the stored object must be forwarded verbatim)", ce)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, stored) {
		t.Errorf("body was altered by the transport: got %d bytes, want the %d stored gzip bytes", len(body), len(stored))
	}
}

// Ensure captureTransport satisfies the interface at compile time.
var _ http.RoundTripper = (*captureTransport)(nil)

// Ensure S3Client satisfies Fetcher at compile time.
var _ Fetcher = (*S3Client)(nil)

// silence unused import warning in case bytes is used only indirectly
var _ = bytes.NewReader
