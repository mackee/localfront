package sign_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	awssign "github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"
	"github.com/mackee/localfront/internal/sign"
)

const keyPairID = "K2EXAMPLEKEYID"

var (
	testKeyOnce sync.Once
	testPriv    *rsa.PrivateKey
	testTrusted []sign.Key
)

// testKey lazily generates an RSA key pair and the matching trusted Key list.
func testKey(t *testing.T) (*rsa.PrivateKey, []sign.Key) {
	t.Helper()
	testKeyOnce.Do(func() {
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		pub, err := sign.ParsePublicKey(string(pemBytes))
		if err != nil {
			t.Fatalf("parse public key: %v", err)
		}
		testPriv = priv
		testTrusted = []sign.Key{{ID: keyPairID, RSA: pub}}
	})
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err != nil {
		t.Errorf("canned signed URL should verify, got: %v", err)
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err != nil {
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err != nil {
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
	err = sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), "")
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
	if err := sign.Verify(req, trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err == nil {
		t.Fatal("unknown Key-Pair-Id should be rejected")
	}
}

func TestVerify_NoCredentials(t *testing.T) {
	_, trusted := testKey(t)
	r := httptest.NewRequest(http.MethodGet, "https://media.example.test/premium/video.mp4", nil)
	if err := sign.Verify(r, trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err != nil {
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err != nil {
		t.Errorf("custom signed URL whose resource has a query string should verify, got: %v", err)
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
	if err := sign.Verify(r, trusted, time.Now(), "index.html"); err != nil {
		t.Errorf("root request should verify against the default root object, got: %v", err)
	}
	// Without the default root object the root request resolves to "/", which the
	// signer never signed, so it must be rejected.
	if err := sign.Verify(r, trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(req, trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(allowed, trusted, time.Now(), ""); err != nil {
		t.Errorf("in-range IP should verify, got: %v", err)
	}

	denied := requestFromSignedURL(signed)
	denied.RemoteAddr = "198.51.100.7:1234"
	if err := sign.Verify(denied, trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(requestFromSignedURL(signed), trusted, time.Now(), ""); err == nil {
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
	if err := sign.Verify(r, trusted, time.Now(), ""); err != nil {
		t.Errorf("signed cookies should verify, got: %v", err)
	}
}
