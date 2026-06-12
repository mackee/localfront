package config

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mackee/localfront/internal/cfntmpl"
)

func src(name, data string) cfntmpl.Source {
	return cfntmpl.Source{Name: name, Data: []byte(data)}
}

// mustLoad calls Load and fails if it returns an error.
func mustLoad(t *testing.T, sources []cfntmpl.Source, params map[string]string) *Config {
	t.Helper()
	cfg, err := Load(sources, params)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	return cfg
}

// ── C1) Happy path ─────────────────────────────────────────────────────────

const happyPathTemplate = `
Parameters:
  AppEnv:
    Type: String
    Default: test

Resources:
  MyKVS:
    Type: AWS::CloudFront::KeyValueStore
    Properties:
      Name: my-kvs

  MyPublicKey:
    Type: AWS::CloudFront::PublicKey
    Properties:
      PublicKeyConfig:
        Name: my-public-key
        CallerReference: ref1
        EncodedKey: "-----BEGIN PUBLIC KEY-----\nMFwwDQYJKoZIhvcNAQEBBQADSwAwSAJBAMIU2c\n-----END PUBLIC KEY-----"

  MyKeyGroup:
    Type: AWS::CloudFront::KeyGroup
    Properties:
      KeyGroupConfig:
        Name: my-key-group
        Items:
          - !Ref MyPublicKey

  MyFunction:
    Type: AWS::CloudFront::Function
    Properties:
      Name: my-function
      FunctionCode: "function handler(event) { return event.request; }"
      FunctionConfig:
        Runtime: cloudfront-js-2.0
        KeyValueStoreAssociations:
          - KeyValueStoreARN: !GetAtt MyKVS.Arn

  MyTemplateCP:
    Type: AWS::CloudFront::CachePolicy
    Properties:
      CachePolicyConfig:
        Name: my-template-cp
        MinTTL: 0
        DefaultTTL: 300
        MaxTTL: 3600
        ParametersInCacheKeyAndForwardedToOrigin:
          EnableAcceptEncodingGzip: true
          EnableAcceptEncodingBrotli: false
          HeadersConfig:
            HeaderBehavior: none
          CookiesConfig:
            CookieBehavior: none
          QueryStringsConfig:
            QueryStringBehavior: none

  MyDistribution:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases:
          - assets.example.test
        DefaultRootObject: index.html
        Origins:
          - Id: s3-origin
            DomainName: assets.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
          - Id: api-origin
            DomainName: api.internal
            CustomOriginConfig:
              OriginProtocolPolicy: http-only
              HTTPPort: 8080
        DefaultCacheBehavior:
          TargetOriginId: s3-origin
          ViewerProtocolPolicy: redirect-to-https
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
        CacheBehaviors:
          - PathPattern: "/api/*"
            TargetOriginId: api-origin
            ViewerProtocolPolicy: allow-all
            CachePolicyId: !Ref MyTemplateCP
            OriginRequestPolicyId: 216adef6-5c7f-47e4-b989-5492eafa07d3
            FunctionAssociations:
              - EventType: viewer-request
                FunctionARN: !GetAtt MyFunction.FunctionARN
            TrustedKeyGroups:
              - !Ref MyKeyGroup
`

var distributionIDPattern = regexp.MustCompile(`^E[A-Z0-9]{13}$`)

