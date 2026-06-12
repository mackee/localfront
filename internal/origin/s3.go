package origin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// emptyPayloadHash is the SHA-256 of an empty body. GET/HEAD carry no payload,
// so this is the x-amz-content-sha256 value the request is signed with.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// S3Client is a Fetcher that talks to an S3-compatible store with SigV4-signed
// path-style requests. It forwards the store's response verbatim — including
// 206 (Range), 304 (conditional), and 4xx/5xx errors — so the data plane can
// map and post-process them the way CloudFront does.
type S3Client struct {
	endpoint  *url.URL
	region    string
	creds     aws.Credentials
	signer    *v4.Signer
	transport http.RoundTripper
	now       func() time.Time
}

// NewS3Client builds an S3 client for the given store endpoint. The region is
// the one used to sign requests to the store (its own region, e.g. us-east-1),
// not the production region encoded in origin domain names.
func NewS3Client(endpoint, region, accessKey, secretKey string, transport http.RoundTripper) (*S3Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("S3 endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 endpoint %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q: expected a URL like http://localhost:9000", endpoint)
	}
	if region == "" {
		region = "us-east-1"
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &S3Client{
		endpoint:  u,
		region:    region,
		creds:     aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		signer:    v4.NewSigner(),
		transport: transport,
		now:       time.Now,
	}, nil
}

func (c *S3Client) Fetch(ctx context.Context, req *Request) (*Response, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	u := *c.endpoint
	u.Path = strings.TrimRight(c.endpoint.Path, "/") + "/" + req.Bucket + "/" + req.Key

	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building S3 request: %w", err)
	}
	for name, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(name, v)
		}
	}
	// The standalone SigV4 signer does not set this header (the S3 service
	// middleware normally would), but S3 requires it on every request.
	httpReq.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)

	if err := c.signer.SignHTTP(ctx, c.creds, httpReq, emptyPayloadHash, "s3", c.region, c.now().UTC()); err != nil {
		return nil, fmt.Errorf("signing S3 request: %w", err)
	}

	resp, err := c.transport.RoundTrip(httpReq)
	if err != nil {
		return nil, fmt.Errorf("S3 request to %s failed: %w", req.Bucket, err)
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
	}, nil
}
