package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mackee/localfront/internal/cfntmpl"
)

var supportedResourceTypes = map[string]bool{
	"AWS::CloudFront::Distribution":          true,
	"AWS::CloudFront::Function":              true,
	"AWS::CloudFront::KeyValueStore":         true,
	"AWS::CloudFront::CachePolicy":           true,
	"AWS::CloudFront::OriginRequestPolicy":   true,
	"AWS::CloudFront::ResponseHeadersPolicy": true,
	"AWS::CloudFront::PublicKey":             true,
	"AWS::CloudFront::KeyGroup":              true,
	"AWS::CloudFront::OriginAccessControl":   true,
}

// Load parses template sources and builds the resolved configuration.
func Load(sources []cfntmpl.Source, parameters map[string]string) (*Config, error) {
	parsed, err := cfntmpl.Parse(sources)
	if err != nil {
		return nil, err
	}
	return Build(parsed, parameters)
}

// Build resolves intrinsics and turns the supported resources into a Config.
func Build(parsed *cfntmpl.Parsed, parameters map[string]string) (*Config, error) {
	raw := parsed.Resources()
	cfg := &Config{}
	for _, r := range raw {
		if !supportedResourceTypes[r.Type] {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("%s: skipping resource %s of unsupported type %s", r.Source, r.LogicalID, r.Type))
		}
	}
	resolved, err := parsed.Resolve(cfntmpl.ResolveOptions{
		Parameters: parameters,
		Refs:       newRefValuer(raw),
		Include:    func(r *cfntmpl.RawResource) bool { return supportedResourceTypes[r.Type] },
	})
	if err != nil {
		return nil, err
	}
	cfg.Warnings = append(cfg.Warnings, resolved.Warnings...)

	managed, err := loadManaged()
	if err != nil {
		return nil, err
	}
	b := &builder{
		cfg:                     cfg,
		rawByLogicalID:          map[string]*cfntmpl.RawResource{},
		cachePolicies:           map[string]*CachePolicy{},
		originRequestPolicies:   map[string]*OriginRequestPolicy{},
		responseHeadersPolicies: map[string]*ResponseHeadersPolicy{},
		publicKeys:              map[string]*PublicKey{},
		keyGroups:               map[string]*KeyGroup{},
		functionsByARN:          map[string]*Function{},
		kvsByARN:                map[string]*KeyValueStore{},
		aliasOwner:              map[string]string{},
	}
	for _, r := range raw {
		b.rawByLogicalID[r.LogicalID] = r
	}
	maps.Copy(b.cachePolicies, managed.cachePolicies)
	maps.Copy(b.originRequestPolicies, managed.originRequestPolicies)
	maps.Copy(b.responseHeadersPolicies, managed.responseHeadersPolicies)

	// Resources referenced by distributions are built first; distributions
	// come last so every lookup can be resolved.
	passes := []struct {
		typ   string
		build func(res *cfntmpl.Resource) error
	}{
		{"AWS::CloudFront::OriginAccessControl", b.originAccessControl},
		{"AWS::CloudFront::KeyValueStore", b.keyValueStore},
		{"AWS::CloudFront::PublicKey", b.publicKey},
		{"AWS::CloudFront::KeyGroup", b.keyGroup},
		{"AWS::CloudFront::CachePolicy", b.cachePolicy},
		{"AWS::CloudFront::OriginRequestPolicy", b.originRequestPolicy},
		{"AWS::CloudFront::ResponseHeadersPolicy", b.responseHeadersPolicy},
		{"AWS::CloudFront::Function", b.function},
		{"AWS::CloudFront::Distribution", b.distribution},
	}
	for _, pass := range passes {
		for _, res := range resolved.Resources {
			if res.Type != pass.typ {
				continue
			}
			if err := pass.build(res); err != nil {
				return nil, fmt.Errorf("%s: Resources/%s: %w", res.Source, res.LogicalID, err)
			}
		}
	}
	return cfg, nil
}

