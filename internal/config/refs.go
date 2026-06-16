package config

import (
	"strings"

	"github.com/mackee/localfront/internal/cfntmpl"
)

// refValuer tells the template resolver what Ref and Fn::GetAtt evaluate to.
// All values derive from logical IDs (never from resolved properties), so
// reference resolution cannot form cycles.
//
// Besides the CloudFront resource types localfront implements, AWS::S3::Bucket
// gets reference-only support: CDK-synthesized templates routinely point
// distribution origins at `Fn::GetAtt Bucket.RegionalDomainName`, so those
// references must produce real bucket domain names even though the bucket
// resource itself is skipped.
type refValuer struct {
	resources map[string]*cfntmpl.RawResource
}

func newRefValuer(resources []*cfntmpl.RawResource) *refValuer {
	m := make(map[string]*cfntmpl.RawResource, len(resources))
	for _, r := range resources {
		m[r.LogicalID] = r
	}
	return &refValuer{resources: m}
}

const fixedTimestamp = "1970-01-01T00:00:00Z"

func (rv *refValuer) RefValue(logicalID string) (string, bool) {
	res, ok := rv.resources[logicalID]
	if !ok {
		return "", false
	}
	switch res.Type {
	case "AWS::CloudFront::Distribution":
		return distributionID(logicalID), true
	case "AWS::CloudFront::Function":
		return functionARN(rawFunctionName(res)), true
	case "AWS::CloudFront::KeyValueStore":
		// Ref returns the store name.
		if name, ok := rawStringProp(res, "Name"); ok {
			return name, true
		}
		return logicalID, true
	case "AWS::CloudFront::CachePolicy":
		return uuidID("cache-policy", logicalID), true
	case "AWS::CloudFront::OriginRequestPolicy":
		return uuidID("origin-request-policy", logicalID), true
	case "AWS::CloudFront::ResponseHeadersPolicy":
		return uuidID("response-headers-policy", logicalID), true
	case "AWS::CloudFront::PublicKey":
		return publicKeyID(logicalID), true
	case "AWS::CloudFront::KeyGroup":
		return uuidID("key-group", logicalID), true
	case "AWS::CloudFront::OriginAccessControl":
		return oacID(logicalID), true
	case "AWS::S3::Bucket":
		return bucketNameOf(res), true
	}
	return "", false
}

func (rv *refValuer) AttValue(logicalID, attr string) (string, bool) {
	res, ok := rv.resources[logicalID]
	if !ok {
		return "", false
	}
	switch res.Type {
	case "AWS::CloudFront::Distribution":
		id := distributionID(logicalID)
		switch attr {
		case "Id":
			return id, true
		case "DomainName":
			return distributionDomain(id), true
		case "ARN":
			return distributionARN(id), true
		}
	case "AWS::CloudFront::Function":
		switch attr {
		case "FunctionARN", "FunctionMetadata.FunctionARN":
			return functionARN(rawFunctionName(res)), true
		case "Stage":
			return "LIVE", true
		}
	case "AWS::CloudFront::KeyValueStore":
		switch attr {
		case "Arn":
			return keyValueStoreARN(uuidID("key-value-store", logicalID)), true
		case "Id":
			return uuidID("key-value-store", logicalID), true
		case "Status":
			return "READY", true
		}
	case "AWS::CloudFront::CachePolicy":
		switch attr {
		case "Id":
			return uuidID("cache-policy", logicalID), true
		case "LastModifiedTime":
			return fixedTimestamp, true
		}
	case "AWS::CloudFront::OriginRequestPolicy":
		switch attr {
		case "Id":
			return uuidID("origin-request-policy", logicalID), true
		case "LastModifiedTime":
			return fixedTimestamp, true
		}
	case "AWS::CloudFront::ResponseHeadersPolicy":
		switch attr {
		case "Id":
			return uuidID("response-headers-policy", logicalID), true
		case "LastModifiedTime":
			return fixedTimestamp, true
		}
	case "AWS::CloudFront::PublicKey":
		switch attr {
		case "Id":
			return publicKeyID(logicalID), true
		case "CreatedTime":
			return fixedTimestamp, true
		}
	case "AWS::CloudFront::KeyGroup":
		switch attr {
		case "Id":
			return uuidID("key-group", logicalID), true
		case "LastModifiedTime":
			return fixedTimestamp, true
		}
	case "AWS::CloudFront::OriginAccessControl":
		if attr == "Id" {
			return oacID(logicalID), true
		}
	case "AWS::S3::Bucket":
		name := bucketNameOf(res)
		switch attr {
		case "Arn":
			return "arn:aws:s3:::" + name, true
		case "DomainName":
			return name + ".s3.amazonaws.com", true
		case "RegionalDomainName":
			return name + ".s3." + cfntmpl.DefaultRegion + ".amazonaws.com", true
		case "DualStackDomainName":
			return name + ".s3.dualstack." + cfntmpl.DefaultRegion + ".amazonaws.com", true
		case "WebsiteURL":
			return "http://" + name + ".s3-website-" + cfntmpl.DefaultRegion + ".amazonaws.com", true
		}
	}
	return "", false
}

func distributionDomain(id string) string {
	return strings.ToLower(id) + ".cloudfront.localhost"
}

// rawStringProp reads a property from an unresolved resource if and only if
// it is a plain string (not an intrinsic).
func rawStringProp(res *cfntmpl.RawResource, key string) (string, bool) {
	props, ok := res.Properties.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := props[key].(string)
	return s, ok && s != ""
}

// rawFunctionName derives the function name used in generated ARNs. The Name
// property is required by CloudFormation and is a literal string in practice
// (CDK synthesizes literals); if it is an intrinsic we fall back to the
// logical ID so ARNs stay deterministic.
func rawFunctionName(res *cfntmpl.RawResource) string {
	if name, ok := rawStringProp(res, "Name"); ok {
		return name
	}
	return res.LogicalID
}

// bucketNameOf derives the bucket name for reference-only S3 buckets:
// the literal BucketName property when present, otherwise a name derived
// from the logical ID the way CloudFormation-generated names look.
func bucketNameOf(res *cfntmpl.RawResource) string {
	if name, ok := rawStringProp(res, "BucketName"); ok {
		return name
	}
	return sanitizeBucketName(res.LogicalID)
}

func sanitizeBucketName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}
