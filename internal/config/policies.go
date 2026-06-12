package config

import (
	"fmt"
	"time"
)

func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

func cachePolicyFromProps(id string, p *cachePolicyConfigProps) (*CachePolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("CachePolicyConfig is required")
	}
	cp := &CachePolicy{
		ID:           id,
		Name:         p.Name,
		MinTTL:       secondsToDuration(p.MinTTL.Value(0)),
		DefaultTTL:   secondsToDuration(p.DefaultTTL.Value(86400)),
		MaxTTL:       secondsToDuration(p.MaxTTL.Value(31536000)),
		Headers:      ListSelection{Behavior: "none"},
		Cookies:      ListSelection{Behavior: "none"},
		QueryStrings: ListSelection{Behavior: "none"},
	}
	if k := p.ParametersInCacheKeyAndForwardedToOrigin; k != nil {
		cp.Gzip = k.EnableAcceptEncodingGzip.Value(false)
		cp.Brotli = k.EnableAcceptEncodingBrotli.Value(false)
		if k.HeadersConfig != nil {
			cp.Headers = ListSelection{Behavior: defaultBehavior(k.HeadersConfig.HeaderBehavior, "none"), Items: k.HeadersConfig.Headers}
		}
		if k.CookiesConfig != nil {
			cp.Cookies = ListSelection{Behavior: defaultBehavior(k.CookiesConfig.CookieBehavior, "none"), Items: k.CookiesConfig.Cookies}
		}
		if k.QueryStringsConfig != nil {
			cp.QueryStrings = ListSelection{Behavior: defaultBehavior(k.QueryStringsConfig.QueryStringBehavior, "none"), Items: k.QueryStringsConfig.QueryStrings}
		}
	}
	return cp, nil
}

func originRequestPolicyFromProps(id string, p *originRequestPolicyConfigProps) (*OriginRequestPolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("OriginRequestPolicyConfig is required")
	}
	orp := &OriginRequestPolicy{
		ID:           id,
		Name:         p.Name,
		Headers:      ListSelection{Behavior: "none"},
		Cookies:      ListSelection{Behavior: "none"},
		QueryStrings: ListSelection{Behavior: "none"},
	}
	if p.HeadersConfig != nil {
		orp.Headers = ListSelection{Behavior: defaultBehavior(p.HeadersConfig.HeaderBehavior, "none"), Items: p.HeadersConfig.Headers}
	}
	if p.CookiesConfig != nil {
		orp.Cookies = ListSelection{Behavior: defaultBehavior(p.CookiesConfig.CookieBehavior, "none"), Items: p.CookiesConfig.Cookies}
	}
	if p.QueryStringsConfig != nil {
		orp.QueryStrings = ListSelection{Behavior: defaultBehavior(p.QueryStringsConfig.QueryStringBehavior, "none"), Items: p.QueryStringsConfig.QueryStrings}
	}
	return orp, nil
}

