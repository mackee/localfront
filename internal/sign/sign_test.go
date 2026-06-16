package sign_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	awssign "github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"
	"github.com/mackee/localfront/internal/sign"
)

const keyPairID = "K2EXAMPLEKEYID"

// testPriv and testTrusted are populated once by TestMain. Tests read them
// without further synchronization: TestMain establishes a happens-before
// relationship before any TestX runs, so concurrent t.Parallel reads are
// safe even though the values themselves are package-level.
var (
	testPriv    *rsa.PrivateKey
	testTrusted []sign.Key
)

func TestMain(m *testing.M) {
	priv, trusted, err := generateTestKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign_test: generating RSA test key: %v\n", err)
		os.Exit(1)
	}
	testPriv = priv
	testTrusted = trusted
	os.Exit(m.Run())
}

func generateTestKey() (*rsa.PrivateKey, []sign.Key, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal public key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	pub, err := sign.ParsePublicKey(string(pemBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}
	return priv, []sign.Key{{ID: keyPairID, RSA: pub}}, nil
}

// testKey returns the package-wide test key pair and trusted-key list.
func testKey(t *testing.T) (*rsa.PrivateKey, []sign.Key) {
	t.Helper()
	return testPriv, testTrusted
}

// requestFromSignedURL turns an SDK-signed URL into an *http.Request as the
// data plane would see it.
func requestFromSignedURL(signedURL string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, signedURL, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	return r
}

func TestVerify_CannedURL_Accept(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	expires := time.Now().Add(time.Hour)
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4", expires)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("canned signed URL should verify, got: %v", err)
	}
}

func TestVerify_CannedURL_PortIsPartOfResource(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	// localfront is reached at media.example.test:8080 and the URL is signed for
	// that exact authority; the port is part of the signed resource.
	signed, err := signer.Sign("https://media.example.test:8080/premium/video.mp4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := requestFromSignedURL(signed)
	req.Host = "media.example.test:8080"
	if err := sign.Verify(req, trusted, time.Now(), "", "media.example.test:8080"); err != nil {
		t.Errorf("canned signed URL should verify against the host:port it was signed for, got: %v", err)
	}
	// The same request must NOT verify against the port-less authority: the port
	// is part of the signed resource, not stripped.
	if err := sign.Verify(req, trusted, time.Now(), "", "media.example.test"); err == nil {
		t.Error("verification should fail when the public host omits the signed port")
	}
}

func TestVerify_CannedURL_PublicHostOverride(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// The viewer reaches localfront via a different host than the one the URL was
	// signed for; --public-host pins verification to the signing host.
	req := requestFromSignedURL(signed)
	req.Host = "localhost:8080"
	if err := sign.Verify(req, trusted, time.Now(), "", "media.example.test"); err != nil {
		t.Errorf("canned signed URL should verify against the configured public host, got: %v", err)
	}
	// Without the override, the resource is reconstructed from the request Host
	// (localhost:8080) and fails.
	if err := sign.Verify(req, trusted, time.Now(), "", ""); err == nil {
		t.Error("verification should fail when the request host differs and no public host is set")
	}
}

func TestVerify_CannedURL_WithQueryString(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	expires := time.Now().Add(time.Hour)
	// The base URL carries application query parameters; the canned resource the
	// signer signs includes them (and the '&' between them), so the verifier must
	// reconstruct the query and encode '&' literally (not as the U+0026 escape).
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4?foo=bar&baz=qux", expires)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("canned signed URL with query string should verify, got: %v", err)
	}
}

func TestVerify_CannedURL_EscapedPath(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	expires := time.Now().Add(time.Hour)
	// The signer signs the URL with its percent-encoded bytes; net/http decodes
	// r.URL.Path, so verification must reconstruct the resource from the escaped
	// path or a valid signed URL with an escaped path would be rejected.
	signed, err := signer.Sign("https://media.example.test/premium/my%20video.mp4", expires)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("canned signed URL with an escaped path should verify, got: %v", err)
	}
}

func TestVerify_CannedURL_Expired(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	expires := time.Now().Add(-time.Minute) // already expired
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4", expires)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	err = sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", "")
	if err == nil {
		t.Fatal("expired signed URL should be rejected")
	}
}

func TestVerify_CannedURL_Tampered(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := requestFromSignedURL(signed)
	// Tamper: change the requested path so the signature no longer matches.
	req.URL.Path = "/premium/other.mp4"
	if err := sign.Verify(req, trusted, time.Now(), "", ""); err == nil {
		t.Fatal("tampered request should be rejected")
	}
}

