package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// AWS managed policies, built in under their well-known IDs. The data file
// mirrors the AWS documentation; names carry the "Managed-" prefix.
//
//go:embed managed_policies.json
var managedPoliciesJSON []byte

type managedPoliciesFile struct {
	CachePolicies []struct {
		Id                string
		CachePolicyConfig *cachePolicyConfigProps
	}
	OriginRequestPolicies []struct {
		Id                        string
		OriginRequestPolicyConfig *originRequestPolicyConfigProps
	}
	ResponseHeadersPolicies []struct {
		Id                          string
		ResponseHeadersPolicyConfig *responseHeadersPolicyConfigProps
	}
}

type managedSet struct {
	cachePolicies           map[string]*CachePolicy
	originRequestPolicies   map[string]*OriginRequestPolicy
	responseHeadersPolicies map[string]*ResponseHeadersPolicy
}

var loadManaged = sync.OnceValues(func() (*managedSet, error) {
	var file managedPoliciesFile
	if err := json.Unmarshal(managedPoliciesJSON, &file); err != nil {
		return nil, fmt.Errorf("managed policies data: %w", err)
	}
	set := &managedSet{
		cachePolicies:           map[string]*CachePolicy{},
		originRequestPolicies:   map[string]*OriginRequestPolicy{},
		responseHeadersPolicies: map[string]*ResponseHeadersPolicy{},
	}
	for _, e := range file.CachePolicies {
		cp, err := cachePolicyFromProps(e.Id, e.CachePolicyConfig)
		if err != nil {
			return nil, fmt.Errorf("managed cache policy %s: %w", e.Id, err)
		}
		set.cachePolicies[e.Id] = cp
	}
	for _, e := range file.OriginRequestPolicies {
		orp, err := originRequestPolicyFromProps(e.Id, e.OriginRequestPolicyConfig)
		if err != nil {
			return nil, fmt.Errorf("managed origin request policy %s: %w", e.Id, err)
		}
		set.originRequestPolicies[e.Id] = orp
	}
	for _, e := range file.ResponseHeadersPolicies {
		rhp, err := responseHeadersPolicyFromProps(e.Id, e.ResponseHeadersPolicyConfig)
		if err != nil {
			return nil, fmt.Errorf("managed response headers policy %s: %w", e.Id, err)
		}
		set.responseHeadersPolicies[e.Id] = rhp
	}
	return set, nil
})