type builder struct {
	cfg                     *Config
	rawByLogicalID          map[string]*cfntmpl.RawResource
	cachePolicies           map[string]*CachePolicy
	originRequestPolicies   map[string]*OriginRequestPolicy
	responseHeadersPolicies map[string]*ResponseHeadersPolicy
	publicKeys              map[string]*PublicKey
	keyGroups               map[string]*KeyGroup
	functionsByARN          map[string]*Function
	kvsByARN                map[string]*KeyValueStore
	aliasOwner              map[string]string
}

func (b *builder) warnf(format string, args ...any) {
	b.cfg.Warnings = append(b.cfg.Warnings, fmt.Sprintf(format, args...))
}

// originAccessControl is accepted but not enforced: localfront always talks
// to the object store with the credentials given at startup.
func (b *builder) originAccessControl(res *cfntmpl.Resource) error {
	var p originAccessControlProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	oac := &OriginAccessControl{
		LogicalID: res.LogicalID,
		ID:        oacID(res.LogicalID),
	}
	if p.OriginAccessControlConfig != nil {
		oac.Name = p.OriginAccessControlConfig.Name
	}
	b.cfg.OriginAccessControls = append(b.cfg.OriginAccessControls, oac)
	return nil
}

func (b *builder) keyValueStore(res *cfntmpl.Resource) error {
	var p keyValueStoreProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	if p.Name == "" {
		return fmt.Errorf("KeyValueStore Name is required")
	}
	id := uuidID("key-value-store", res.LogicalID)
	kvs := &KeyValueStore{
		LogicalID: res.LogicalID,
		Name:      p.Name,
		ID:        id,
		ARN:       keyValueStoreARN(id),
	}
	if p.ImportSource != nil {
		if !strings.EqualFold(p.ImportSource.SourceType, "S3") {
			return fmt.Errorf("KeyValueStore ImportSource SourceType must be S3, got %q", p.ImportSource.SourceType)
		}
		kvs.ImportSourceARN = p.ImportSource.SourceArn
	}
	b.cfg.KeyValueStores = append(b.cfg.KeyValueStores, kvs)
	b.kvsByARN[kvs.ARN] = kvs
	return nil
}

func (b *builder) publicKey(res *cfntmpl.Resource) error {
	var p publicKeyProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	if p.PublicKeyConfig == nil || p.PublicKeyConfig.EncodedKey == "" {
		return fmt.Errorf("PublicKeyConfig.EncodedKey is required")
	}
	pk := &PublicKey{
		LogicalID:  res.LogicalID,
		ID:         publicKeyID(res.LogicalID),
		Name:       p.PublicKeyConfig.Name,
		EncodedKey: p.PublicKeyConfig.EncodedKey,
	}
	b.cfg.PublicKeys = append(b.cfg.PublicKeys, pk)
	b.publicKeys[pk.ID] = pk
	return nil
}

func (b *builder) keyGroup(res *cfntmpl.Resource) error {
	var p keyGroupProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	if p.KeyGroupConfig == nil {
		return fmt.Errorf("KeyGroupConfig is required")
	}
	kg := &KeyGroup{
		LogicalID: res.LogicalID,
		ID:        uuidID("key-group", res.LogicalID),
		Name:      p.KeyGroupConfig.Name,
	}
	for _, keyID := range p.KeyGroupConfig.Items {
		if cfntmpl.IsUnresolved(keyID) {
			return fmt.Errorf("KeyGroup references a public key that could not be resolved: %s", keyID)
		}
		pk, ok := b.publicKeys[keyID]
		if !ok {
			return fmt.Errorf("KeyGroup references public key %s, which is not defined in the loaded templates", keyID)
		}
		kg.Keys = append(kg.Keys, pk)
	}
	b.cfg.KeyGroups = append(b.cfg.KeyGroups, kg)
	b.keyGroups[kg.ID] = kg
	return nil
}

func (b *builder) cachePolicy(res *cfntmpl.Resource) error {
	var p cachePolicyProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	cp, err := cachePolicyFromProps(uuidID("cache-policy", res.LogicalID), p.CachePolicyConfig)
	if err != nil {
		return err
	}
	b.cachePolicies[cp.ID] = cp
	return nil
}

func (b *builder) originRequestPolicy(res *cfntmpl.Resource) error {
	var p originRequestPolicyProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	orp, err := originRequestPolicyFromProps(uuidID("origin-request-policy", res.LogicalID), p.OriginRequestPolicyConfig)
	if err != nil {
		return err
	}
	b.originRequestPolicies[orp.ID] = orp
	return nil
}

