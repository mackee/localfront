// Package sign verifies CloudFront signed URLs and signed cookies against the
// trusted key groups of a cache behavior. It validates the RSA-SHA1 signature
// produced for canned and custom policies (the same scheme the aws-sdk-go-v2
// CloudFront URL signer emits) and enforces the policy's expiry, not-before,
// and source-IP conditions.
package sign

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Key is a trusted CloudFront public key: the RSA key plus its key-pair ID
// (the ID a signer puts in the Key-Pair-Id parameter).
type Key struct {
	ID  string
	RSA *rsa.PublicKey
}

// ParsePublicKey parses a PEM-encoded RSA public key (PKIX/SPKI or PKCS#1).
func ParsePublicKey(encoded string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("public key is not RSA")
		}
		return rsaKey, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported public key format (expected PKIX or PKCS#1 RSA)")
}

// DenyError reports why access was denied. The data plane maps it to a 403.
type DenyError struct{ Message string }

func (e *DenyError) Error() string { return e.Message }

func deny(format string, args ...any) error {
	return &DenyError{Message: fmt.Sprintf(format, args...)}
}

// Verify checks the signed-URL or signed-cookie credentials on r against the
// trusted keys. It returns nil when access is granted, or a *DenyError.
// defaultRootObject is the distribution's root object: a request for the root is
// served (and was signed) as that object, so it is resolved before matching.
//
// publicHost is the host (optionally host:port) used as the resource authority
// when matching the signed resource: a canned policy's resource is
// reconstructed against it (verbatim, including any port, since the canned
// signature covers the exact resource URL the signer produced), and a custom
// policy's Resource host is matched against it. When empty it falls back to the
// request's Host header — the wildcard-subdomain case, where each tenant
// arrives on its own host.
func Verify(r *http.Request, trusted []Key, now time.Time, defaultRootObject, publicHost string) error {
	c, err := extractCredentials(r)
	if err != nil {
		return err
	}
	key := findKey(trusted, c.keyPairID)
	if key == nil {
		return deny("Key-Pair-Id %q is not in a trusted key group for this behavior", c.keyPairID)
	}
	sig, err := awsBase64Decode(c.signature)
	if err != nil {
		return deny("malformed Signature")
	}

	if c.policy != "" {
		return verifyCustom(r, key, c.policy, sig, now, defaultRootObject, publicHost)
	}
	return verifyCanned(r, key, c.expires, sig, now, defaultRootObject, resourceHost(r, publicHost))
}

// resourceHost returns the authority used when matching the signed resource:
// reconstructing a canned-policy resource, and matching a custom-policy
// Resource's host. An explicit publicHost (from --public-host) is used
// verbatim; otherwise the request's Host header is used as received. Either way
// the port, if present, is part of the resource.
func resourceHost(r *http.Request, publicHost string) string {
	if publicHost != "" {
		return publicHost
	}
	return r.Host
}

// applyDefaultRootObject mirrors the data plane: a request for the distribution
// root ("" or "/") is served as the default root object, which is the URL the
// signer signed, so the resource is reconstructed/matched against that object.
func applyDefaultRootObject(path, defaultRootObject string) string {
	if (path == "" || path == "/") && defaultRootObject != "" {
		return "/" + defaultRootObject
	}
	return path
}

type credentials struct {
	keyPairID string
	signature string
	policy    string // custom policy (base64, AWS-escaped); empty for canned
	expires   string // canned expiry epoch seconds; empty for custom
}

// extractCredentials reads the signing parameters from the query string, then
// falls back to the CloudFront-* cookies.
func extractCredentials(r *http.Request) (credentials, error) {
	q := r.URL.Query()
	c := credentials{
		keyPairID: q.Get("Key-Pair-Id"),
		signature: q.Get("Signature"),
		policy:    q.Get("Policy"),
		expires:   q.Get("Expires"),
	}
	if c.keyPairID == "" && c.signature == "" {
		c = credentials{
			keyPairID: cookie(r, "CloudFront-Key-Pair-Id"),
			signature: cookie(r, "CloudFront-Signature"),
			policy:    cookie(r, "CloudFront-Policy"),
			expires:   cookie(r, "CloudFront-Expires"),
		}
	}
	switch {
	case c.keyPairID == "":
		return c, deny("Missing Key-Pair-Id")
	case c.signature == "":
		return c, deny("Missing Signature")
	case c.policy == "" && c.expires == "":
		return c, deny("Missing Policy or Expires")
	}
	return c, nil
}

