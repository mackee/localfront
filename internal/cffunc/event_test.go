package cffunc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRequestEvent_Structure(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/path?a=1&b=2&a=3", nil)
	r.Header.Set("Host", "example.test")
	r.Header.Add("Accept", "text/html")
	r.Header.Add("X-Multi", "one")
	r.Header.Add("X-Multi", "two")
	r.Header.Set("Cookie", "session=abc; theme=dark")
	r.RemoteAddr = "203.0.113.5:4444"

	viewer := http.Header{"CloudFront-Viewer-Country": {"JP"}}
	ev := NewRequestEvent("viewer-request", Context{EventType: "viewer-request"}, r, viewer)

	if ev.Version != "1.0" {
		t.Errorf("version = %q, want 1.0", ev.Version)
	}
	if ev.Viewer.IP != "203.0.113.5" {
		t.Errorf("viewer.ip = %q, want 203.0.113.5", ev.Viewer.IP)
	}
	if ev.Request.Method != "GET" {
		t.Errorf("method = %q", ev.Request.Method)
	}

	// Header names are lowercased.
	if _, ok := ev.Request.Headers["accept"]; !ok {
		t.Errorf("headers should be lowercased; missing 'accept' in %v", ev.Request.Headers)
	}
	// multiValue captured for repeated headers.
	multi := ev.Request.Headers["x-multi"]
	if multi.Value != "one" || len(multi.MultiValue) != 2 || multi.MultiValue[1].Value != "two" {
		t.Errorf("x-multi = %+v, want value=one multiValue=[one two]", multi)
	}
	// Cookie is not a header; it is parsed into cookies.
	if _, ok := ev.Request.Headers["cookie"]; ok {
		t.Errorf("Cookie must not appear in headers")
	}
	if ev.Request.Cookies["session"].Value != "abc" || ev.Request.Cookies["theme"].Value != "dark" {
		t.Errorf("cookies = %+v", ev.Request.Cookies)
	}
	// Synthesized viewer header merged in (lowercased).
	if got := ev.Request.Headers["cloudfront-viewer-country"].Value; got != "JP" {
		t.Errorf("cloudfront-viewer-country = %q, want JP", got)
	}
	// Query string with repeated key collapses into multiValue.
	a := ev.Request.Querystring["a"]
	if a.Value != "1" || len(a.MultiValue) != 2 || a.MultiValue[1].Value != "3" {
		t.Errorf("querystring a = %+v, want value=1 multiValue=[1 3]", a)
	}
}

func TestNewRequestEvent_HostFromRequestHost(t *testing.T) {
	// A server-received request keeps Host in r.Host, not in r.Header; the event
	// must still expose it as the lowercase "host" header.
	r := httptest.NewRequest(http.MethodGet, "http://viewer.example.test/path", nil)
	if _, ok := r.Header["Host"]; ok {
		t.Fatal("precondition: httptest request should not carry Host in Header")
	}
	r.RemoteAddr = "203.0.113.5:4444"

	ev := NewRequestEvent("viewer-request", Context{EventType: "viewer-request"}, r, nil)

	host, ok := ev.Request.Headers["host"]
	if !ok {
		t.Fatalf("event missing 'host' header; headers = %v", ev.Request.Headers)
	}
	if host.Value != "viewer.example.test" {
		t.Errorf("host = %q, want viewer.example.test", host.Value)
	}
}

func TestResponseHTTPHeaders_Cookies(t *testing.T) {
	resp := &Response{
		Headers: map[string]Field{
			"content-type": {Value: "text/html"},
		},
		Cookies: map[string]ResponseCookie{
			"session": {Value: "abc", Attributes: "Path=/; Secure; HttpOnly"},
			"theme":   {Value: "dark"},
		},
	}
	h := resp.HTTPHeaders()

	if got := h.Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	cookies := h.Values("Set-Cookie")
	// Sorted by cookie name: session, theme.
	want := []string{"session=abc; Path=/; Secure; HttpOnly", "theme=dark"}
	if len(cookies) != len(want) {
		t.Fatalf("Set-Cookie = %v, want %v", cookies, want)
	}
	for i := range want {
		if cookies[i] != want[i] {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, cookies[i], want[i])
		}
	}
}