func TestHappyPath(t *testing.T) {
	cfg := mustLoad(t, []cfntmpl.Source{src("happy.yaml", happyPathTemplate)}, nil)

	if len(cfg.Distributions) != 1 {
		t.Fatalf("expected 1 distribution, got %d", len(cfg.Distributions))
	}
	d := cfg.Distributions[0]

	// distribution ID format
	if !distributionIDPattern.MatchString(d.ID) {
		t.Errorf("distribution ID %q does not match ^E[A-Z0-9]{13}$", d.ID)
	}

	// DomainName == lowercase(ID) + ".cloudfront.localhost"
	wantDomain := strings.ToLower(d.ID) + ".cloudfront.localhost"
	if d.DomainName != wantDomain {
		t.Errorf("DomainName: expected %q, got %q", wantDomain, d.DomainName)
	}

	// Aliases lowercased
	if len(d.Aliases) != 1 || d.Aliases[0] != "assets.example.test" {
		t.Errorf("Aliases: expected [assets.example.test], got %v", d.Aliases)
	}

	// DefaultRootObject
	if d.DefaultRootObject != "index.html" {
		t.Errorf("DefaultRootObject: expected 'index.html', got %q", d.DefaultRootObject)
	}

	// Origins
	if len(d.Origins) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(d.Origins))
	}

	s3o := d.Origins[0]
	if s3o.S3 == nil {
		t.Fatal("first origin should be S3")
	}
	if s3o.S3.Bucket != "assets" {
		t.Errorf("S3 bucket: expected 'assets', got %q", s3o.S3.Bucket)
	}
	if s3o.S3.Region != "us-east-1" {
		t.Errorf("S3 region: expected 'us-east-1', got %q", s3o.S3.Region)
	}

	customO := d.Origins[1]
	if customO.Custom == nil {
		t.Fatal("second origin should be Custom")
	}
	if customO.Custom.Host != "api.internal" {
		t.Errorf("custom host: expected 'api.internal', got %q", customO.Custom.Host)
	}
	if customO.Custom.HTTPPort != 8080 {
		t.Errorf("custom HTTP port: expected 8080, got %d", customO.Custom.HTTPPort)
	}
	if customO.Custom.ProtocolPolicy != "http-only" {
		t.Errorf("custom protocol: expected 'http-only', got %q", customO.Custom.ProtocolPolicy)
	}

	// Default behavior uses managed CachingOptimized
	if d.DefaultBehavior == nil {
		t.Fatal("DefaultBehavior is nil")
	}
	cp := d.DefaultBehavior.CachePolicy
	if cp == nil {
		t.Fatal("DefaultBehavior.CachePolicy is nil")
	}
	if cp.Name != "Managed-CachingOptimized" {
		t.Errorf("default cache policy: expected 'Managed-CachingOptimized', got %q", cp.Name)
	}
	if !cp.Gzip {
		t.Error("CachingOptimized should have Gzip=true")
	}
	if !cp.Brotli {
		t.Error("CachingOptimized should have Brotli=true")
	}

	// /api/* behavior
	if len(d.Behaviors) != 1 {
		t.Fatalf("expected 1 cache behavior, got %d", len(d.Behaviors))
	}
	bh := d.Behaviors[0]
	if bh.PathPattern != "/api/*" {
		t.Errorf("path pattern: expected '/api/*', got %q", bh.PathPattern)
	}
	if bh.CachePolicy == nil || bh.CachePolicy.Name != "my-template-cp" {
		t.Errorf("/api/* cache policy: expected 'my-template-cp', got %v", bh.CachePolicy)
	}
	if bh.OriginRequestPolicy == nil || bh.OriginRequestPolicy.Name != "Managed-AllViewer" {
		t.Errorf("/api/* origin request policy: expected 'Managed-AllViewer', got %v", bh.OriginRequestPolicy)
	}
	if bh.OriginRequestPolicy != nil && bh.OriginRequestPolicy.Headers.Behavior != "allViewer" {
		t.Errorf("AllViewer headers behavior: expected 'allViewer', got %q", bh.OriginRequestPolicy.Headers.Behavior)
	}

	// Function
	if len(cfg.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(cfg.Functions))
	}
	fn := cfg.Functions[0]
	if fn.Name != "my-function" {
		t.Errorf("function name: expected 'my-function', got %q", fn.Name)
	}
	if fn.Runtime != "cloudfront-js-2.0" {
		t.Errorf("function runtime: expected 'cloudfront-js-2.0', got %q", fn.Runtime)
	}

	// Function has KVS
	if len(fn.KeyValueStores) != 1 {
		t.Fatalf("expected function to have 1 KVS, got %d", len(fn.KeyValueStores))
	}
	kvs := fn.KeyValueStores[0]
	// ARN matches arn:aws:cloudfront::123456789012:key-value-store/<uuid>
	kvsARNPattern := regexp.MustCompile(`^arn:aws:cloudfront::123456789012:key-value-store/[0-9a-f-]{36}$`)
	if !kvsARNPattern.MatchString(kvs.ARN) {
		t.Errorf("KVS ARN %q does not match expected pattern", kvs.ARN)
	}

	// Function viewer-request association
	if bh.ViewerRequest == nil {
		t.Error("expected viewer-request function association")
	} else if bh.ViewerRequest.Name != "my-function" {
		t.Errorf("viewer-request function: expected 'my-function', got %q", bh.ViewerRequest.Name)
	}

	// TrustedKeyGroups
	if len(bh.TrustedKeyGroups) != 1 {
		t.Fatalf("expected 1 trusted key group, got %d", len(bh.TrustedKeyGroups))
	}
	kg := bh.TrustedKeyGroups[0]
	if kg.Name != "my-key-group" {
		t.Errorf("key group name: expected 'my-key-group', got %q", kg.Name)
	}
	if len(kg.Keys) != 1 || kg.Keys[0].Name != "my-public-key" {
		t.Errorf("key group keys: expected [my-public-key], got %v", kg.Keys)
	}
}