func cookie(r *http.Request, name string) string {
	if ck, err := r.Cookie(name); err == nil {
		return ck.Value
	}
	return ""
}

func findKey(trusted []Key, id string) *rsa.PublicKey {
	for _, k := range trusted {
		if k.ID == id {
			return k.RSA
		}
	}
	return nil
}

// verifyCanned validates a canned policy. The signed policy embeds the resource
// URL (the base URL with its application query string, but without CloudFront's
// signing parameters), which localfront reconstructs from the request; the
// viewer side is plain HTTP locally, so both https and http resources are tried.
func verifyCanned(r *http.Request, key *rsa.PublicKey, expires string, sig []byte, now time.Time, defaultRootObject, host string) error {
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return deny("malformed Expires")
	}
	query := resourceQuery(r.URL.RawQuery)
	// EscapedPath preserves the exact percent-encoding the signer signed;
	// net/http has already decoded r.URL.Path, which would break verification
	// for resources whose path contains escaped bytes.
	escapedPath := applyDefaultRootObject(r.URL.EscapedPath(), defaultRootObject)
	matched := false
	for _, scheme := range []string{"https", "http"} {
		resource := scheme + "://" + host + escapedPath
		if query != "" {
			resource += "?" + query
		}
		if verifyRSA(key, cannedPolicyJSON(resource, exp), sig) {
			matched = true
			break
		}
	}
	if !matched {
		return deny("signature does not match")
	}
	if !now.Before(time.Unix(exp, 0)) {
		return deny("the signed URL has expired")
	}
	return nil
}

// verifyCustom validates a custom policy: the signature covers the decoded
// policy document verbatim, and its conditions are then enforced.
func verifyCustom(r *http.Request, key *rsa.PublicKey, policyParam string, sig []byte, now time.Time, defaultRootObject, publicHost string) error {
	jsonPolicy, err := awsBase64Decode(policyParam)
	if err != nil {
		return deny("malformed Policy")
	}
	if !verifyRSA(key, jsonPolicy, sig) {
		return deny("signature does not match")
	}
	stmt, err := parsePolicyStatement(jsonPolicy)
	if err != nil {
		return deny("malformed Policy: %v", err)
	}
	if stmt.dateLessThan > 0 && !now.Before(time.Unix(stmt.dateLessThan, 0)) {
		return deny("the signed URL has expired")
	}
	if stmt.dateGreaterThan > 0 && !now.After(time.Unix(stmt.dateGreaterThan, 0)) {
		return deny("the signed URL is not valid yet")
	}
	if stmt.ipAddress != "" {
		if err := checkIP(stmt.ipAddress, r.RemoteAddr); err != nil {
			return err
		}
	}
	// EscapedPath preserves the percent-encoding the signer signed into the
	// policy Resource; net/http has already decoded r.URL.Path, which would
	// reject a valid request for a resource whose path contains escaped bytes
	// (the canned path uses EscapedPath for the same reason).
	if !resourceMatches(stmt.resource, resourceHost(r, publicHost), applyDefaultRootObject(r.URL.EscapedPath(), defaultRootObject)) {
		return deny("the request does not match the signed resource")
	}
	return nil
}

func verifyRSA(key *rsa.PublicKey, message, sig []byte) bool {
	sum := sha1.Sum(message)
	return rsa.VerifyPKCS1v15(key, crypto.SHA1, sum[:], sig) == nil
}

// cloudFrontSigningParams are the query parameters CloudFront appends to a
// signed URL. They are not part of the resource the canned policy signs, so the
// verifier strips them before reconstructing the resource.
var cloudFrontSigningParams = map[string]bool{
	"Expires":        true,
	"Signature":      true,
	"Key-Pair-Id":    true,
	"Policy":         true,
	"Hash-Algorithm": true,
}

// resourceQuery returns the application query string of a signed URL: the raw
// query with CloudFront's signing parameters removed, preserving the order and
// raw encoding of the remaining parameters (the signer signs the base URL, with
// its query string, verbatim).
func resourceQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var kept []string
	for _, param := range strings.Split(rawQuery, "&") {
		if param == "" {
			continue
		}
		key := param
		if eq := strings.IndexByte(param, '='); eq >= 0 {
			key = param[:eq]
		}
		if cloudFrontSigningParams[key] {
			continue
		}
		kept = append(kept, param)
	}
	return strings.Join(kept, "&")
}

