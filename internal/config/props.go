package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// decodeProps decodes a resolved property tree into a CloudFormation-shaped
// struct. Unknown properties are ignored on purpose: production templates
// must keep loading as the CloudFormation resource spec evolves.
func decodeProps(props map[string]any, dst any) error {
	b, err := json.Marshal(props)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// flexBool accepts both JSON booleans and their string forms ("true"),
// which CloudFormation treats interchangeably.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	v, err := strconv.ParseBool(strings.Trim(string(data), `"`))
	if err != nil {
		return fmt.Errorf("cannot parse %s as a boolean", string(data))
	}
	*b = flexBool(v)
	return nil
}

// Value returns the boolean, treating an absent (nil) value as def.
func (b *flexBool) Value(def bool) bool {
	if b == nil {
		return def
	}
	return bool(*b)
}

// flexInt accepts JSON numbers and numeric strings.
type flexInt int64

func (n *flexInt) UnmarshalJSON(data []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	if err != nil {
		return fmt.Errorf("cannot parse %s as a number", string(data))
	}
	*n = flexInt(int64(f))
	return nil
}

// Value returns the number, treating an absent (nil) value as def.
func (n *flexInt) Value(def int64) int64 {
	if n == nil {
		return def
	}
	return int64(*n)
}

// flexFloat accepts JSON numbers and numeric strings.
type flexFloat float64

func (n *flexFloat) UnmarshalJSON(data []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	if err != nil {
		return fmt.Errorf("cannot parse %s as a number", string(data))
	}
	*n = flexFloat(f)
	return nil
}

// Value returns the number, treating an absent (nil) value as def.
func (n *flexFloat) Value(def float64) float64 {
	if n == nil {
		return def
	}
	return float64(*n)
}

// stringList accepts a plain JSON array of strings, the API-shaped
// {"Items": [...]} object, and a bare string.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*l = arr
		return nil
	}
	var obj struct{ Items []string }
	if err := json.Unmarshal(data, &obj); err == nil && obj.Items != nil {
		*l = obj.Items
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*l = []string{s}
		return nil
	}
	return fmt.Errorf("cannot parse %s as a list of strings", string(data))
}

// --- AWS::CloudFront::Distribution ---

type distributionProps struct {
	DistributionConfig *distributionConfigProps
}

type distributionConfigProps struct {
	Aliases                      stringList
	CNAMEs                       stringList // legacy spelling of Aliases
	Comment                      string
	ContinuousDeploymentPolicyId string
	CustomErrorResponses         []customErrorResponseProps
	DefaultCacheBehavior         *behaviorProps
	CacheBehaviors               []behaviorProps
	DefaultRootObject            string
	Enabled                      *flexBool
	OriginGroups                 *originGroupsProps
	Origins                      []originProps
	Restrictions                 json.RawMessage // stored, not enforced
	ViewerCertificate            json.RawMessage // ignored: plain HTTP only
	WebACLId                     string          // ignored
	AnycastIpListId              string
	TenantConfig                 json.RawMessage
}

type originGroupsProps struct {
	Quantity *flexInt
	Items    []json.RawMessage
}

type originProps struct {
	Id                    string
	DomainName            string
	OriginPath            string
	ConnectionAttempts    *flexInt
	ConnectionTimeout     *flexInt
	CustomOriginConfig    *customOriginConfigProps
	S3OriginConfig        *s3OriginConfigProps
	OriginAccessControlId string // accepted, not enforced
	OriginCustomHeaders   []originCustomHeaderProps
	VpcOriginConfig       json.RawMessage
}

type customOriginConfigProps struct {
	HTTPPort               *flexInt
	HTTPSPort              *flexInt
	OriginKeepaliveTimeout *flexInt
	OriginProtocolPolicy   string
	OriginReadTimeout      *flexInt
	OriginSSLProtocols     stringList // ignored
}

type s3OriginConfigProps struct {
	OriginAccessIdentity string // accepted, not enforced
	OriginReadTimeout    *flexInt
}

type originCustomHeaderProps struct {
	HeaderName  string
	HeaderValue string
}

type behaviorProps struct {
	PathPattern                string
	TargetOriginId             string
	ViewerProtocolPolicy       string // accepted, never redirects locally
	AllowedMethods             stringList
	CachedMethods              stringList
	CachePolicyId              string
	OriginRequestPolicyId      string
	ResponseHeadersPolicyId    string
	Compress                   *flexBool
	FieldLevelEncryptionId     string
	ForwardedValues            *forwardedValuesProps
	FunctionAssociations       []functionAssociationProps
	LambdaFunctionAssociations []json.RawMessage
	RealtimeLogConfigArn       string
	TrustedKeyGroups           stringList
	TrustedSigners             stringList
	MinTTL                     *flexFloat // legacy, with ForwardedValues
	DefaultTTL                 *flexFloat
	MaxTTL                     *flexFloat
}

type forwardedValuesProps struct {
	QueryString          *flexBool
	Cookies              *forwardedCookiesProps
	Headers              stringList
	QueryStringCacheKeys stringList
}

type forwardedCookiesProps struct {
	Forward          string // none / whitelist / all
	WhitelistedNames stringList
}

type functionAssociationProps struct {
	EventType   string
	FunctionARN string
}

