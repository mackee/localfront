package dataplane_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	awssign "github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/dataplane"
)

const m6KeyPairID = "KSIGNEXAMPLE01"

func m6KeyPair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// signedURLDistribution builds an S3-less custom-origin distribution whose
// /premium/* behavior is protected by a trusted key group.
func signedURLDistribution(t *testing.T, encodedKey string, origin *config.Origin) (*config.Config, *config.Distribution) {
	pubKey := &config.PublicKey{LogicalID: "PK", ID: m6KeyPairID, Name: "pk", EncodedKey: encodedKey}
	keyGroup := &config.KeyGroup{LogicalID: "KG", ID: "kg-1", Name: "kg", Keys: []*config.PublicKey{pubKey}}

	openBeh := getHeadBehavior(origin, "")
	premiumBeh := getHeadBehavior(origin, "/premium/*")
	premiumBeh.TrustedKeyGroups = []*config.KeyGroup{keyGroup}

	dist := &config.Distribution{
		LogicalID: "D1", ID: "D1", DomainName: "d1.cloudfront.localhost",
		Aliases: []string{"media.example.test"}, Enabled: true,
		Origins:         []*config.Origin{origin},
		DefaultBehavior: openBeh,
		Behaviors:       []*config.Behavior{premiumBeh},
	}
	cfg := &config.Config{
		Distributions: []*config.Distribution{dist},
		PublicKeys:    []*config.PublicKey{pubKey},
		KeyGroups:     []*config.KeyGroup{keyGroup},
	}
	return cfg, dist
}

func TestM6_SignedURL(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	priv, encodedKey := m6KeyPair(t)
	o := customOrigin("o1", host, port)
	cfg, _ := signedURLDistribution(t, encodedKey, o)
	srv := dataplane.New(cfg, newLogger())

	signer := awssign.NewURLSigner(m6KeyPairID, priv)

	t.Run("valid signed URL is served", func(t *testing.T) {
		signed, err := signer.Sign("https://media.example.test/premium/video.mp4", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, signed, nil)
		req.Host = "media.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rr.Code)
		}
	})

	t.Run("unsigned request is denied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://media.example.test/premium/video.mp4", nil)
		req.Host = "media.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
	})

	t.Run("expired signed URL is denied", func(t *testing.T) {
		signed, err := signer.Sign("https://media.example.test/premium/video.mp4", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, signed, nil)
		req.Host = "media.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
	})

	t.Run("unprotected path needs no signature", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://media.example.test/public.html", nil)
		req.Host = "media.example.test"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (default behavior has no key group)", rr.Code)
		}
	})
}

func TestM6_SignedCookies(t *testing.T) {
	originSrv, host, port := newTestOriginServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = originSrv

	priv, encodedKey := m6KeyPair(t)
	o := customOrigin("o1", host, port)
	cfg, _ := signedURLDistribution(t, encodedKey, o)
	srv := dataplane.New(cfg, newLogger())

	signer := awssign.NewCookieSigner(m6KeyPairID, priv)
	policy := &awssign.Policy{Statements: []awssign.Statement{{
		Resource: "https://media.example.test/premium/*",
		Condition: awssign.Condition{
			DateLessThan: awssign.NewAWSEpochTime(time.Now().Add(time.Hour)),
		},
	}}}
	cookies, err := signer.SignWithPolicy(policy)
	if err != nil {
		t.Fatalf("sign cookies: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://media.example.test/premium/clip.mp4", nil)
	req.Host = "media.example.test"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (valid signed cookies)", rr.Code)
	}
}