func (b *builder) responseHeadersPolicy(res *cfntmpl.Resource) error {
	var p responseHeadersPolicyProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	rhp, err := responseHeadersPolicyFromProps(uuidID("response-headers-policy", res.LogicalID), p.ResponseHeadersPolicyConfig)
	if err != nil {
		return err
	}
	b.responseHeadersPolicies[rhp.ID] = rhp
	return nil
}

func (b *builder) function(res *cfntmpl.Resource) error {
	var p functionProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	if p.Name == "" {
		return fmt.Errorf("Function Name is required")
	}
	if p.FunctionCode == "" {
		return fmt.Errorf("FunctionCode is required")
	}
	fn := &Function{
		LogicalID: res.LogicalID,
		Name:      p.Name,
		ARN:       functionARN(rawFunctionName(b.rawByLogicalID[res.LogicalID])),
		Runtime:   "cloudfront-js-2.0",
		Code:      p.FunctionCode,
	}
	if p.FunctionConfig != nil {
		if rt := p.FunctionConfig.Runtime; rt != "" {
			if rt != "cloudfront-js-1.0" && rt != "cloudfront-js-2.0" {
				return fmt.Errorf("unsupported function runtime %q", rt)
			}
			fn.Runtime = rt
		}
		for _, assoc := range p.FunctionConfig.KeyValueStoreAssociations {
			arn := assoc.KeyValueStoreARN
			if cfntmpl.IsUnresolved(arn) {
				return fmt.Errorf("function references a key-value store that could not be resolved: %s", arn)
			}
			kvs, ok := b.kvsByARN[arn]
			if !ok {
				return fmt.Errorf("function references key-value store %s, which is not defined in the loaded templates", arn)
			}
			fn.KeyValueStores = append(fn.KeyValueStores, kvs)
		}
	}
	b.cfg.Functions = append(b.cfg.Functions, fn)
	b.functionsByARN[fn.ARN] = fn
	return nil
}

func (b *builder) distribution(res *cfntmpl.Resource) error {
	var p distributionProps
	if err := decodeProps(res.Properties, &p); err != nil {
		return err
	}
	cfg := p.DistributionConfig
	if cfg == nil {
		return fmt.Errorf("DistributionConfig is required")
	}
	switch {
	case cfg.ContinuousDeploymentPolicyId != "":
		return fmt.Errorf("continuous deployment is not supported")
	case cfg.AnycastIpListId != "":
		return fmt.Errorf("anycast static IPs are not supported")
	case len(cfg.TenantConfig) > 0:
		return fmt.Errorf("multi-tenant (SaaS Manager) distributions are not supported")
	}
	if og := cfg.OriginGroups; og != nil && (og.Quantity.Value(0) > 0 || len(og.Items) > 0) {
		return fmt.Errorf("origin group failover is not implemented yet")
	}

	id := distributionID(res.LogicalID)
	d := &Distribution{
		LogicalID:         res.LogicalID,
		ID:                id,
		ARN:               distributionARN(id),
		DomainName:        distributionDomain(id),
		Enabled:           cfg.Enabled.Value(true),
		DefaultRootObject: strings.TrimPrefix(cfg.DefaultRootObject, "/"),
	}
	if !d.Enabled {
		b.warnf("distribution %s is disabled (Enabled: false) and will not be served", res.LogicalID)
	}

	for _, alias := range append(append(StringList{}, cfg.Aliases...), cfg.CNAMEs...) {
		if cfntmpl.IsUnresolved(alias) {
			return fmt.Errorf("alias could not be resolved: %s", alias)
		}
		alias = strings.ToLower(alias)
		if owner, dup := b.aliasOwner[alias]; dup {
			if owner == res.LogicalID {
				continue
			}
			return fmt.Errorf("alias %s is already used by distribution %s", alias, owner)
		}
		b.aliasOwner[alias] = res.LogicalID
		d.Aliases = append(d.Aliases, alias)
	}

	if len(cfg.Origins) == 0 {
		return fmt.Errorf("at least one origin is required")
	}
	originsByID := map[string]*Origin{}
	for i, op := range cfg.Origins {
		o, err := buildOrigin(&op)
		if err != nil {
			return fmt.Errorf("origin %d (%s): %w", i, op.Id, err)
		}
		if _, dup := originsByID[o.ID]; dup {
			return fmt.Errorf("origin %d: duplicate origin Id %q", i, o.ID)
		}
		originsByID[o.ID] = o
		d.Origins = append(d.Origins, o)
	}

	if cfg.DefaultCacheBehavior == nil {
		return fmt.Errorf("DefaultCacheBehavior is required")
	}
	defaultBehavior, err := b.buildBehavior(cfg.DefaultCacheBehavior, originsByID, true, res.LogicalID, 0)
	if err != nil {
		return fmt.Errorf("DefaultCacheBehavior: %w", err)
	}
	d.DefaultBehavior = defaultBehavior
	for i := range cfg.CacheBehaviors {
		bh, err := b.buildBehavior(&cfg.CacheBehaviors[i], originsByID, false, res.LogicalID, i)
		if err != nil {
			return fmt.Errorf("CacheBehaviors/%d: %w", i, err)
		}
		d.Behaviors = append(d.Behaviors, bh)
	}

	for i, er := range cfg.CustomErrorResponses {
		if er.ErrorCode == nil {
			return fmt.Errorf("CustomErrorResponses/%d: ErrorCode is required", i)
		}
		resp := &ErrorResponse{
			ErrorCode:        int(er.ErrorCode.Value(0)),
			ResponseCode:     int(er.ResponseCode.Value(er.ErrorCode.Value(0))),
			ResponsePagePath: er.ResponsePagePath,
		}
		if resp.ResponsePagePath != "" && !strings.HasPrefix(resp.ResponsePagePath, "/") {
			return fmt.Errorf("CustomErrorResponses/%d: ResponsePagePath must start with /", i)
		}
		d.ErrorResponses = append(d.ErrorResponses, resp)
	}

	b.cfg.Distributions = append(b.cfg.Distributions, d)
	return nil
}

