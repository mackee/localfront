//go:build e2e

// Package e2e drives localfront as a black box: a built binary serving a real
// template against an S3-compatible store (RustFS) in a container.
//
// Run with:
//
//	go test -tags e2e ./e2e/...
//
// The store image is configurable via LOCALFRONT_E2E_S3_IMAGE (default
// rustfs/rustfs:latest), so a MinIO-compatible image can be substituted when
// the default is unavailable. Containers are managed with ory/dockertest; the
// whole package is behind the e2e build tag, so these dependencies are never
// compiled into the localfront binary or fetched by `go install`.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/ory/dockertest/v3"
	dc "github.com/ory/dockertest/v3/docker"
)

const (
	accessKey = "rustfsadmin"
	secretKey = "rustfsadmin"
	region    = "us-east-1"
)

// s3Image returns the store image repository and tag.
func s3Image() (repository, tag string) {
	image := os.Getenv("LOCALFRONT_E2E_S3_IMAGE")
	if image == "" {
		image = "rustfs/rustfs:latest"
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// store is a running S3-compatible object store.
type store struct {
	endpoint  string
	signer    *v4.Signer
	creds     aws.Credentials
	transport http.RoundTripper
}

// startStore launches the object store container with dockertest and waits
// until it accepts requests. It skips the test (not fails) when Docker is
// unavailable.
func startStore(t *testing.T) *store {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Skipf("docker not usable, skipping e2e: %v", err)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker daemon not reachable, skipping e2e: %v", err)
	}
	pool.MaxWait = 90 * time.Second

	repository, tag := s3Image()
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   repository,
		Tag:          tag,
		ExposedPorts: []string{"9000/tcp"},
		Env: []string{
			"RUSTFS_ACCESS_KEY=" + accessKey,
			"RUSTFS_SECRET_KEY=" + secretKey,
			"MINIO_ROOT_USER=" + accessKey,
			"MINIO_ROOT_PASSWORD=" + secretKey,
		},
	}, func(hc *dc.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = dc.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Fatalf("starting %s:%s: %v", repository, tag, err)
	}
	// Backstop: self-destruct if the test process dies without cleaning up.
	_ = resource.Expire(300)
	t.Cleanup(func() {
		_ = pool.Purge(resource)
	})

	s := &store{
		endpoint:  "http://" + hostPort(resource),
		signer:    v4.NewSigner(),
		creds:     aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		transport: http.DefaultTransport,
	}

	// A signed ListBuckets returns 200 only once the store has finished
	// initializing; during startup it answers 503 "Service not ready".
	if err := pool.Retry(func() error {
		status, err := s.signedStatus(http.MethodGet, "/")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("store not ready: status %d", status)
		}
		return nil
	}); err != nil {
		logs := containerLogs(pool, resource)
		t.Fatalf("object store did not become ready at %s: %v\n%s", s.endpoint, err, logs)
	}
	return s
}

// hostPort returns the reachable host:port for the store's 9000 port,
// normalizing the 0.0.0.0 binding that Docker reports on some platforms.
func hostPort(resource *dockertest.Resource) string {
	hp := resource.GetHostPort("9000/tcp")
	if strings.HasPrefix(hp, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(hp, "0.0.0.0:")
	}
	if hp == "" {
		return "127.0.0.1:" + resource.GetPort("9000/tcp")
	}
	return hp
}

func containerLogs(pool *dockertest.Pool, resource *dockertest.Resource) string {
	var buf bytes.Buffer
	_ = pool.Client.Logs(dc.LogsOptions{
		Container:    resource.Container.ID,
		OutputStream: &buf,
		ErrorStream:  &buf,
		Stdout:       true,
		Stderr:       true,
	})
	return buf.String()
}