// ── C2) Determinism ───────────────────────────────────────────────────────────

func TestDeterminism(t *testing.T) {
	sources := []cfntmpl.Source{src("happy.yaml", happyPathTemplate)}

	cfg1 := mustLoad(t, sources, nil)
	cfg2 := mustLoad(t, sources, nil)

	if len(cfg1.Distributions) == 0 || len(cfg2.Distributions) == 0 {
		t.Fatal("no distributions")
	}
	if cfg1.Distributions[0].ID != cfg2.Distributions[0].ID {
		t.Errorf("distribution IDs differ: %q vs %q",
			cfg1.Distributions[0].ID, cfg2.Distributions[0].ID)
	}
	if len(cfg1.Functions) == 0 || len(cfg2.Functions) == 0 {
		t.Fatal("no functions")
	}
	if cfg1.Functions[0].ARN != cfg2.Functions[0].ARN {
		t.Errorf("function ARNs differ: %q vs %q",
			cfg1.Functions[0].ARN, cfg2.Functions[0].ARN)
	}
	if len(cfg1.KeyValueStores) == 0 || len(cfg2.KeyValueStores) == 0 {
		t.Fatal("no KVS")
	}
	if cfg1.KeyValueStores[0].ID != cfg2.KeyValueStores[0].ID {
		t.Errorf("KVS IDs differ: %q vs %q",
			cfg1.KeyValueStores[0].ID, cfg2.KeyValueStores[0].ID)
	}
}

func TestDifferentLogicalIDsYieldDifferentIDs(t *testing.T) {
	tmplA := `
Resources:
  DistA:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: bucket.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	tmplB := `
Resources:
  DistB:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: bucket.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	cfgA := mustLoad(t, []cfntmpl.Source{src("a.yaml", tmplA)}, nil)
	cfgB := mustLoad(t, []cfntmpl.Source{src("b.yaml", tmplB)}, nil)

	if cfgA.Distributions[0].ID == cfgB.Distributions[0].ID {
		t.Errorf("different logical IDs produced the same distribution ID: %q",
			cfgA.Distributions[0].ID)
	}
}

// ── C3) Managed policies ──────────────────────────────────────────────────────

func TestManagedCachingDisabled(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 4135ea2d-6df8-44a3-9df3-4b5a84be39ad
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if len(cfg.Distributions) == 0 {
		t.Fatal("no distributions")
	}
	cp := cfg.Distributions[0].DefaultBehavior.CachePolicy
	if cp == nil || cp.Name != "Managed-CachingDisabled" {
		t.Errorf("expected 'Managed-CachingDisabled', got %v", cp)
	}
}

