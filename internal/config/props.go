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

// FlexBool accepts both JSON booleans and their string forms ("true"),
// which CloudFormation treats interchangeably.
type FlexBool bool

func (b *FlexBool) UnmarshalJSON(data []byte) error {
	v, err := strconv.ParseBool(strings.Trim(string(data), `"`))
	if err != nil {
		return fmt.Errorf("cannot parse %s as a boolean", string(data))
	}
	*b = FlexBool(v)
	return nil
}

// Value returns the boolean, treating an absent (nil) value as def.
func (b *FlexBool) Value(def bool) bool {
	if b == nil {
		return def
	}
	return bool(*b)
}

// FlexInt accepts JSON numbers and numeric strings.
type FlexInt int64

func (n *FlexInt) UnmarshalJSON(data []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	if err != nil {
		return fmt.Errorf("cannot parse %s as a number", string(data))
	}
	*n = FlexInt(int64(f))
	return nil
}

// Value returns the number, treating an absent (nil) value as def.
func (n *FlexInt) Value(def int64) int64 {
	if n == nil {
		return def
	}
	return int64(*n)
}

// FlexFloat accepts JSON numbers and numeric strings.
type FlexFloat float64

func (n *FlexFloat) UnmarshalJSON(data []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(data), `"`), 64)
	if err != nil {
		return fmt.Errorf("cannot parse %s as a number", string(data))
	}
	*n = FlexFloat(f)
	return nil
}

// Value returns the number, treating an absent (nil) value as def.
func (n *FlexFloat) Value(def float64) float64 {
	if n == nil {
		return def
	}
	return float64(*n)
}

// StringList accepts a plain JSON array of strings, the API-shaped
// {"Items": [...]} object, and a bare string.
type StringList []string

func (l *StringList) UnmarshalJSON(data []byte) error {
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
	Aliases                      StringList
	CNAMEs                       StringList // legacy spelling of Aliases
	Comment                      string
	ContinuousDeploymentPolicyId string
	CustomErrorResponses         []customErrorResponseProps
	DefaultCacheBehavior         *behaviorProps
	CacheBehaviors               []behaviorProps
	DefaultRootObject            string
	Enabled                      *FlexBool
	OriginGroups                 *originGroupsProps
	Origins                      []originProps
	Restrictions                 json.RawMessage // stored, not enforced
	ViewerCertificate            json.RawMessage // ignored: plain HTTP only
	WebACLId                     string          // ignored
	AnycastIpListId              string
	TenantConfig                 json.RawMessage
}

type originGroupsProps struct {
	Quantity *FlexInt
	Items    []json.RawMessage
}

type originProps struct {
	Id                    string
	DomainName            string
	OriginPath            string
	ConnectionAttempts    *FlexInt
	ConnectionTimeout     *FlexInt
	CustomOriginConfig    *customOriginConfigProps
	S3OriginConfig        *s3OriginConfigProps
	OriginAccessControlId string // accepted, not enforced
	OriginCustomHeaders   []originCustomHeaderProps
	VpcOriginConfig       json.RawMessage
}

type customOriginConfigProps struct {
	HTTPPort               *FlexInt
	HTTPSPort              *FlexInt
	OriginKeepaliveTimeout *FlexInt
	OriginProtocolPolicy   string
	OriginReadTimeout      *FlexInt
	OriginSSLProtocols     StringList // ignored
}

type s3OriginConfigProps struct {
	OriginAccessIdentity string // accepted, not enforced
	OriginReadTimeout    *FlexInt
}

type originCustomHeaderProps struct {
	HeaderName  string
	HeaderValue string
}

type behaviorProps struct {
	PathPattern                string
	TargetOriginId             string
	ViewerProtocolPolicy       string // accepted, never redirects locally
	AllowedMethods             StringList
	CachedMethods              StringList
	CachePolicyId              string
	OriginRequestPolicyId      string
	ResponseHeadersPolicyId    string
	Compress                   *FlexBool
	FieldLevelEncryptionId     string
	ForwardedValues            *forwardedValuesProps
	FunctionAssociations       []functionAssociationProps
	LambdaFunctionAssociations []json.RawMessage
	RealtimeLogConfigArn       string
	TrustedKeyGroups           StringList
	TrustedSigners             StringList
	MinTTL                     *FlexFloat // legacy, with ForwardedValues
	DefaultTTL                 *FlexFloat
	MaxTTL                     *FlexFloat
}

type forwardedValuesProps struct {
	QueryString          *FlexBool
	Cookies              *forwardedCookiesProps
	Headers              StringList
	QueryStringCacheKeys StringList
}

type forwardedCookiesProps struct {
	Forward          string // none / whitelist / all
	WhitelistedNames StringList
}

type functionAssociationProps struct {
	EventType   string
	FunctionARN string
}

type customErrorResponseProps struct {
	ErrorCode          *FlexInt
	ResponseCode       *FlexInt
	ResponsePagePath   string
	ErrorCachingMinTTL *FlexFloat // ignored: the PoC does not cache
}

// --- AWS::CloudFront::CachePolicy ---

type cachePolicyProps struct {
	CachePolicyConfig *cachePolicyConfigProps
}

type cachePolicyConfigProps struct {
	Name                                     string
	Comment                                  string
	DefaultTTL                               *FlexFloat
	MaxTTL                                   *FlexFloat
	MinTTL                                   *FlexFloat
	ParametersInCacheKeyAndForwardedToOrigin *cacheKeyParamsProps
}

type cacheKeyParamsProps struct {
	EnableAcceptEncodingBrotli *FlexBool
	EnableAcceptEncodingGzip   *FlexBool
	HeadersConfig              *headersConfigProps
	CookiesConfig              *cookiesConfigProps
	QueryStringsConfig         *queryStringsConfigProps
}

type headersConfigProps struct {
	HeaderBehavior string
	Headers        StringList
}

type cookiesConfigProps struct {
	CookieBehavior string
	Cookies        StringList
}

type queryStringsConfigProps struct {
	QueryStringBehavior string
	QueryStrings        StringList
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
	AccessControlAllowCredentials *FlexBool
	AccessControlAllowHeaders     StringList
	AccessControlAllowMethods     StringList
	AccessControlAllowOrigins     StringList
	AccessControlExposeHeaders    StringList
	AccessControlMaxAgeSec        *FlexInt
	OriginOverride                *FlexBool
}

type customHeadersConfigProps struct {
	Items []customHeaderProps
}

type customHeaderProps struct {
	Header   string
	Value    string
	Override *FlexBool
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
	Override              *FlexBool
}

type contentTypeOptionsProps struct {
	Override *FlexBool
}

type frameOptionsProps struct {
	FrameOption string
	Override    *FlexBool
}

type referrerPolicyProps struct {
	ReferrerPolicy string
	Override       *FlexBool
}

type hstsProps struct {
	AccessControlMaxAgeSec *FlexInt
	IncludeSubdomains      *FlexBool
	Preload                *FlexBool
	Override               *FlexBool
}

type xssProtectionProps struct {
	ModeBlock  *FlexBool
	Override   *FlexBool
	Protection *FlexBool
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
	Items   StringList
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