// signedStatus sends a signed request to the given path and returns its status.
func (s *store) signedStatus(method, path string) (int, error) {
	req, err := http.NewRequest(method, s.endpoint+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)
	if err := s.signer.SignHTTP(context.Background(), s.creds, req, emptyPayload, "s3", region, time.Now().UTC()); err != nil {
		return 0, err
	}
	resp, err := s.transport.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// put signs and sends a PUT (bucket create when key is empty, object otherwise).
func (s *store) put(t *testing.T, bucket, key, contentType string, body []byte) {
	t.Helper()
	u := s.endpoint + "/" + bucket
	if key != "" {
		u += "/" + key
	}
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building PUT: %v", err)
	}
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	// Retry on 503: the store can report ready on ListBuckets a moment before
	// it accepts writes.
	var lastStatus int
	var lastMsg []byte
	for attempt := 0; attempt < 10; attempt++ {
		send := req.Clone(context.Background())
		send.ContentLength = int64(len(body))
		if len(body) == 0 {
			send.Body = http.NoBody // ensures Content-Length: 0 (S3 requires it)
		} else {
			send.Body = io.NopCloser(bytes.NewReader(body))
		}
		send.Header.Set("X-Amz-Content-Sha256", payloadHash)
		if err := s.signer.SignHTTP(context.Background(), s.creds, send, payloadHash, "s3", region, time.Now().UTC()); err != nil {
			t.Fatalf("signing PUT: %v", err)
		}
		resp, err := s.transport.RoundTrip(send)
		if err != nil {
			t.Fatalf("PUT %s failed: %v", u, err)
		}
		lastStatus = resp.StatusCode
		if resp.StatusCode < 300 {
			_ = resp.Body.Close()
			return
		}
		lastMsg, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("PUT %s returned %d: %s", u, lastStatus, lastMsg)
}

// putDir uploads every file under localDir into bucket, keyed by its relative
// path, guessing a Content-Type from the extension.
func (s *store) putDir(t *testing.T, bucket, localDir string) {
	t.Helper()
	s.put(t, bucket, "", "", nil) // create bucket
	err := filepath.WalkDir(localDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s.put(t, bucket, filepath.ToSlash(rel), contentTypeFor(rel), body)
		return nil
	})
	if err != nil {
		t.Fatalf("uploading %s: %v", localDir, err)
	}
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript"
	case strings.HasSuffix(name, ".css"):
		return "text/css"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// localfront is a running localfront subprocess.
type localfront struct {
	baseURL string
}

var dataPlaneLine = regexp.MustCompile(`data plane\s+(http://\S+)`)

// start builds and launches localfront against the given template and store.
func startLocalfront(t *testing.T, templatePath string, st *store) *localfront {
	t.Helper()
	bin := buildBinary(t)

	cmd := exec.Command(bin, "serve",
		"--template", templatePath,
		"--listen", "127.0.0.1:0",
		"--s3-endpoint", st.endpoint,
		"--s3-region", region,
		"--s3-access-key", accessKey,
		"--s3-secret-key", secretKey,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting localfront: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	baseURL := readDataPlaneURL(t, stdout, &stderr)
	lf := &localfront{baseURL: baseURL}
	lf.waitReady(t)
	return lf
}

func readDataPlaneURL(t *testing.T, stdout io.Reader, stderr *bytes.Buffer) string {
	t.Helper()
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 512)
		for {
			n, err := stdout.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if m := dataPlaneLine.FindSubmatch(buf); m != nil {
				ch <- result{url: string(m[1])}
				return
			}
			if err != nil {
				ch <- result{err: err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("localfront exited before serving: %v\nstderr:\n%s", r.err, stderr.String())
		}
		return r.url
	case <-time.After(15 * time.Second):
		t.Fatalf("localfront did not print its data plane URL\nstderr:\n%s", stderr.String())
		return ""
	}
}

func (lf *localfront) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, lf.baseURL+"/", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("localfront data plane did not become ready at %s", lf.baseURL)
}

// get issues a request to the data plane with the given Host and headers.
func (lf *localfront) get(t *testing.T, method, host, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, lf.baseURL+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "localfront")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/mackee/localfront/cmd/localfront")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building localfront: %v\n%s", err, out)
	}
	return bin
}

// repoFile resolves a path relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("resolving %s: %v", rel, err)
	}
	return abs
}

func mustReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return b
}

func fmtHeaders(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(v, ","))
	}
	return b.String()
}

var _ = fmtHeaders // used in failure messages by individual tests