func responseHeadersPolicyFromProps(id string, p *responseHeadersPolicyConfigProps) (*ResponseHeadersPolicy, error) {
	if p == nil {
		return nil, fmt.Errorf("ResponseHeadersPolicyConfig is required")
	}
	rhp := &ResponseHeadersPolicy{ID: id, Name: p.Name}
	if c := p.CorsConfig; c != nil {
		rhp.Cors = &CorsConfig{
			AllowCredentials: c.AccessControlAllowCredentials.Value(false),
			AllowHeaders:     c.AccessControlAllowHeaders,
			AllowMethods:     c.AccessControlAllowMethods,
			AllowOrigins:     c.AccessControlAllowOrigins,
			ExposeHeaders:    c.AccessControlExposeHeaders,
			OriginOverride:   c.OriginOverride.Value(false),
		}
		if c.AccessControlMaxAgeSec != nil {
			rhp.Cors.MaxAgeSec = c.AccessControlMaxAgeSec.Value(0)
			rhp.Cors.HasMaxAge = true
		}
	}
	if c := p.CustomHeadersConfig; c != nil {
		for _, h := range c.Items {
			rhp.CustomHeaders = append(rhp.CustomHeaders, CustomHeader{
				Name:     h.Header,
				Value:    h.Value,
				Override: h.Override.Value(false),
			})
		}
	}
	if c := p.RemoveHeadersConfig; c != nil {
		for _, h := range c.Items {
			rhp.RemoveHeaders = append(rhp.RemoveHeaders, h.Header)
		}
	}
	if c := p.SecurityHeadersConfig; c != nil {
		sec := &SecurityHeaders{}
		if v := c.ContentSecurityPolicy; v != nil {
			sec.ContentSecurityPolicy = &HeaderValue{Value: v.ContentSecurityPolicy, Override: v.Override.Value(false)}
		}
		if v := c.ContentTypeOptions; v != nil {
			sec.ContentTypeOptions = &HeaderToggle{Override: v.Override.Value(false)}
		}
		if v := c.FrameOptions; v != nil {
			sec.FrameOptions = &HeaderValue{Value: v.FrameOption, Override: v.Override.Value(false)}
		}
		if v := c.ReferrerPolicy; v != nil {
			sec.ReferrerPolicy = &HeaderValue{Value: v.ReferrerPolicy, Override: v.Override.Value(false)}
		}
		if v := c.StrictTransportSecurity; v != nil {
			sec.StrictTransportSecurity = &HSTS{
				MaxAgeSec:         v.AccessControlMaxAgeSec.Value(0),
				IncludeSubdomains: v.IncludeSubdomains.Value(false),
				Preload:           v.Preload.Value(false),
				Override:          v.Override.Value(false),
			}
		}
		if v := c.XSSProtection; v != nil {
			sec.XSSProtection = &XSSProtection{
				Protection: v.Protection.Value(false),
				ModeBlock:  v.ModeBlock.Value(false),
				ReportURI:  v.ReportUri,
				Override:   v.Override.Value(false),
			}
		}
		rhp.Security = sec
	}
	return rhp, nil
}

// cachePolicyFromForwardedValues converts a legacy ForwardedValues block (plus
// the behavior-level TTLs that accompany it) into an equivalent CachePolicy.
func cachePolicyFromForwardedValues(name string, b *behaviorProps) *CachePolicy {
	fv := b.ForwardedValues
	cp := &CachePolicy{
		Name:         name,
		MinTTL:       secondsToDuration(b.MinTTL.Value(0)),
		DefaultTTL:   secondsToDuration(b.DefaultTTL.Value(86400)),
		MaxTTL:       secondsToDuration(b.MaxTTL.Value(31536000)),
		Headers:      ListSelection{Behavior: "none"},
		Cookies:      ListSelection{Behavior: "none"},
		QueryStrings: ListSelection{Behavior: "none"},
	}
	if len(fv.Headers) > 0 {
		all := false
		for _, h := range fv.Headers {
			if h == "*" {
				all = true
			}
		}
		if all {
			cp.Headers = ListSelection{Behavior: "all"}
		} else {
			cp.Headers = ListSelection{Behavior: "whitelist", Items: fv.Headers}
		}
	}
	if fv.Cookies != nil {
		switch fv.Cookies.Forward {
		case "all":
			cp.Cookies = ListSelection{Behavior: "all"}
		case "whitelist":
			cp.Cookies = ListSelection{Behavior: "whitelist", Items: fv.Cookies.WhitelistedNames}
		}
	}
	if fv.QueryString.Value(false) {
		if len(fv.QueryStringCacheKeys) > 0 {
			cp.QueryStrings = ListSelection{Behavior: "whitelist", Items: fv.QueryStringCacheKeys}
		} else {
			cp.QueryStrings = ListSelection{Behavior: "all"}
		}
	}
	return cp
}

func defaultBehavior(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