func TestVerify_UnknownKeyPairID(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner("K-DIFFERENT-ID", priv) // not in trusted set
	signed, err := signer.Sign("https://media.example.test/premium/video.mp4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err == nil {
		t.Fatal("unknown Key-Pair-Id should be rejected")
	}
}

func TestVerify_NoCredentials(t *testing.T) {
	_, trusted := testKey(t)
	r := httptest.NewRequest(http.MethodGet, "https://media.example.test/premium/video.mp4", nil)
	if err := sign.Verify(r, trusted, time.Now(), "", ""); err == nil {
		t.Fatal("request without credentials should be rejected")
	}
}

func TestVerify_CustomURL_Accept(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/*",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/video.mp4", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("custom signed URL should verify, got: %v", err)
	}
}

// A custom-policy Resource may carry an application query string; only the path
// part is matched, so the request (which has no such query) must still verify.
func TestVerify_CustomURL_ResourceWithQueryString(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/video.mp4?foo=bar",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/video.mp4?foo=bar", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("custom signed URL whose resource has a query string should verify, got: %v", err)
	}
}

// A custom-policy Resource with an exact, percent-encoded path must match the
// request's escaped path, not net/http's decoded r.URL.Path (the canned path is
// already handled by TestVerify_CannedURL_EscapedPath).
func TestVerify_CustomURL_EscapedPath(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/my%20video.mp4",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/my%20video.mp4", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err != nil {
		t.Errorf("custom signed URL with an escaped resource path should verify, got: %v", err)
	}
}

// A request for the distribution root is served (and was signed) as the default
// root object, so verification must resolve it before matching.
func TestVerify_CannedURL_DefaultRootObject(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	expires := time.Now().Add(time.Hour)
	signed, err := signer.Sign("https://media.example.test/index.html", expires)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	r := requestFromSignedURL(signed)
	r.URL.Path = "/" // the viewer requests the root
	r.URL.RawPath = ""
	if err := sign.Verify(r, trusted, time.Now(), "index.html", ""); err != nil {
		t.Errorf("root request should verify against the default root object, got: %v", err)
	}
	// Without the default root object the root request resolves to "/", which the
	// signer never signed, so it must be rejected.
	if err := sign.Verify(r, trusted, time.Now(), "", ""); err == nil {
		t.Error("root request should not verify when no default root object resolves it")
	}
}

func TestVerify_CustomURL_ResourceMismatch(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/*",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/video.mp4", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := requestFromSignedURL(signed)
	req.URL.Path = "/secret/other.mp4" // outside the signed resource pattern
	if err := sign.Verify(req, trusted, time.Now(), "", ""); err == nil {
		t.Fatal("request outside the signed resource should be rejected")
	}
}

func TestVerify_CustomURL_IPAddress(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/*",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
			IPAddress:    &awssign.IPAddress{SourceIP: "192.0.2.0/24"},
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/video.mp4", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	allowed := requestFromSignedURL(signed)
	allowed.RemoteAddr = "192.0.2.50:1234"
	if err := sign.Verify(allowed, trusted, time.Now(), "", ""); err != nil {
		t.Errorf("in-range IP should verify, got: %v", err)
	}

	denied := requestFromSignedURL(signed)
	denied.RemoteAddr = "198.51.100.7:1234"
	if err := sign.Verify(denied, trusted, time.Now(), "", ""); err == nil {
		t.Error("out-of-range IP should be rejected")
	}
}

func TestVerify_CustomURL_DateGreaterThan(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewURLSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/*",
		Condition: awssign.Condition{
			DateLessThan:    awssign.NewAWSEpochTime(time.Now().Add(2 * time.Hour)),
			DateGreaterThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)), // not valid yet
		},
	}}}
	signed, err := signer.SignWithPolicy("https://media.example.test/premium/video.mp4", policy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "", ""); err == nil {
		t.Fatal("URL before its DateGreaterThan should be rejected")
	}
}

func TestVerify_SignedCookies_Accept(t *testing.T) {
	priv, trusted := testKey(t)
	signer := awssign.NewCookieSigner(keyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/videos/*",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	cookies, err := signer.SignWithPolicy(policy)
	if err != nil {
		t.Fatalf("sign cookies: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "https://media.example.test/videos/ep1/seg1.ts", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if err := sign.Verify(r, trusted, time.Now(), "", ""); err != nil {
		t.Errorf("signed cookies should verify, got: %v", err)
	}
}
