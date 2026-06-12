// Package config turns parsed CloudFormation templates into the resolved
// model the data plane serves from: distributions, behaviors, origins,
// policies, functions, and key material.
package config

import (
	"strings"
	"time"
)

// Config is the complete resolved configuration of one localfront instance.
type Config struct {
	Distributions        []*Distribution
	Functions            []*Function
	KeyValueStores       []*KeyValueStore
	KeyGroups            []*KeyGroup
	PublicKeys           []*PublicKey
	OriginAccessControls []*OriginAccessControl
	Warnings             []string
}

// Distribution models one AWS::CloudFront::Distribution.
type Distribution struct {
	LogicalID  string
	ID         string // e.g. E2ABCDEFGHIJ3, derived from the logical ID
	ARN        string
	DomainName string // <id, lowercased>.cloudfront.localhost
	Aliases    []string
	Enabled    bool

	DefaultRootObject string
	Origins           []*Origin
	DefaultBehavior   *Behavior
	Behaviors         []*Behavior // in template order; first match wins
	ErrorResponses    []*ErrorResponse
}

// Hostnames returns every hostname this distribution serves.
func (d *Distribution) Hostnames() []string {
	out := make([]string, 0, len(d.Aliases)+1)
	out = append(out, d.DomainName)
	out = append(out, d.Aliases...)
	return out
}

// Origin is one origin of a distribution: either Custom or S3 is set.
type Origin struct {
	ID                 string
	OriginPath         string
	CustomHeaders      []Header
	Custom             *CustomOrigin
	S3                 *S3Origin
	ConnectionAttempts int
	ConnectionTimeout  time.Duration
}

// Header is a static header attached to origin requests.
type Header struct {
	Name  string
	Value string
}

// CustomOrigin is an HTTP(S) origin.
type CustomOrigin struct {
	Host             string
	HTTPPort         int
	HTTPSPort        int
	ProtocolPolicy   string // http-only / https-only / match-viewer
	ReadTimeout      time.Duration
	KeepaliveTimeout time.Duration
}

// S3Origin is an origin served from the external S3-compatible store.
type S3Origin struct {
	Bucket string
	Region string
}

// Behavior is a cache behavior with all references resolved.
type Behavior struct {
	PathPattern           string // "" for the default behavior
	Origin                *Origin
	ViewerProtocolPolicy  string
	AllowedMethods        []string
	CachedMethods         []string
	Compress              bool
	CachePolicy           *CachePolicy
	OriginRequestPolicy   *OriginRequestPolicy   // may be nil
	ResponseHeadersPolicy *ResponseHeadersPolicy // may be nil
	TrustedKeyGroups      []*KeyGroup
	ViewerRequest         *Function // may be nil
	ViewerResponse        *Function // may be nil
}

// ErrorResponse is one CustomErrorResponse entry.
type ErrorResponse struct {
	ErrorCode        int
	ResponseCode     int
	ResponsePagePath string // "" means: keep the origin response body
}

// ListSelection captures the headers/cookies/query-strings selection of a
// cache policy or origin request policy: a behavior keyword plus an optional
// item list (for whitelist / allExcept).
type ListSelection struct {
	Behavior string
	Items    []string
}

// Contains reports whether name is in the item list, case-insensitively.
func (s ListSelection) Contains(name string) bool {
	for _, it := range s.Items {
		if strings.EqualFold(it, name) {
			return true
		}
	}
	return false
}

// CachePolicy determines the cache key and what reaches the origin.
type CachePolicy struct {
	ID           string
	Name         string
	MinTTL       time.Duration
	DefaultTTL   time.Duration
	MaxTTL       time.Duration
	Gzip         bool          // normalize Accept-Encoding: gzip
	Brotli       bool          // normalize Accept-Encoding: br
	Headers      ListSelection // none / whitelist
	Cookies      ListSelection // none / whitelist / allExcept / all
	QueryStrings ListSelection // none / whitelist / allExcept / all
}

// OriginRequestPolicy adds viewer values to origin requests without
// affecting the cache key.
type OriginRequestPolicy struct {
	ID           string
	Name         string
	Headers      ListSelection // none / whitelist / allViewer / allViewerAndWhitelistCloudFront / allExcept
	Cookies      ListSelection // none / whitelist / all / allExcept
	QueryStrings ListSelection // none / whitelist / all / allExcept
}

// ResponseHeadersPolicy adds or removes response headers.
type ResponseHeadersPolicy struct {
	ID            string
	Name          string
	Cors          *CorsConfig
	CustomHeaders []CustomHeader
	RemoveHeaders []string
	Security      *SecurityHeaders
}

// CorsConfig mirrors ResponseHeadersPolicy CorsConfig.
type CorsConfig struct {
	AllowCredentials bool
	AllowHeaders     []string
	AllowMethods     []string
	AllowOrigins     []string
	ExposeHeaders    []string
	MaxAgeSec        int64
	HasMaxAge        bool
	OriginOverride   bool
}

// CustomHeader is a static response header added by a policy.
type CustomHeader struct {
	Name     string
	Value    string
	Override bool
}

// SecurityHeaders mirrors ResponseHeadersPolicy SecurityHeadersConfig.
type SecurityHeaders struct {
	ContentSecurityPolicy   *HeaderValue
	ContentTypeOptions      *HeaderToggle
	FrameOptions            *HeaderValue
	ReferrerPolicy          *HeaderValue
	StrictTransportSecurity *HSTS
	XSSProtection           *XSSProtection
}

// HeaderValue is a security header with a single value.
type HeaderValue struct {
	Value    string
	Override bool
}

// HeaderToggle is a security header that is either present or not.
type HeaderToggle struct {
	Override bool
}

// HSTS mirrors StrictTransportSecurity.
type HSTS struct {
	MaxAgeSec         int64
	IncludeSubdomains bool
	Preload           bool
	Override          bool
}

// XSSProtection mirrors XSSProtection.
type XSSProtection struct {
	Protection bool
	ModeBlock  bool
	ReportURI  string
	Override   bool
}

// Function is a CloudFront Function with its KVS bindings.
type Function struct {
	LogicalID      string
	Name           string
	ARN            string
	Runtime        string
	Code           string
	KeyValueStores []*KeyValueStore
}

// KeyValueStore is an AWS::CloudFront::KeyValueStore.
type KeyValueStore struct {
	LogicalID       string
	Name            string
	ID              string
	ARN             string
	ImportSourceARN string // S3 ARN of the seed file, "" if none
}

// PublicKey is a viewer signing public key (PEM-encoded).
type PublicKey struct {
	LogicalID  string
	ID         string
	Name       string
	EncodedKey string
}

// KeyGroup is a trusted key group for signed URLs / cookies.
type KeyGroup struct {
	LogicalID string
	ID        string
	Name      string
	Keys      []*PublicKey
}

// OriginAccessControl is accepted but not enforced.
type OriginAccessControl struct {
	LogicalID string
	ID        string
	Name      string
}
