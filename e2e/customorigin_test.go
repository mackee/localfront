//go:build e2e

package e2e

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// echoOrigin is a custom HTTP origin that echoes the request path in the body
// and mirrors selected request headers back as X-Echo-* response headers, so
// tests can assert what reached the origin. It answers OPTIONS with 204.
func echoOrigin(t *testing.T) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.Header().Set("X-Echo-Country", r.Header.Get("CloudFront-Viewer-Country"))
		w.Header().Set("X-Echo-Variant", r.Header.Get("X-Variant"))
		w.Header().Set("Content-Type", "text/plain")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("origin:" + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	h, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split origin addr: %v", err)
	}
	port, _ = strconv.Atoi(p)
	return h, port
}

// writeTemplate substitutes ORIGIN_HOST / ORIGIN_PORT in tmpl and writes it to
// a temp file, returning the path.
func writeTemplate(t *testing.T, tmpl, host string, port int) string {
	t.Helper()
	content := strings.NewReplacer("ORIGIN_HOST", host, "ORIGIN_PORT", strconv.Itoa(port)).Replace(tmpl)
	path := filepath.Join(t.TempDir(), "template.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return path
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// Scenario 3: CloudFront Functions — URL normalization, redirect, and a KVS
// feature flag, exercised through the real binary.
func TestScenario_Functions(t *testing.T) {
	host, port := echoOrigin(t)
	const tmpl = `Resources:
  Flags:
    Type: AWS::CloudFront::KeyValueStore
    Properties:
      Name: feature-flags
  Rewrite:
    Type: AWS::CloudFront::Function
    Properties:
      Name: rewrite
      FunctionConfig:
        Runtime: cloudfront-js-2.0
        Comment: url normalization + flag
        KeyValueStoreAssociations:
          - KeyValueStoreARN: !GetAtt Flags.Arn
      FunctionCode: |
        import cf from 'cloudfront';
        var kvs = cf.kvs();
        async function handler(event) {
          var req = event.request;
          if (req.uri === '/old') {
            return { statusCode: 301, statusDescription: 'Moved', headers: { location: { value: '/new' } } };
          }
          if (req.uri.slice(-1) === '/') { req.uri += 'index.html'; }
          var variant = 'default';
          try { variant = await kvs.get('variant'); } catch (e) {}
          req.headers['x-variant'] = { value: variant };
          return req;
        }
  Dist:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases: [ app.example.test ]
        Origins:
          - Id: o1
            DomainName: ORIGIN_HOST
            CustomOriginConfig:
              HTTPPort: ORIGIN_PORT
              OriginProtocolPolicy: http-only
        DefaultCacheBehavior:
          TargetOriginId: o1
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 4135ea2d-6df8-44a3-9df3-4b5a84be39ad
          OriginRequestPolicyId: 216adef6-5c7f-47e4-b989-5492eafa07d3
          FunctionAssociations:
            - EventType: viewer-request
              FunctionARN: !GetAtt Rewrite.FunctionARN
`
	templatePath := writeTemplate(t, tmpl, host, port)
	seedPath := writeFile(t, "flags.json", `{"data":[{"key":"variant","value":"beta"}]}`)
	lf := startLocalfrontArgs(t, "serve",
		"--template", templatePath,
		"--listen", "127.0.0.1:0",
		"--kvs-seed", "feature-flags="+seedPath,
	)
	const host2 = "app.example.test"

	t.Run("directory URI is normalized", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host2, "/docs/", nil)
		_ = mustReadBody(t, resp)
		if got := resp.Header.Get("X-Echo-Path"); got != "/docs/index.html" {
			t.Errorf("origin path = %q, want /docs/index.html", got)
		}
	})

	t.Run("redirect short-circuits the origin", func(t *testing.T) {
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequest(http.MethodGet, lf.baseURL+"/old", nil)
		req.Host = host2
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /old: %v", err)
		}
		_ = mustReadBody(t, resp)
		if resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("status = %d, want 301", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/new" {
			t.Errorf("Location = %q, want /new", loc)
		}
	})

	t.Run("KVS feature flag reaches the origin", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host2, "/page", nil)
		_ = mustReadBody(t, resp)
		if got := resp.Header.Get("X-Echo-Variant"); got != "beta" {
			t.Errorf("origin X-Variant = %q, want beta (from KVS seed)", got)
		}
	})
}