// cannedPolicyJSON reproduces, byte for byte, the canned policy the
// aws-sdk-go-v2 signer encodes (compact JSON, fixed field order).
func cannedPolicyJSON(resource string, expires int64) []byte {
	var b bytes.Buffer
	b.WriteString(`{"Statement":[{"Resource":`)
	b.Write(jsonString(resource))
	b.WriteString(`,"Condition":{"DateLessThan":{"AWS:EpochTime":`)
	b.WriteString(strconv.FormatInt(expires, 10))
	b.WriteString(`}}}]}`)
	return b.Bytes()
}

// jsonString encodes s as a JSON string the way the aws-sdk-go-v2 CloudFront
// signer does: with HTML escaping disabled, so characters like '&' in a
// resource query string are emitted literally. json.Marshal would escape '&',
// '<' and '>' to their \uXXXX form and break signature verification.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

type statement struct {
	resource        string
	dateLessThan    int64
	dateGreaterThan int64
	ipAddress       string
}

func parsePolicyStatement(jsonPolicy []byte) (statement, error) {
	var p struct {
		Statement []struct {
			Resource  string `json:"Resource"`
			Condition struct {
				DateLessThan *struct {
					Epoch int64 `json:"AWS:EpochTime"`
				} `json:"DateLessThan"`
				DateGreaterThan *struct {
					Epoch int64 `json:"AWS:EpochTime"`
				} `json:"DateGreaterThan"`
				IPAddress *struct {
					SourceIP string `json:"AWS:SourceIp"`
				} `json:"IpAddress"`
			} `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(jsonPolicy, &p); err != nil {
		return statement{}, err
	}
	if len(p.Statement) == 0 {
		return statement{}, fmt.Errorf("no statement")
	}
	s := p.Statement[0]
	out := statement{resource: s.Resource}
	if s.Condition.DateLessThan != nil {
		out.dateLessThan = s.Condition.DateLessThan.Epoch
	}
	if s.Condition.DateGreaterThan != nil {
		out.dateGreaterThan = s.Condition.DateGreaterThan.Epoch
	}
	if s.Condition.IPAddress != nil {
		out.ipAddress = s.Condition.IPAddress.SourceIP
	}
	return out, nil
}

func checkIP(cidr, remoteAddr string) error {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return deny("cannot determine the client IP")
	}
	if !strings.Contains(cidr, "/") {
		cidr += "/32"
	}
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return deny("malformed IpAddress condition")
	}
	if !network.Contains(ip) {
		return deny("the client IP is not allowed by the signed policy")
	}
	return nil
}

// resourceMatches checks the request against the policy resource pattern,
// honoring CloudFront's '*' and '?' wildcards. When the Resource carries a
// scheme://host, the host is enforced as well: a Resource like
// "https://*.tenants.example.com/*" only covers requests whose host matches
// (host is resourceHost(r, publicHost) — the viewer host, or --public-host when
// set). The viewer's scheme is not observable behind TLS termination, so both
// https and http are tried, the same relaxation the canned path makes. A
// path-only Resource (no scheme/host) constrains the path alone.
func resourceMatches(resource, host, path string) bool {
	if resource == "" || resource == "*" {
		return true
	}
	// Drop the query string: only the path part is matched (fidelity note 15).
	// An SDK-signed Resource such as ".../video.mp4?foo=bar" must still match the
	// request, which net/http exposes without the query.
	pattern := resource
	if q := strings.IndexByte(pattern, '?'); q >= 0 {
		pattern = pattern[:q]
	}
	if !strings.Contains(pattern, "://") {
		return matchGlob(pattern, path)
	}
	for _, scheme := range []string{"https", "http"} {
		if matchGlob(pattern, scheme+"://"+host+path) {
			return true
		}
	}
	return false
}

// awsBase64Decode reverses CloudFront's URL-safe base64 substitution and
// decodes the value.
func awsBase64Decode(s string) ([]byte, error) {
	replacer := strings.NewReplacer("-", "+", "_", "=", "~", "/")
	return base64.StdEncoding.DecodeString(replacer.Replace(s))
}

// matchGlob matches CloudFront resource wildcards: '*' (zero or more) and '?'
// (exactly one), both spanning '/'.
func matchGlob(pattern, s string) bool {
	var p, str int
	star := -1
	starMatchEnd := 0
	for str < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[str]):
			p++
			str++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			starMatchEnd = str
			p++
		case star != -1:
			p = star + 1
			starMatchEnd++
			str = starMatchEnd
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
