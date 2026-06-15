//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestSPAHosting exercises the spa-hosting example end to end against a real
// object store: default root object, nested assets with the right
// Content-Type, a miss, and HEAD. The SPA fallback (403/404 -> /index.html)
// is asserted once custom error responses land in M4.
func TestSPAHosting(t *testing.T) {
	st := startStore(t)
	st.putDir(t, "spa-assets", repoFile(t, "examples/spa-hosting/dist"))
	lf := startLocalfront(t, repoFile(t, "examples/spa-hosting/template.yaml"), st)

	const host = "spa.example.test"

	t.Run("default root object", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host, "/", nil)
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / = %d, want 200\n%s", resp.StatusCode, fmtHeaders(resp.Header))
		}
		if !bytes.Contains(body, []byte("<div id=\"root\">")) {
			t.Errorf("GET / did not return index.html, got:\n%s", body)
		}
		if got := resp.Header.Get("X-Cache"); got != "Miss from localfront" {
			t.Errorf("X-Cache = %q, want Miss from localfront", got)
		}
		if resp.Header.Get("X-Amz-Cf-Id") == "" {
			t.Error("missing X-Amz-Cf-Id")
		}
	})

	t.Run("nested asset content-type", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host, "/assets/app.js", nil)
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /assets/app.js = %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Errorf("Content-Type = %q, want a javascript type", ct)
		}
		if !bytes.Contains(body, []byte("Hello from the localfront SPA example")) {
			t.Errorf("unexpected app.js body:\n%s", body)
		}
	})

	t.Run("missing asset 404s (has a file extension)", func(t *testing.T) {
		// /does-not-exist.js matches no behavior-specific error rewrite; the SPA
		// fallback still applies (404 -> /index.html, 200) per the template.
		resp := lf.get(t, http.MethodGet, host, "/does-not-exist.js", nil)
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET missing = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if !bytes.Contains(body, []byte("<div id=\"root\">")) {
			t.Errorf("SPA fallback did not serve index.html, got:\n%s", body)
		}
	})

	t.Run("deep link resolves to the app shell", func(t *testing.T) {
		// A client-side route with no matching object falls back to index.html.
		resp := lf.get(t, http.MethodGet, host, "/app/dashboard", nil)
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET deep link = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if !bytes.Contains(body, []byte("<div id=\"root\">")) {
			t.Errorf("deep link did not serve index.html, got:\n%s", body)
		}
	})

	t.Run("HEAD has no body", func(t *testing.T) {
		resp := lf.get(t, http.MethodHead, host, "/", nil)
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD / = %d, want 200", resp.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("HEAD / returned a body of %d bytes", len(body))
		}
	})
}

// TestMediaDelivery covers Range and conditional requests against an S3 origin
// (scenario 5): partial content and 304 Not Modified.
func TestMediaDelivery(t *testing.T) {
	st := startStore(t)
	const payload = "0123456789abcdef" // 16 bytes, deterministic
	st.put(t, "media", "", "", nil)
	st.put(t, "media", "clip.txt", "text/plain", []byte(payload))
	lf := startLocalfront(t, repoFile(t, "e2e/testdata/media.template.yaml"), st)

	const host = "media.example.test"

	t.Run("range returns 206 and exact bytes", func(t *testing.T) {
		resp := lf.get(t, http.MethodGet, host, "/clip.txt", map[string]string{"Range": "bytes=0-4"})
		body := mustReadBody(t, resp)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("ranged GET = %d, want 206\n%s", resp.StatusCode, fmtHeaders(resp.Header))
		}
		if string(body) != "01234" {
			t.Errorf("range body = %q, want %q", body, "01234")
		}
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-4/") {
			t.Errorf("Content-Range = %q, want bytes 0-4/...", cr)
		}
	})

	t.Run("matching ETag returns 304", func(t *testing.T) {
		first := lf.get(t, http.MethodGet, host, "/clip.txt", nil)
		_ = mustReadBody(t, first)
		etag := first.Header.Get("ETag")
		if etag == "" {
			t.Fatal("origin did not return an ETag")
		}
		resp := lf.get(t, http.MethodGet, host, "/clip.txt", map[string]string{"If-None-Match": etag})
		_ = mustReadBody(t, resp)
		if resp.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional GET = %d, want 304", resp.StatusCode)
		}
	})
}
