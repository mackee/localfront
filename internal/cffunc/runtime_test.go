package cffunc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCompile(t *testing.T, opts Options) *Function {
	t.Helper()
	f, err := Compile(opts)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(f.Close)
	return f
}

func requestEvent(method, target string) *Event {
	r := httptest.NewRequest(method, target, nil)
	return NewRequestEvent("viewer-request", Context{EventType: "viewer-request"}, r, nil)
}

// AWS sample: URL rewrite that appends index.html to directory requests.
func TestExecute_URLRewrite(t *testing.T) {
	const code = `
function handler(event) {
	var request = event.request;
	var uri = request.uri;
	if (uri.endsWith('/')) {
		request.uri += 'index.html';
	} else if (!uri.includes('.')) {
		request.uri += '/index.html';
	}
	return request;
}`
	f := mustCompile(t, Options{Name: "rewrite", Code: code})

	tests := map[string]string{
		"/":       "/index.html",
		"/docs":   "/docs/index.html",
		"/app.js": "/app.js",
		"/blog/":  "/blog/index.html",
	}
	for uri, want := range tests {
		res, err := f.Execute(requestEvent(http.MethodGet, "http://example.test"+uri))
		if err != nil {
			t.Fatalf("Execute(%q): %v", uri, err)
		}
		if res.IsResponse() {
			t.Fatalf("Execute(%q) returned a response, want request", uri)
		}
		if res.Request.URI != want {
			t.Errorf("uri %q -> %q, want %q", uri, res.Request.URI, want)
		}
	}
}

// AWS sample: redirect (viewer-request returning a response).
func TestExecute_Redirect(t *testing.T) {
	const code = `
function handler(event) {
	return {
		statusCode: 301,
		statusDescription: 'Moved Permanently',
		headers: { 'location': { value: 'https://www.example.com' + event.request.uri } }
	};
}`
	f := mustCompile(t, Options{Name: "redirect", Code: code})
	res, err := f.Execute(requestEvent(http.MethodGet, "http://example.test/old"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsResponse() {
		t.Fatal("expected a response (redirect)")
	}
	if res.Response.StatusCode != 301 {
		t.Errorf("statusCode = %d, want 301", res.Response.StatusCode)
	}
	if got := res.Response.Headers["location"].Value; got != "https://www.example.com/old" {
		t.Errorf("location = %q", got)
	}
}

// AWS sample: add a response header (viewer-response).
func TestExecute_AddResponseHeader(t *testing.T) {
	const code = `
function handler(event) {
	var response = event.response;
	response.headers['x-custom'] = { value: 'hello' };
	return response;
}`
	f := mustCompile(t, Options{Name: "addheader", Code: code})
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	ev := NewRequestEvent("viewer-response", Context{EventType: "viewer-response"}, r, nil)
	respHeader := http.Header{}
	respHeader.Set("Content-Type", "text/html")
	ev.AttachResponse(200, respHeader)

	res, err := f.Execute(ev)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsResponse() {
		t.Fatal("viewer-response must yield a response")
	}
	if got := res.Response.Headers["x-custom"].Value; got != "hello" {
		t.Errorf("x-custom = %q, want hello", got)
	}
}

// cloudfront-js 2.0 with the cloudfront module import and a KVS read.
func TestExecute_KVS(t *testing.T) {
	const code = `
import cf from 'cloudfront';
const kvs = cf.kvs();
async function handler(event) {
	const request = event.request;
	try {
		const greeting = await kvs.get('greeting');
		request.headers['x-greeting'] = { value: greeting };
	} catch (e) {
		request.headers['x-greeting'] = { value: 'missing' };
	}
	const flag = await kvs.exists('feature');
	request.headers['x-feature'] = { value: String(flag) };
	return request;
}`
	store := NewKVS()
	store.Replace(map[string]string{"greeting": "hello-kvs", "feature": "on"})
	f := mustCompile(t, Options{Name: "kvs", Code: code, KVS: store})

	res, err := f.Execute(requestEvent(http.MethodGet, "http://example.test/"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := res.Request.Headers["x-greeting"].Value; got != "hello-kvs" {
		t.Errorf("x-greeting = %q, want hello-kvs", got)
	}
	if got := res.Request.Headers["x-feature"].Value; got != "true" {
		t.Errorf("x-feature = %q, want true", got)
	}
}

func TestExecute_KVSMissingKeyCaught(t *testing.T) {
	const code = `
import cf from 'cloudfront';
const kvs = cf.kvs();
async function handler(event) {
	const request = event.request;
	let v = 'default';
	try { v = await kvs.get('nope'); } catch (e) { v = 'caught'; }
	request.headers['x-result'] = { value: v };
	return request;
}`
	store := NewKVS()
	f := mustCompile(t, Options{Name: "kvs-missing", Code: code, KVS: store})
	res, err := f.Execute(requestEvent(http.MethodGet, "http://example.test/"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := res.Request.Headers["x-result"].Value; got != "caught" {
		t.Errorf("x-result = %q, want caught", got)
	}
}

// An uncaught JavaScript exception surfaces as an error (mapped to 503).
func TestExecute_RuntimeException(t *testing.T) {
	const code = `
function handler(event) {
	throw new Error('boom');
}`
	f := mustCompile(t, Options{Name: "boom", Code: code})
	_, err := f.Execute(requestEvent(http.MethodGet, "http://example.test/"))
	if err == nil {
		t.Fatal("expected an error from a throwing handler")
	}
}

// A syntax error is caught at compile time.
func TestCompile_SyntaxError(t *testing.T) {
	_, err := Compile(Options{Name: "bad", Code: "function handler(event) { return ; ;; ) }"})
	if err == nil {
		t.Fatal("expected a compile error for invalid JavaScript")
	}
}