// Scenario 6: CORS preflight + security headers via a response headers policy.
func TestScenario_CORSAndSecurityHeaders(t *testing.T) {
	host, port := echoOrigin(t)
	const tmpl = `Resources:
  Dist:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases: [ api.example.test ]
        Origins:
          - Id: o1
            DomainName: ORIGIN_HOST
            CustomOriginConfig:
              HTTPPort: ORIGIN_PORT
              OriginProtocolPolicy: http-only
        DefaultCacheBehavior:
          TargetOriginId: o1
          ViewerProtocolPolicy: allow-all
          AllowedMethods: [ GET, HEAD, OPTIONS ]
          CachePolicyId: 4135ea2d-6df8-44a3-9df3-4b5a84be39ad
          ResponseHeadersPolicyId: eaab4381-ed33-4a86-88ca-d9558dc6cd63
`
	templatePath := writeTemplate(t, tmpl, host, port)
	lf := startLocalfrontArgs(t, "serve", "--template", templatePath, "--listen", "127.0.0.1:0")
	const host2 = "api.example.test"

	t.Run("preflight", func(t *testing.T) {
		resp := lf.get(t, http.MethodOptions, host2, "/data", map[string]string{
			"Origin":                        "https://app.example.test",
			"Access-Control-Request-Method": "GET",
		})
		_ = mustReadBody(t, resp)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}
	})

	t.Run("actual request has CORS and security headers", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host2, "/data", map[string]string{
			"Origin": "https://app.example.test",
		})
		_ = mustReadBody(t, resp)
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
		}
		if got := resp.Header.Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=") {
			t.Errorf("Strict-Transport-Security = %q, want an HSTS header", got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
	})
}

// Scenario 9 & 10: multi-distribution host routing and per-request viewer
// header overrides reaching both the origin via an AllViewerAndCloudFront ORP.
func TestScenario_MultiDistributionAndGeo(t *testing.T) {
	host, port := echoOrigin(t)
	const tmpl = `Resources:
  SiteA:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases: [ a.example.test ]
        Origins:
          - Id: o1
            DomainName: ORIGIN_HOST
            CustomOriginConfig:
              HTTPPort: ORIGIN_PORT
              OriginProtocolPolicy: http-only
        DefaultCacheBehavior:
          TargetOriginId: o1
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 4135ea2d-6df8-44a3-9df3-4b5a84be39ad
          OriginRequestPolicyId: 33f36d7e-f396-46d9-90e0-52428a34d9dc
  SiteB:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases: [ b.example.test ]
        Origins:
          - Id: o1
            DomainName: ORIGIN_HOST
            CustomOriginConfig:
              HTTPPort: ORIGIN_PORT
              OriginProtocolPolicy: http-only
        DefaultCacheBehavior:
          TargetOriginId: o1
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 4135ea2d-6df8-44a3-9df3-4b5a84be39ad
`
	templatePath := writeTemplate(t, tmpl, host, port)
	lf := startLocalfrontArgs(t, "serve", "--template", templatePath, "--listen", "127.0.0.1:0")

	t.Run("host routing distinguishes distributions", func(t *testing.T) {
		respA := lf.get(t, http.MethodGet, "a.example.test", "/a", nil)
		bodyA := string(mustReadBody(t, respA))
		if respA.StatusCode != http.StatusOK || bodyA != "origin:/a" {
			t.Errorf("a.example.test/a = %d %q", respA.StatusCode, bodyA)
		}
		respB := lf.get(t, http.MethodGet, "b.example.test", "/b", nil)
		bodyB := string(mustReadBody(t, respB))
		if respB.StatusCode != http.StatusOK || bodyB != "origin:/b" {
			t.Errorf("b.example.test/b = %d %q", respB.StatusCode, bodyB)
		}
	})

	t.Run("unknown host is denied", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, "unknown.example.test", "/", nil)
		_ = mustReadBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("unknown host = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("viewer country override reaches the origin", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, "a.example.test", "/geo", map[string]string{
			"X-Localfront-Viewer-Country": "JP",
		})
		_ = mustReadBody(t, resp)
		if got := resp.Header.Get("X-Echo-Country"); got != "JP" {
			t.Errorf("origin CloudFront-Viewer-Country = %q, want JP", got)
		}
	})
}