func TestAttachResponse_SetCookie(t *testing.T) {
	header := http.Header{}
	header.Set("Content-Type", "text/html")
	header.Add("Set-Cookie", "session=abc; Path=/; Secure")
	header.Add("Set-Cookie", "theme=dark")

	ev := &Event{}
	ev.AttachResponse(http.StatusOK, header)

	// Set-Cookie is modeled as response.cookies, not response.headers, so a
	// function can read and modify existing cookies instead of duplicating them.
	if _, ok := ev.Response.Headers["set-cookie"]; ok {
		t.Error("set-cookie must not appear in response.headers")
	}
	if got := ev.Response.Headers["content-type"].Value; got != "text/html" {
		t.Errorf("content-type = %q, want text/html", got)
	}
	if c := ev.Response.Cookies["session"]; c.Value != "abc" || c.Attributes != "Path=/; Secure" {
		t.Errorf("cookies[session] = %+v, want value=abc attributes=Path=/; Secure", c)
	}
	if c := ev.Response.Cookies["theme"]; c.Value != "dark" || c.Attributes != "" {
		t.Errorf("cookies[theme] = %+v, want value=dark attributes=(empty)", c)
	}

	// Round-trip: HTTPHeaders reproduces the original Set-Cookie set (sorted).
	got := ev.Response.HTTPHeaders().Values("Set-Cookie")
	want := []string{"session=abc; Path=/; Secure", "theme=dark"}
	if len(got) != len(want) {
		t.Fatalf("Set-Cookie round-trip = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Set-Cookie[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResponseCookies_Unmarshal(t *testing.T) {
	const raw = `{"statusCode":200,"cookies":{"id":{"value":"v1","attributes":"Path=/"}}}`
	var resp Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c, ok := resp.Cookies["id"]
	if !ok {
		t.Fatalf("cookie 'id' missing; cookies = %v", resp.Cookies)
	}
	if c.Value != "v1" || c.Attributes != "Path=/" {
		t.Errorf("cookie = %+v, want value=v1 attributes=Path=/", c)
	}
}

func TestApplyToRequest_RoundTrip(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/old", nil)
	req := &Request{
		Method: "GET",
		URI:    "/new/path",
		Querystring: map[string]Field{
			"q": {Value: "search term"},
		},
		Headers: map[string]Field{
			"x-added": {Value: "yes"},
		},
		Cookies: map[string]Cookie{
			"sid": {Value: "xyz"},
		},
	}
	req.ApplyToRequest(r)

	if r.URL.Path != "/new/path" {
		t.Errorf("path = %q, want /new/path", r.URL.Path)
	}
	if got := r.URL.Query().Get("q"); got != "search term" {
		t.Errorf("query q = %q, want 'search term'", got)
	}
	if got := r.Header.Get("X-Added"); got != "yes" {
		t.Errorf("X-Added = %q, want yes", got)
	}
	if got := r.Header.Get("Cookie"); got != "sid=xyz" {
		t.Errorf("Cookie = %q, want sid=xyz", got)
	}
}

func TestBody_Unmarshal(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"plain string", `"hello"`, "hello"},
		{"text object", `{"encoding":"text","data":"world"}`, "world"},
		{"base64 object", `{"encoding":"base64","data":"aGVsbG8="}`, "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b Body
			if err := b.UnmarshalJSON([]byte(tc.json)); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := string(b.Bytes()); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseSeed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"bulk format", `{"data":[{"key":"a","value":"1"},{"key":"b","value":"2"}]}`, map[string]string{"a": "1", "b": "2"}},
		{"flat format", `{"a":"1","b":"2"}`, map[string]string{"a": "1", "b": "2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSeed([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseSeed: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
