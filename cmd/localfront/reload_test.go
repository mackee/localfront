package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mackee/localfront/internal/dataplane"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const reloadTemplate = `Resources:
  Dist:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases:
          - %s
        Origins:
          - Id: o1
            DomainName: %s
            CustomOriginConfig:
              HTTPPort: %d
              OriginProtocolPolicy: http-only
        DefaultCacheBehavior:
          TargetOriginId: o1
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`

// The broken template is valid YAML but fails config build (no origins).
const brokenTemplate = `Resources:
  Dist:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        DefaultCacheBehavior:
          TargetOriginId: o1
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`

func statusFor(t *testing.T, server *dataplane.Server, host string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	req.Host = host
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	return rr.Code
}

func TestReload_KeepsOldConfigOnError(t *testing.T) {
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer originSrv.Close()
	host, portStr, _ := net.SplitHostPort(originSrv.Listener.Addr().String())
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "template.yaml")
	write := func(content string) {
		if err := os.WriteFile(tmplPath, []byte(content), 0o600); err != nil {
			t.Fatalf("write template: %v", err)
		}
	}
	write(fmt.Sprintf(reloadTemplate, "a.example.test", host, port))

	logger := testLogger()
	opts := &serveOptions{templates: []string{tmplPath}, parameters: map[string]string{}, kvsSeeds: map[string]string{}}
	cfg, err := loadConfig(opts, logger)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	server := dataplane.New(cfg, logger)
	rl := &reloader{opts: opts, server: server, logger: logger}

	if got := statusFor(t, server, "a.example.test"); got != http.StatusOK {
		t.Fatalf("initial a.example.test = %d, want 200", got)
	}

	// A broken template must fail reload and leave the old config serving.
	write(brokenTemplate)
	if err := rl.reload(); err == nil {
		t.Fatal("reload of a broken template should fail")
	}
	if got := statusFor(t, server, "a.example.test"); got != http.StatusOK {
		t.Errorf("after failed reload a.example.test = %d, want 200 (old config kept)", got)
	}

	// A valid template with a new alias should apply.
	write(fmt.Sprintf(reloadTemplate, "b.example.test", host, port))
	if err := rl.reload(); err != nil {
		t.Fatalf("reload of a valid template: %v", err)
	}
	if got := statusFor(t, server, "b.example.test"); got != http.StatusOK {
		t.Errorf("after reload b.example.test = %d, want 200", got)
	}
	if got := statusFor(t, server, "a.example.test"); got != http.StatusForbidden {
		t.Errorf("after reload a.example.test = %d, want 403 (alias removed)", got)
	}
}