func TestUnknownCachePolicyUUID(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
`
	_, err := Load([]cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if err == nil {
		t.Fatal("expected error for unknown cache policy UUID")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error should contain 'neither', got: %v", err)
	}
}

// ── C4) Legacy ForwardedValues ────────────────────────────────────────────────

const forwardedValuesTemplate = `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          ForwardedValues:
            QueryString: true
            Headers:
              - Origin
            Cookies:
              Forward: whitelist
              WhitelistedNames:
                - session
          MinTTL: 0
          DefaultTTL: 60
          MaxTTL: 3600
`

func TestLegacyForwardedValues(t *testing.T) {
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", forwardedValuesTemplate)}, nil)
	if len(cfg.Distributions) == 0 {
		t.Fatal("no distributions")
	}
	cp := cfg.Distributions[0].DefaultBehavior.CachePolicy
	if cp == nil {
		t.Fatal("CachePolicy is nil")
	}

	if cp.QueryStrings.Behavior != "all" {
		t.Errorf("QueryStrings: expected 'all', got %q", cp.QueryStrings.Behavior)
	}
	if cp.Headers.Behavior != "whitelist" {
		t.Errorf("Headers: expected 'whitelist', got %q", cp.Headers.Behavior)
	}
	if !cp.Headers.Contains("Origin") {
		t.Errorf("Headers should contain 'Origin', got %v", cp.Headers.Items)
	}
	if cp.Cookies.Behavior != "whitelist" {
		t.Errorf("Cookies: expected 'whitelist', got %q", cp.Cookies.Behavior)
	}
	if !cp.Cookies.Contains("session") {
		t.Errorf("Cookies should contain 'session', got %v", cp.Cookies.Items)
	}
	if cp.DefaultTTL != 60*time.Second {
		t.Errorf("DefaultTTL: expected 60s, got %v", cp.DefaultTTL)
	}
	if cp.MaxTTL != 3600*time.Second {
		t.Errorf("MaxTTL: expected 3600s, got %v", cp.MaxTTL)
	}
}

func TestForwardedValuesHeadersWildcard(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          ForwardedValues:
            QueryString: false
            Headers:
              - "*"
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	cp := cfg.Distributions[0].DefaultBehavior.CachePolicy
	if cp.Headers.Behavior != "all" {
		t.Errorf("Headers wildcard: expected 'all', got %q", cp.Headers.Behavior)
	}
}

func TestCachePolicyAndForwardedValuesBothError(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
          ForwardedValues:
            QueryString: false
`
	_, err := Load([]cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if err == nil {
		t.Fatal("expected error when both CachePolicyId and ForwardedValues are set")
	}
}

// ── C5) Unsupported features ──────────────────────────────────────────────────

func TestUnsupportedFeatureTable(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name: "OriginGroups",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        OriginGroups:
          Quantity: 1
          Items:
            - {}
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "origin group",
		},
		{
			name: "LambdaEdge",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
          LambdaFunctionAssociations:
            - LambdaFunctionARN: arn:aws:lambda:us-east-1:123456789012:function:F:1
              EventType: viewer-request
`,
			want: "Lambda@Edge",
		},
		{
			name: "FieldLevelEncryption",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
          FieldLevelEncryptionId: abc123
`,
			want: "field-level encryption",
		},
		{
			name: "RealtimeLogs",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
          RealtimeLogConfigArn: arn:aws:cloudfront::123456789012:realtime-log-config/myconfig
`,
			want: "real-time logs",
		},
		{
			name: "TrustedSigners",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
          TrustedSigners:
            - self
`,
			want: "TrustedSigners",
		},
		{
			name: "ContinuousDeployment",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        ContinuousDeploymentPolicyId: some-policy-id
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "continuous deployment",
		},
		{
			name: "NoCachePolicyOrForwardedValues",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
`,
			want: "CachePolicyId",
		},
		{
			name: "TargetOriginIdMismatch",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: real-origin
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: wrong-origin
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "wrong-origin",
		},
		{
			name: "S3WebsiteEndpoint",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: mybucket.s3-website-us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "website",
		},
		{
			name: "OriginWithNeitherConfig",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: somehost.example.com
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "S3OriginConfig",
		},
		{
			name: "CustomOriginMissingProtocolPolicy",
			template: `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: api.internal
            CustomOriginConfig:
              HTTPPort: 80
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`,
			want: "OriginProtocolPolicy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]cfntmpl.Source{src("t.yaml", tc.template)}, nil)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should contain %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestFnImportValueUnsupportedError(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName:
              Fn::ImportValue: some-export
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	_, err := Load([]cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if err == nil {
		t.Fatal("expected error for Fn::ImportValue")
	}
	var uie *cfntmpl.UnsupportedIntrinsicError
	if !errors.As(err, &uie) {
		t.Errorf("expected *UnsupportedIntrinsicError, got %T: %v", err, err)
	} else if uie.Name != "Fn::ImportValue" {
		t.Errorf("expected Name='Fn::ImportValue', got %q", uie.Name)
	}
}

// ── C6) CDK-style fixture ──────────────────────────────────────────────────────

func TestCDKFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/cdk-app.template.json")
	if err != nil {
		t.Fatalf("reading cdk fixture: %v", err)
	}
	cfg, err := Load([]cfntmpl.Source{{Name: "cdk-app.template.json", Data: data}}, nil)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Check warnings mention skipped types
	skippedTypes := []string{"AWS::S3::Bucket", "AWS::S3::BucketPolicy", "AWS::CDK::Metadata"}
	for _, typ := range skippedTypes {
		found := false
		for _, w := range cfg.Warnings {
			if strings.Contains(w, typ) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected warning about skipped type %q, warnings: %v", typ, cfg.Warnings)
		}
	}

	// CDKMetadataAvailable is a Condition so it should cause an error if included,
	// but since CDK::Metadata is not a supported type, it is excluded from resolution.
	// The OAC IS a supported type — check it doesn't error.

	// One distribution
	if len(cfg.Distributions) != 1 {
		t.Fatalf("expected 1 distribution, got %d: %v", len(cfg.Distributions), cfg.Distributions)
	}
	d := cfg.Distributions[0]
	if len(d.Origins) != 1 {
		t.Fatalf("expected 1 origin, got %d", len(d.Origins))
	}
	origin := d.Origins[0]
	if origin.S3 == nil {
		t.Fatal("expected S3 origin")
	}

	// The bucket logical ID is "AssetsBucket", sanitized → "assetsbucket"
	wantBucket := sanitizeBucketName("AssetsBucket")
	if origin.S3.Bucket != wantBucket {
		t.Errorf("S3 bucket: expected %q, got %q", wantBucket, origin.S3.Bucket)
	}
	if origin.S3.Region != cfntmpl.DefaultRegion {
		t.Errorf("S3 region: expected %q, got %q", cfntmpl.DefaultRegion, origin.S3.Region)
	}
}

// ── C7) Alias conflict ────────────────────────────────────────────────────────

func TestAliasConflict(t *testing.T) {
	tmplStr := `
Resources:
  D1:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases:
          - shared.example.test
        Origins:
          - Id: o
            DomainName: b1.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
  D2:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Aliases:
          - shared.example.test
        Origins:
          - Id: o
            DomainName: b2.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	_, err := Load([]cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if err == nil {
		t.Fatal("expected error for alias conflict")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Errorf("error should contain 'already used', got: %v", err)
	}
}

// ── C8) parseS3Domain table ───────────────────────────────────────────────────

func TestParseS3Domain(t *testing.T) {
	tests := []struct {
		domain     string
		wantBucket string
		wantRegion string
		wantOK     bool
	}{
		{"assets.s3.us-east-1.amazonaws.com", "assets", "us-east-1", true},
		{"assets.s3.amazonaws.com", "assets", "us-east-1", true},
		{"assets.s3-eu-west-1.amazonaws.com", "assets", "eu-west-1", true},
		{"assets.s3.dualstack.ap-northeast-1.amazonaws.com", "assets", "ap-northeast-1", true},
		{"my.dotted.bucket.s3.us-east-1.amazonaws.com", "my.dotted.bucket", "us-east-1", true},
		{"example.com", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			bucket, region, ok := parseS3Domain(tc.domain)
			if ok != tc.wantOK {
				t.Errorf("ok: expected %v, got %v", tc.wantOK, ok)
			}
			if ok {
				if bucket != tc.wantBucket {
					t.Errorf("bucket: expected %q, got %q", tc.wantBucket, bucket)
				}
				if region != tc.wantRegion {
					t.Errorf("region: expected %q, got %q", tc.wantRegion, region)
				}
			}
		})
	}
}

// ── C9) Distribution Enabled false ───────────────────────────────────────────

func TestDistributionEnabledFalse(t *testing.T) {
	tmplStr := `
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: false
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if len(cfg.Distributions) == 0 {
		t.Fatal("no distributions")
	}
	if cfg.Distributions[0].Enabled {
		t.Error("expected Enabled=false")
	}
	// Warnings should mention disabled
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(strings.ToLower(w), "disabled") || strings.Contains(strings.ToLower(w), "enabled") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about disabled distribution, warnings: %v", cfg.Warnings)
	}
}

// ── C10) KVS ImportSource ─────────────────────────────────────────────────────

func TestKVSImportSourceS3(t *testing.T) {
	tmplStr := `
Resources:
  MyKVS:
    Type: AWS::CloudFront::KeyValueStore
    Properties:
      Name: my-kvs
      ImportSource:
        SourceType: S3
        SourceArn: arn:aws:s3:::my-bucket/kvs-data.json
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if len(cfg.KeyValueStores) == 0 {
		t.Fatal("no KVS")
	}
	kvs := cfg.KeyValueStores[0]
	if kvs.ImportSourceARN != "arn:aws:s3:::my-bucket/kvs-data.json" {
		t.Errorf("ImportSourceARN: expected arn, got %q", kvs.ImportSourceARN)
	}
}

func TestKVSImportSourceNonS3Error(t *testing.T) {
	tmplStr := `
Resources:
  MyKVS:
    Type: AWS::CloudFront::KeyValueStore
    Properties:
      Name: my-kvs
      ImportSource:
        SourceType: DynamoDB
        SourceArn: arn:aws:dynamodb:us-east-1:123456789012:table/mytable
`
	_, err := Load([]cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	if err == nil {
		t.Fatal("expected error for non-S3 ImportSource")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "s3") {
		t.Errorf("error should mention S3, got: %v", err)
	}
}

// ── Additional edge cases ──────────────────────────────────────────────────────

func TestLoadWithParameters(t *testing.T) {
	tmplStr := `
Parameters:
  BucketName:
    Type: String
    Default: default-bucket
Resources:
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName:
              Fn::Sub: "${BucketName}.s3.us-east-1.amazonaws.com"
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, map[string]string{"BucketName": "custom-bucket"})
	if len(cfg.Distributions) == 0 {
		t.Fatal("no distributions")
	}
	if cfg.Distributions[0].Origins[0].S3.Bucket != "custom-bucket" {
		t.Errorf("expected 'custom-bucket', got %q", cfg.Distributions[0].Origins[0].S3.Bucket)
	}
}

func TestUnsupportedResourceTypeSkippedWithWarning(t *testing.T) {
	tmplStr := `
Resources:
  MyBucket:
    Type: AWS::S3::Bucket
  D:
    Type: AWS::CloudFront::Distribution
    Properties:
      DistributionConfig:
        Enabled: true
        Origins:
          - Id: o
            DomainName: b.s3.us-east-1.amazonaws.com
            S3OriginConfig: {}
        DefaultCacheBehavior:
          TargetOriginId: o
          ViewerProtocolPolicy: allow-all
          CachePolicyId: 658327ea-f89d-4fab-a63d-7e88639e58f6
`
	cfg := mustLoad(t, []cfntmpl.Source{src("t.yaml", tmplStr)}, nil)
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "AWS::S3::Bucket") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about AWS::S3::Bucket skip, warnings: %v", cfg.Warnings)
	}
}

// ── ID generation sanity ─────────────────────────────────────────────────────

func TestDistributionIDFormat(t *testing.T) {
	id := distributionID("MyDistribution")
	if !distributionIDPattern.MatchString(id) {
		t.Errorf("distributionID %q does not match ^E[A-Z0-9]{13}$", id)
	}
}

func TestUUIDIDFormat(t *testing.T) {
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	id := uuidID("cache-policy", "MyPolicy")
	if !uuidPattern.MatchString(id) {
		t.Errorf("uuidID %q does not match UUID format", id)
	}
}