type customErrorResponseProps struct {
	ErrorCode          *flexInt
	ResponseCode       *flexInt
	ResponsePagePath   string
	ErrorCachingMinTTL *flexFloat // ignored: the PoC does not cache
}

// --- AWS::CloudFront::CachePolicy ---

type cachePolicyProps struct {
	CachePolicyConfig *cachePolicyConfigProps
}

type cachePolicyConfigProps struct {
	Name                                     string
	Comment                                  string
	DefaultTTL                               *flexFloat
	MaxTTL                                   *flexFloat
	MinTTL                                   *flexFloat
	ParametersInCacheKeyAndForwardedToOrigin *cacheKeyParamsProps
}

type cacheKeyParamsProps struct {
	EnableAcceptEncodingBrotli *flexBool
	EnableAcceptEncodingGzip   *flexBool
	HeadersConfig              *headersConfigProps
	CookiesConfig              *cookiesConfigProps
	QueryStringsConfig         *queryStringsConfigProps
}

type headersConfigProps struct {
	HeaderBehavior string
	Headers        stringList
}

type cookiesConfigProps struct {
	CookieBehavior string
	Cookies        stringList
}

type queryStringsConfigProps struct {
	QueryStringBehavior string
	QueryStrings        stringList
}

// --- AWS::CloudFront::OriginRequestPolicy ---

type originRequestPolicyProps struct {
	OriginRequestPolicyConfig *originRequestPolicyConfigProps
}

type originRequestPolicyConfigProps struct {
	Name               string
	Comment            string
	HeadersConfig      *headersConfigProps
	CookiesConfig      *cookiesConfigProps
	QueryStringsConfig *queryStringsConfigProps
}

// --- AWS::CloudFront::ResponseHeadersPolicy ---

type responseHeadersPolicyProps struct {
	ResponseHeadersPolicyConfig *responseHeadersPolicyConfigProps
}

type responseHeadersPolicyConfigProps struct {
	Name                      string
	Comment                   string
	CorsConfig                *corsConfigProps
	CustomHeadersConfig       *customHeadersConfigProps
	RemoveHeadersConfig       *removeHeadersConfigProps
	SecurityHeadersConfig     *securityHeadersConfigProps
	ServerTimingHeadersConfig json.RawMessage // ignored
}

type corsConfigProps struct {
	AccessControlAllowCredentials *flexBool
	AccessControlAllowHeaders     stringList
	AccessControlAllowMethods     stringList
	AccessControlAllowOrigins     stringList
	AccessControlExposeHeaders    stringList
	AccessControlMaxAgeSec        *flexInt
	OriginOverride                *flexBool
}

type customHeadersConfigProps struct {
	Items []customHeaderProps
}

type customHeaderProps struct {
	Header   string
	Value    string
	Override *flexBool
}

type removeHeadersConfigProps struct {
	Items []removeHeaderProps
}

type removeHeaderProps struct {
	Header string
}

type securityHeadersConfigProps struct {
	ContentSecurityPolicy   *cspProps
	ContentTypeOptions      *contentTypeOptionsProps
	FrameOptions            *frameOptionsProps
	ReferrerPolicy          *referrerPolicyProps
	StrictTransportSecurity *hstsProps
	XSSProtection           *xssProtectionProps
}

type cspProps struct {
	ContentSecurityPolicy string
	Override              *flexBool
}

type contentTypeOptionsProps struct {
	Override *flexBool
}

type frameOptionsProps struct {
	FrameOption string
	Override    *flexBool
}

type referrerPolicyProps struct {
	ReferrerPolicy string
	Override       *flexBool
}

type hstsProps struct {
	AccessControlMaxAgeSec *flexInt
	IncludeSubdomains      *flexBool
	Preload                *flexBool
	Override               *flexBool
}

type xssProtectionProps struct {
	ModeBlock  *flexBool
	Override   *flexBool
	Protection *flexBool
	ReportUri  string
}

// --- AWS::CloudFront::Function ---

type functionProps struct {
	Name           string
	FunctionCode   string
	FunctionConfig *functionConfigProps
}

type functionConfigProps struct {
	Comment                   string
	Runtime                   string
	KeyValueStoreAssociations []kvsAssociationProps
}

type kvsAssociationProps struct {
	KeyValueStoreARN string
}

// --- AWS::CloudFront::KeyValueStore ---

type keyValueStoreProps struct {
	Name         string
	Comment      string
	ImportSource *importSourceProps
}

type importSourceProps struct {
	SourceArn  string
	SourceType string
}

// --- AWS::CloudFront::PublicKey / KeyGroup / OriginAccessControl ---

type publicKeyProps struct {
	PublicKeyConfig *publicKeyConfigProps
}

type publicKeyConfigProps struct {
	CallerReference string
	Comment         string
	EncodedKey      string
	Name            string
}

type keyGroupProps struct {
	KeyGroupConfig *keyGroupConfigProps
}

type keyGroupConfigProps struct {
	Comment string
	Items   stringList
	Name    string
}

type originAccessControlProps struct {
	OriginAccessControlConfig *originAccessControlConfigProps
}

type originAccessControlConfigProps struct {
	Description                   string
	Name                          string
	OriginAccessControlOriginType string
	SigningBehavior               string
	SigningProtocol               string
}