var s3DomainPattern = regexp.MustCompile(`^(?P<bucket>[a-z0-9][a-z0-9.-]*?)\.s3(?:[.-](?:dualstack\.)?(?P<region>[a-z0-9-]+))?\.amazonaws\.com$`)

// parseS3Domain extracts bucket and region from an S3 REST endpoint domain
// (virtual-hosted style), e.g. assets.s3.us-east-1.amazonaws.com.
func parseS3Domain(domain string) (bucket, region string, ok bool) {
	m := s3DomainPattern.FindStringSubmatch(strings.ToLower(domain))
	if m == nil {
		return "", "", false
	}
	bucket = m[1]
	region = m[2]
	if region == "" {
		region = cfntmpl.DefaultRegion
	}
	return bucket, region, true
}

func buildOrigin(op *originProps) (*Origin, error) {
	if op.Id == "" {
		return nil, fmt.Errorf("origin Id is required")
	}
	if op.DomainName == "" {
		return nil, fmt.Errorf("origin DomainName is required")
	}
	if cfntmpl.IsUnresolved(op.DomainName) {
		return nil, fmt.Errorf("origin DomainName could not be resolved: %s (define the referenced resource in a loaded template or replace the reference with a literal value)", op.DomainName)
	}
	if len(op.VpcOriginConfig) > 0 {
		return nil, fmt.Errorf("VPC origins are not supported")
	}
	if op.OriginPath != "" && !strings.HasPrefix(op.OriginPath, "/") {
		return nil, fmt.Errorf("OriginPath must start with / (got %q)", op.OriginPath)
	}
	o := &Origin{
		ID:                 op.Id,
		OriginPath:         op.OriginPath,
		ConnectionAttempts: int(op.ConnectionAttempts.Value(3)),
		ConnectionTimeout:  time.Duration(op.ConnectionTimeout.Value(10)) * time.Second,
	}
	for _, h := range op.OriginCustomHeaders {
		o.CustomHeaders = append(o.CustomHeaders, Header{Name: h.HeaderName, Value: h.HeaderValue})
	}
	switch {
	case op.S3OriginConfig != nil && op.CustomOriginConfig != nil:
		return nil, fmt.Errorf("origin must not have both S3OriginConfig and CustomOriginConfig")
	case op.S3OriginConfig != nil:
		if strings.Contains(op.DomainName, ".s3-website") {
			return nil, fmt.Errorf("S3 website endpoints are custom origins; use CustomOriginConfig for %s", op.DomainName)
		}
		bucket, region, ok := parseS3Domain(op.DomainName)
		if !ok {
			return nil, fmt.Errorf("cannot derive a bucket from S3 origin domain %q (expected <bucket>.s3[.<region>].amazonaws.com)", op.DomainName)
		}
		o.S3 = &S3Origin{Bucket: bucket, Region: region}
	case op.CustomOriginConfig != nil:
		c := op.CustomOriginConfig
		switch c.OriginProtocolPolicy {
		case "http-only", "https-only", "match-viewer":
		case "":
			return nil, fmt.Errorf("CustomOriginConfig.OriginProtocolPolicy is required")
		default:
			return nil, fmt.Errorf("invalid OriginProtocolPolicy %q", c.OriginProtocolPolicy)
		}
		o.Custom = &CustomOrigin{
			Host:             op.DomainName,
			HTTPPort:         int(c.HTTPPort.Value(80)),
			HTTPSPort:        int(c.HTTPSPort.Value(443)),
			ProtocolPolicy:   c.OriginProtocolPolicy,
			ReadTimeout:      time.Duration(c.OriginReadTimeout.Value(30)) * time.Second,
			KeepaliveTimeout: time.Duration(c.OriginKeepaliveTimeout.Value(5)) * time.Second,
		}
	default:
		return nil, fmt.Errorf("origin needs either S3OriginConfig or CustomOriginConfig")
	}
	return o, nil
}

