// Package cfntmpl parses CloudFormation templates (JSON or YAML) and
// resolves the subset of intrinsic functions supported by localfront.
//
// The package is intentionally ignorant of resource semantics: what a Ref or
// Fn::GetAtt evaluates to is delegated to a RefValuer supplied by the caller,
// which also decides which resources are resolved at all.
package cfntmpl

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Source is one template file to load.
type Source struct {
	Name string // file name, used in error messages
	Data []byte
}

// RawResource is a resource as written in a template: its properties still
// contain unresolved intrinsic functions in long form (Ref, Fn::GetAtt, ...).
type RawResource struct {
	LogicalID  string
	Type       string
	Condition  string // logical ID of a Conditions entry, "" if none
	Properties any
	Source     string
}

// parameterDef is the declaration of a template parameter.
type parameterDef struct {
	typ        string
	defaultVal any
	hasDefault bool
	source     string
}

// Parsed is a set of merged templates before intrinsic resolution.
type Parsed struct {
	parameters map[string]*parameterDef
	mappings   map[string]any
	resources  map[string]*RawResource
	order      []string // logical IDs in encounter order
}

// Resources returns all resources of the loaded templates, including types
// the caller may not support, in encounter order.
func (p *Parsed) Resources() []*RawResource {
	out := make([]*RawResource, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, p.resources[id])
	}
	return out
}

// Parse loads and merges one or more template files. Intrinsic functions are
// normalized to their long form but not resolved yet.
func Parse(sources []Source) (*Parsed, error) {
	p := &Parsed{
		parameters: map[string]*parameterDef{},
		mappings:   map[string]any{},
		resources:  map[string]*RawResource{},
	}
	for _, src := range sources {
		if err := p.parseOne(src); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Parsed) parseOne(src Source) error {
	var root yaml.Node
	if err := yaml.Unmarshal(src.Data, &root); err != nil {
		return fmt.Errorf("%s: %w", src.Name, err)
	}
	doc, err := nodeToAny(&root)
	if err != nil {
		return fmt.Errorf("%s: %w", src.Name, err)
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: template must be a mapping at the top level", src.Name)
	}
	if _, ok := top["Transform"]; ok {
		return fmt.Errorf("%s: CloudFormation transforms (e.g. SAM) are not supported", src.Name)
	}
	if err := p.parseParameters(src.Name, top["Parameters"]); err != nil {
		return err
	}
	if err := p.parseMappings(src.Name, top["Mappings"]); err != nil {
		return err
	}
	return p.parseResources(src.Name, top["Resources"])
}

func (p *Parsed) parseParameters(srcName string, v any) error {
	if v == nil {
		return nil
	}
	params, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: Parameters must be a mapping", srcName)
	}
	for name, raw := range params {
		if prev, dup := p.parameters[name]; dup {
			return fmt.Errorf("%s: parameter %q is already defined in %s", srcName, name, prev.source)
		}
		def := &parameterDef{typ: "String", source: srcName}
		body, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: parameter %q must be a mapping", srcName, name)
		}
		if t, ok := body["Type"].(string); ok {
			def.typ = t
		}
		if dv, ok := body["Default"]; ok {
			def.defaultVal = dv
			def.hasDefault = true
		}
		p.parameters[name] = def
	}
	return nil
}

func (p *Parsed) parseMappings(srcName string, v any) error {
	if v == nil {
		return nil
	}
	mappings, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: Mappings must be a mapping", srcName)
	}
	for name, m := range mappings {
		if _, dup := p.mappings[name]; dup {
			return fmt.Errorf("%s: mapping %q is defined in more than one template", srcName, name)
		}
		p.mappings[name] = m
	}
	return nil
}

func (p *Parsed) parseResources(srcName string, v any) error {
	if v == nil {
		return fmt.Errorf("%s: template has no Resources section", srcName)
	}
	resources, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: Resources must be a mapping", srcName)
	}
	// Map iteration order is random; collect IDs sorted within one file so
	// load order (and therefore warnings and IDs in Config) is deterministic.
	for _, id := range sortedKeys(resources) {
		raw := resources[id]
		if prev, dup := p.resources[id]; dup {
			return fmt.Errorf("%s: logical ID %q is already defined in %s", srcName, id, prev.Source)
		}
		body, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: resource %q must be a mapping", srcName, id)
		}
		typ, ok := body["Type"].(string)
		if !ok || typ == "" {
			return fmt.Errorf("%s: resource %q has no Type", srcName, id)
		}
		res := &RawResource{
			LogicalID:  id,
			Type:       typ,
			Properties: body["Properties"],
			Source:     srcName,
		}
		if cond, ok := body["Condition"].(string); ok {
			res.Condition = cond
		}
		p.resources[id] = res
		p.order = append(p.order, id)
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
