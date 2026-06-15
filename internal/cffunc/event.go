// Package cffunc runs CloudFront Functions (cloudfront-js 1.0 / 2.0) on a
// sandboxed QuickJS-NG engine compiled to WebAssembly and executed on wazero,
// with an in-memory KeyValueStore. It builds the version 1.0 event object the
// functions expect, invokes the viewer-request / viewer-response handler, and
// returns the (possibly modified) request or the response it produced.
package cffunc

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Event is a CloudFront Functions event (event structure version 1.0).
type Event struct {
	Version  string    `json:"version"`
	Context  Context   `json:"context"`
	Viewer   Viewer    `json:"viewer"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
}

// Context carries distribution metadata.
type Context struct {
	DistributionDomainName string `json:"distributionDomainName"`
	DistributionID         string `json:"distributionId"`
	EventType              string `json:"eventType"`
	RequestID              string `json:"requestId"`
}

// Viewer carries viewer metadata.
type Viewer struct {
	IP string `json:"ip"`
}

// ValueEntry is one entry of a multiValue list.
type ValueEntry struct {
	Value string `json:"value"`
}

// Field is the CloudFront representation of a header / query-string parameter:
// a primary value plus an optional list of all values.
type Field struct {
	Value      string       `json:"value"`
	MultiValue []ValueEntry `json:"multiValue,omitempty"`
}

// Cookie is the CloudFront representation of a request cookie.
type Cookie struct {
	Value      string       `json:"value"`
	MultiValue []ValueEntry `json:"multiValue,omitempty"`
}

// Request is the request half of the event.
type Request struct {
	Method      string            `json:"method"`
	URI         string            `json:"uri"`
	Querystring map[string]Field  `json:"querystring"`
	Headers     map[string]Field  `json:"headers"`
	Cookies     map[string]Cookie `json:"cookies"`
}

// Response is the response half of the event (and the short-circuit response a
// viewer-request function may return).
type Response struct {
	StatusCode        int                       `json:"statusCode"`
	StatusDescription string                    `json:"statusDescription,omitempty"`
	Headers           map[string]Field          `json:"headers"`
	Cookies           map[string]ResponseCookie `json:"cookies,omitempty"`
	Body              *Body                     `json:"body,omitempty"`
}

// ResponseCookie is a cookie a function sets on the response: a value plus the
// optional attributes string (e.g. "Path=/; Secure; Expires=...") as CloudFront
// Functions model it.
type ResponseCookie struct {
	Value      string `json:"value"`
	Attributes string `json:"attributes,omitempty"`
}

// values returns all values of a Field, preferring multiValue when present.
func (f Field) values() []string {
	if len(f.MultiValue) == 0 {
		return []string{f.Value}
	}
	out := make([]string, len(f.MultiValue))
	for i, e := range f.MultiValue {
		out[i] = e.Value
	}
	return out
}

func fieldFromValues(values []string) Field {
	f := Field{Value: values[0]}
	if len(values) > 1 {
		f.MultiValue = make([]ValueEntry, len(values))
		for i, v := range values {
			f.MultiValue[i] = ValueEntry{Value: v}
		}
	}
	return f
}

// NewRequestEvent builds a viewer-request (or viewer-response) request event
// from an HTTP request. viewerHeaders are the synthesized CloudFront-* headers,
// merged in so functions observe them (e.g. CloudFront-Viewer-Country).
func NewRequestEvent(eventType string, ctx Context, r *http.Request, viewerHeaders http.Header) *Event {
	headers := headersToFields(r.Header, viewerHeaders)
	// Go stores the viewer Host in r.Host (stripped from r.Header); CloudFront
	// exposes it as the lowercase "host" header in the function event.
	if r.Host != "" {
		headers["host"] = fieldFromValues([]string{r.Host})
	}
	req := &Request{
		Method:      r.Method,
		URI:         r.URL.Path,
		Querystring: queryToFields(r.URL.RawQuery),
		Headers:     headers,
		Cookies:     cookiesToMap(r.Header.Get("Cookie")),
	}
	return &Event{Version: "1.0", Context: ctx, Viewer: Viewer{IP: clientIP(r.RemoteAddr)}, Request: req}
}

// AttachResponse adds the response half to an event, for viewer-response.
func (e *Event) AttachResponse(statusCode int, header http.Header) {
	e.Response = &Response{
		StatusCode: statusCode,
		Headers:    headersToFields(header, nil),
	}
}

// ApplyToRequest writes the function's modified request back onto r: URI, query
// string, headers, and the Cookie header. The HTTP method is not modifiable by
// CloudFront Functions and is left unchanged.
func (req *Request) ApplyToRequest(r *http.Request) {
	r.URL.Path = req.URI
	r.URL.RawPath = ""
	r.URL.RawQuery = fieldsToQuery(req.Querystring)

	newHeaders := make(http.Header, len(req.Headers))
	for name, f := range req.Headers {
		canonical := http.CanonicalHeaderKey(name)
		newHeaders[canonical] = f.values()
	}
	if len(req.Cookies) > 0 {
		newHeaders.Set("Cookie", cookiesToHeader(req.Cookies))
	}
	r.Header = newHeaders
}

// queryToFields parses a raw query string into the event's querystring map,
// collapsing repeated keys into multiValue.
func queryToFields(rawQuery string) map[string]Field {
	out := map[string]Field{}
	if rawQuery == "" {
		return out
	}
	order := []string{}
	grouped := map[string][]string{}
	for _, param := range strings.Split(rawQuery, "&") {
		if param == "" {
			continue
		}
		key, value, _ := strings.Cut(param, "=")
		dk, _ := url.QueryUnescape(key)
		dv, _ := url.QueryUnescape(value)
		if _, seen := grouped[dk]; !seen {
			order = append(order, dk)
		}
		grouped[dk] = append(grouped[dk], dv)
	}
	for _, k := range order {
		out[k] = fieldFromValues(grouped[k])
	}
	return out
}

// fieldsToQuery rebuilds a query string from the event's querystring map. Keys
// are sorted for deterministic output.
func fieldsToQuery(fields map[string]Field) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range fields[k].values() {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// headersToFields lowercases header names (CloudFront event convention) and
// merges synthesized CloudFront-* headers on top. Cookie is omitted (it is
// represented separately as cookies).
func headersToFields(header, viewerHeaders http.Header) map[string]Field {
	out := map[string]Field{}
	for name, values := range header {
		lower := strings.ToLower(name)
		if lower == "cookie" {
			continue
		}
		out[lower] = fieldFromValues(values)
	}
	for name, values := range viewerHeaders {
		out[strings.ToLower(name)] = fieldFromValues(values)
	}
	return out
}

// HTTPHeaders converts the response's header fields back to an http.Header,
// including a Set-Cookie header for each cookie the function set on the
// response. Cookie names are sorted for deterministic output.
func (r *Response) HTTPHeaders() http.Header {
	out := make(http.Header, len(r.Headers))
	for name, f := range r.Headers {
		out[http.CanonicalHeaderKey(name)] = f.values()
	}
	if len(r.Cookies) > 0 {
		names := make([]string, 0, len(r.Cookies))
		for name := range r.Cookies {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			c := r.Cookies[name]
			v := name + "=" + c.Value
			if c.Attributes != "" {
				v += "; " + c.Attributes
			}
			out.Add("Set-Cookie", v)
		}
	}
	return out
}

// BodyBytes returns the decoded response body, or nil when there is none.
func (r *Response) BodyBytes() []byte {
	return r.Body.Bytes()
}

func cookiesToMap(cookieHeader string) map[string]Cookie {
	out := map[string]Cookie{}
	if cookieHeader == "" {
		return out
	}
	for _, pair := range strings.Split(cookieHeader, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		out[strings.TrimSpace(name)] = Cookie{Value: value}
	}
	return out
}

func cookiesToHeader(cookies map[string]Cookie) string {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(cookies))
	for _, name := range names {
		parts = append(parts, name+"="+cookies[name].Value)
	}
	return strings.Join(parts, "; ")
}

func clientIP(remoteAddr string) string {
	if i := strings.LastIndexByte(remoteAddr, ':'); i >= 0 {
		// Handle IPv6 in brackets and plain host:port.
		host := remoteAddr[:i]
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
		if !strings.Contains(host, ":") || strings.Count(remoteAddr, ":") > 1 {
			return host
		}
	}
	return remoteAddr
}

// Body is a response body returned by a function, accepting either a plain JSON
// string or {"encoding": "text"|"base64", "data": "..."}.
type Body struct {
	Encoding string
	Data     string
}

func (b *Body) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		b.Encoding = "text"
		b.Data = s
		return nil
	}
	var obj struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	b.Encoding = obj.Encoding
	b.Data = obj.Data
	return nil
}

// Bytes decodes the body to its raw bytes.
func (b *Body) Bytes() []byte {
	if b == nil {
		return nil
	}
	if strings.EqualFold(b.Encoding, "base64") {
		if decoded, err := base64.StdEncoding.DecodeString(b.Data); err == nil {
			return decoded
		}
	}
	return []byte(b.Data)
}