var (
	methodsGetHead        = []string{"GET", "HEAD"}
	methodsGetHeadOptions = []string{"GET", "HEAD", "OPTIONS"}
	methodsAll            = []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"}
)

func normalizeMethods(kind string, methods StringList, def []string, allowAll bool) ([]string, error) {
	if len(methods) == 0 {
		return def, nil
	}
	out := make([]string, 0, len(methods))
	for _, m := range methods {
		out = append(out, strings.ToUpper(m))
	}
	slices.Sort(out)
	switch {
	case slices.Equal(out, methodsGetHead), slices.Equal(out, methodsGetHeadOptions):
		return out, nil
	case allowAll && slices.Equal(out, methodsAll):
		return out, nil
	}
	return nil, fmt.Errorf("invalid %s %v", kind, []string(methods))
}

func (b *builder) buildBehavior(props *behaviorProps, origins map[string]*Origin, isDefault bool, distLogicalID string, idx int) (*Behavior, error) {
	if isDefault && props.PathPattern != "" {
		return nil, fmt.Errorf("DefaultCacheBehavior must not have a PathPattern")
	}
	if !isDefault && props.PathPattern == "" {
		return nil, fmt.Errorf("PathPattern is required")
	}
	switch {
	case len(props.LambdaFunctionAssociations) > 0:
		return nil, fmt.Errorf("Lambda@Edge is intentionally not supported; use CloudFront Functions")
	case props.FieldLevelEncryptionId != "":
		return nil, fmt.Errorf("field-level encryption is not supported")
	case props.RealtimeLogConfigArn != "":
		return nil, fmt.Errorf("real-time logs are not supported")
	case len(props.TrustedSigners) > 0:
		return nil, fmt.Errorf("legacy TrustedSigners are not supported; use TrustedKeyGroups")
	}
	origin, ok := origins[props.TargetOriginId]
	if !ok {
		return nil, fmt.Errorf("TargetOriginId %q does not match any origin", props.TargetOriginId)
	}
	bh := &Behavior{
		PathPattern:          props.PathPattern,
		Origin:               origin,
		ViewerProtocolPolicy: props.ViewerProtocolPolicy,
		Compress:             props.Compress.Value(false),
	}
	var err error
	if bh.AllowedMethods, err = normalizeMethods("AllowedMethods", props.AllowedMethods, methodsGetHead, true); err != nil {
		return nil, err
	}
	if bh.CachedMethods, err = normalizeMethods("CachedMethods", props.CachedMethods, methodsGetHead, false); err != nil {
		return nil, err
	}

	switch {
	case props.CachePolicyId != "" && props.ForwardedValues != nil:
		return nil, fmt.Errorf("CachePolicyId and legacy ForwardedValues cannot be combined")
	case props.CachePolicyId != "":
		if cfntmpl.IsUnresolved(props.CachePolicyId) {
			return nil, fmt.Errorf("CachePolicyId could not be resolved: %s", props.CachePolicyId)
		}
		cp, ok := b.cachePolicies[props.CachePolicyId]
		if !ok {
			return nil, fmt.Errorf("cache policy %s is neither defined in the loaded templates nor a managed policy", props.CachePolicyId)
		}
		bh.CachePolicy = cp
	case props.ForwardedValues != nil:
		name := fmt.Sprintf("legacy-forwarded-values-%s-%d", distLogicalID, idx)
		bh.CachePolicy = cachePolicyFromForwardedValues(name, props)
	default:
		return nil, fmt.Errorf("a behavior requires CachePolicyId (or legacy ForwardedValues)")
	}

	if props.OriginRequestPolicyId != "" {
		if props.ForwardedValues != nil {
			return nil, fmt.Errorf("OriginRequestPolicyId and legacy ForwardedValues cannot be combined")
		}
		if cfntmpl.IsUnresolved(props.OriginRequestPolicyId) {
			return nil, fmt.Errorf("OriginRequestPolicyId could not be resolved: %s", props.OriginRequestPolicyId)
		}
		orp, ok := b.originRequestPolicies[props.OriginRequestPolicyId]
		if !ok {
			return nil, fmt.Errorf("origin request policy %s is neither defined in the loaded templates nor a managed policy", props.OriginRequestPolicyId)
		}
		bh.OriginRequestPolicy = orp
	}
	if props.ResponseHeadersPolicyId != "" {
		if cfntmpl.IsUnresolved(props.ResponseHeadersPolicyId) {
			return nil, fmt.Errorf("ResponseHeadersPolicyId could not be resolved: %s", props.ResponseHeadersPolicyId)
		}
		rhp, ok := b.responseHeadersPolicies[props.ResponseHeadersPolicyId]
		if !ok {
			return nil, fmt.Errorf("response headers policy %s is neither defined in the loaded templates nor a managed policy", props.ResponseHeadersPolicyId)
		}
		bh.ResponseHeadersPolicy = rhp
	}

	for _, kgID := range props.TrustedKeyGroups {
		if cfntmpl.IsUnresolved(kgID) {
			return nil, fmt.Errorf("TrustedKeyGroups entry could not be resolved: %s", kgID)
		}
		kg, ok := b.keyGroups[kgID]
		if !ok {
			return nil, fmt.Errorf("key group %s is not defined in the loaded templates", kgID)
		}
		bh.TrustedKeyGroups = append(bh.TrustedKeyGroups, kg)
	}

	for i, assoc := range props.FunctionAssociations {
		if cfntmpl.IsUnresolved(assoc.FunctionARN) {
			return nil, fmt.Errorf("FunctionAssociations/%d: FunctionARN could not be resolved: %s", i, assoc.FunctionARN)
		}
		fn, ok := b.functionsByARN[assoc.FunctionARN]
		if !ok {
			return nil, fmt.Errorf("FunctionAssociations/%d: function %s is not defined in the loaded templates", i, assoc.FunctionARN)
		}
		switch assoc.EventType {
		case "viewer-request":
			if bh.ViewerRequest != nil {
				return nil, fmt.Errorf("FunctionAssociations/%d: duplicate viewer-request association", i)
			}
			bh.ViewerRequest = fn
		case "viewer-response":
			if bh.ViewerResponse != nil {
				return nil, fmt.Errorf("FunctionAssociations/%d: duplicate viewer-response association", i)
			}
			bh.ViewerResponse = fn
		default:
			return nil, fmt.Errorf("FunctionAssociations/%d: invalid EventType %q (CloudFront Functions support viewer-request and viewer-response)", i, assoc.EventType)
		}
	}
	return bh, nil
}
