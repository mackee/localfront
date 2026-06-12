package cfntmpl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DefaultRegion is the value of the AWS::Region pseudo parameter and the
// region assumed for S3 bucket domain names derived from references.
const DefaultRegion = "us-east-1"

// AccountID is the dummy account ID used in generated ARNs and the
// AWS::AccountId pseudo parameter.
const AccountID = "123456789012"

// UnresolvedPrefix marks placeholder strings substituted for references that
// could not be resolved (e.g. a Ref to a resource type localfront does not
// know). Callers must reject these values wherever they would actually be
// used; in ignored properties (certificate ARNs, WAF ACLs, ...) they are
// harmless.
const UnresolvedPrefix = "localfront:unresolved:"

// IsUnresolved reports whether v is a placeholder produced for an
// unresolvable reference, or a string containing one (e.g. via Fn::Sub).
func IsUnresolved(v any) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, UnresolvedPrefix)
}

// RefValuer supplies the values of Ref and Fn::GetAtt for resources.
// Returning false means the reference cannot be resolved; the resolver then
// substitutes a placeholder and records a warning.
type RefValuer interface {
	RefValue(logicalID string) (string, bool)
	AttValue(logicalID, attr string) (string, bool)
}

// ResolveOptions controls intrinsic resolution.
type ResolveOptions struct {
	// Parameters overrides template parameter values (--parameter key=value).
	Parameters map[string]string
	// Refs resolves Ref / Fn::GetAtt between resources. Required.
	Refs RefValuer
	// Include selects which resources are resolved. Resources excluded here
	// are skipped entirely (their properties may contain intrinsics localfront
	// cannot resolve). Nil means all resources.
	Include func(r *RawResource) bool
}

// Resource is a resource with fully resolved properties.
type Resource struct {
	LogicalID  string
	Type       string
	Properties map[string]any
	Source     string
}

// Template is the result of resolving a Parsed template set.
type Template struct {
	Resources []*Resource
	Warnings  []string
}

var pseudoParameters = map[string]string{
	"AWS::AccountId": AccountID,
	"AWS::Partition": "aws",
	"AWS::Region":    DefaultRegion,
	"AWS::URLSuffix": "amazonaws.com",
	"AWS::StackName": "localfront",
	"AWS::StackId": "arn:aws:cloudformation:" + DefaultRegion + ":" + AccountID +
		":stack/localfront/00000000-0000-0000-0000-000000000000",
}

// noValue is the sentinel produced by Ref AWS::NoValue; containers drop it.
type noValueType struct{}

var noValue = noValueType{}

// UnsupportedIntrinsicError is returned when a template uses an intrinsic
// function outside the supported subset.
type UnsupportedIntrinsicError struct {
	Name string
	Path string
}

func (e *UnsupportedIntrinsicError) Error() string {
	return fmt.Sprintf("%s: intrinsic function %s is not supported by localfront", e.Path, e.Name)
}

// Resolve evaluates all intrinsic functions in the selected resources.
func (p *Parsed) Resolve(opts ResolveOptions) (*Template, error) {
	if opts.Refs == nil {
		return nil, fmt.Errorf("cfntmpl: ResolveOptions.Refs is required")
	}
	for name := range opts.Parameters {
		if _, ok := p.parameters[name]; !ok {
			return nil, fmt.Errorf("--parameter %s: no such parameter in the loaded templates", name)
		}
	}
	r := &resolver{parsed: p, opts: opts, warned: map[string]bool{}}
	out := &Template{}
	for _, res := range p.Resources() {
		if opts.Include != nil && !opts.Include(res) {
			continue
		}
		if res.Condition != "" {
			return nil, fmt.Errorf("resource %s: Conditions are not supported by localfront", res.LogicalID)
		}
		path := "Resources/" + res.LogicalID + "/Properties"
		props := map[string]any{}
		if res.Properties != nil {
			v, err := r.value(res.Properties, path)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", res.Source, err)
			}
			if m, ok := v.(map[string]any); ok {
				props = m
			} else if _, isNoValue := v.(noValueType); !isNoValue {
				return nil, fmt.Errorf("%s: %s: Properties must be a mapping", res.Source, path)
			}
		}
		out.Resources = append(out.Resources, &Resource{
			LogicalID:  res.LogicalID,
			Type:       res.Type,
			Properties: props,
			Source:     res.Source,
		})
	}
	out.Warnings = r.warnings
	return out, nil
}

type resolver struct {
	parsed   *Parsed
	opts     ResolveOptions
	warnings []string
	warned   map[string]bool
}

func (r *resolver) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !r.warned[msg] {
		r.warned[msg] = true
		r.warnings = append(r.warnings, msg)
	}
}

func (r *resolver) value(v any, path string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			for key, arg := range t {
				if isIntrinsicKey(key, arg) {
					return r.intrinsic(key, arg, path)
				}
			}
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := r.value(val, path+"/"+k)
			if err != nil {
				return nil, err
			}
			if _, drop := rv.(noValueType); drop {
				continue
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for i, val := range t {
			rv, err := r.value(val, fmt.Sprintf("%s/%d", path, i))
			if err != nil {
				return nil, err
			}
			if _, drop := rv.(noValueType); drop {
				continue
			}
			out = append(out, rv)
		}
		return out, nil
	default:
		return v, nil
	}
}

func isIntrinsicKey(key string, arg any) bool {
	if key == "Ref" {
		return true
	}
	if strings.HasPrefix(key, "Fn::") {
		return true
	}
	if key == "Condition" {
		_, isString := arg.(string)
		return isString
	}
	return false
}

func (r *resolver) intrinsic(name string, arg any, path string) (any, error) {
	switch name {
	case "Ref":
		return r.ref(arg, path)
	case "Fn::GetAtt":
		return r.getAtt(arg, path)
	case "Fn::Sub":
		return r.sub(arg, path)
	case "Fn::Join":
		return r.join(arg, path)
	case "Fn::FindInMap":
		return r.findInMap(arg, path)
	default:
		return nil, &UnsupportedIntrinsicError{Name: name, Path: path}
	}
}

func (r *resolver) ref(arg any, path string) (any, error) {
	name, ok := arg.(string)
	if !ok {
		return nil, fmt.Errorf("%s: Ref expects a string, got %T", path, arg)
	}
	if name == "AWS::NoValue" {
		return noValue, nil
	}
	if v, ok := pseudoParameters[name]; ok {
		return v, nil
	}
	if def, ok := r.parsed.parameters[name]; ok {
		return r.parameterValue(name, def, path)
	}
	if v, ok := r.opts.Refs.RefValue(name); ok {
		return v, nil
	}
	return r.placeholder(name, "", path), nil
}

func (r *resolver) getAtt(arg any, path string) (any, error) {
	var logicalID, attr string
	switch t := arg.(type) {
	case string:
		var ok bool
		logicalID, attr, ok = strings.Cut(t, ".")
		if !ok {
			return nil, fmt.Errorf("%s: Fn::GetAtt %q must be of the form LogicalID.Attribute", path, t)
		}
	case []any:
		if len(t) < 2 {
			return nil, fmt.Errorf("%s: Fn::GetAtt expects [logical ID, attribute]", path)
		}
		parts := make([]string, 0, len(t))
		for i, p := range t {
			rv, err := r.value(p, fmt.Sprintf("%s/%d", path, i))
			if err != nil {
				return nil, err
			}
			s, ok := rv.(string)
			if !ok {
				return nil, fmt.Errorf("%s: Fn::GetAtt elements must be strings", path)
			}
			parts = append(parts, s)
		}
		logicalID, attr = parts[0], strings.Join(parts[1:], ".")
	default:
		return nil, fmt.Errorf("%s: Fn::GetAtt expects a string or a list, got %T", path, arg)
	}
	if v, ok := r.opts.Refs.AttValue(logicalID, attr); ok {
		return v, nil
	}
	return r.placeholder(logicalID, attr, path), nil
}

func (r *resolver) placeholder(logicalID, attr, path string) string {
	ref := logicalID
	if attr != "" {
		ref += "." + attr
	}
	if res, ok := r.parsed.resources[logicalID]; ok {
		r.warnf("%s: reference to %s (resource type %s) cannot be resolved; substituted a placeholder", path, ref, res.Type)
	} else {
		r.warnf("%s: reference to unknown logical ID %s; substituted a placeholder", path, ref)
	}
	return UnresolvedPrefix + ref
}

func (r *resolver) parameterValue(name string, def *parameterDef, path string) (any, error) {
	raw, ok := r.opts.Parameters[name]
	if !ok {
		if !def.hasDefault {
			return nil, fmt.Errorf("%s: parameter %s has no default; pass --parameter %s=<value>", path, name, name)
		}
		return normalizeParameter(def.typ, def.defaultVal)
	}
	return normalizeParameter(def.typ, raw)
}

// normalizeParameter coerces a parameter value: list-typed parameters become
// []string, everything else a string (CloudFormation treats Number parameters
// as strings wherever they are interpolated).
func normalizeParameter(typ string, v any) (any, error) {
	isList := typ == "CommaDelimitedList" || strings.HasPrefix(typ, "List<")
	if isList {
		switch t := v.(type) {
		case string:
			parts := strings.Split(t, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts, nil
		case []any:
			parts := make([]string, 0, len(t))
			for _, e := range t {
				s, err := stringify(e)
				if err != nil {
					return nil, err
				}
				parts = append(parts, s)
			}
			return parts, nil
		}
	}
	return stringify(v)
}

func stringify(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case uint64:
		return strconv.FormatUint(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("cannot convert %T to a string", v)
	}
}

var subVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

func (r *resolver) sub(arg any, path string) (any, error) {
	var tpl string
	locals := map[string]any{}
	switch t := arg.(type) {
	case string:
		tpl = t
	case []any:
		if len(t) != 2 {
			return nil, fmt.Errorf("%s: Fn::Sub expects a string or [template, variables]", path)
		}
		s, ok := t[0].(string)
		if !ok {
			return nil, fmt.Errorf("%s: Fn::Sub template must be a string", path)
		}
		tpl = s
		vars, ok := t[1].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: Fn::Sub variables must be a mapping", path)
		}
		for k, v := range vars {
			rv, err := r.value(v, path+"/"+k)
			if err != nil {
				return nil, err
			}
			locals[k] = rv
		}
	default:
		return nil, fmt.Errorf("%s: Fn::Sub expects a string or a list, got %T", path, arg)
	}

	var firstErr error
	out := subVarPattern.ReplaceAllStringFunc(tpl, func(match string) string {
		name := match[2 : len(match)-1]
		if strings.HasPrefix(name, "!") {
			return "${" + name[1:] + "}"
		}
		v, err := r.subVariable(name, locals, path)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return v
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (r *resolver) subVariable(name string, locals map[string]any, path string) (string, error) {
	if v, ok := locals[name]; ok {
		s, err := stringify(v)
		if err != nil {
			return "", fmt.Errorf("%s: Fn::Sub variable %s: %w", path, name, err)
		}
		return s, nil
	}
	var v any
	var err error
	if strings.Contains(name, ".") && !strings.HasPrefix(name, "AWS::") {
		v, err = r.getAtt(name, path)
	} else {
		v, err = r.ref(name, path)
	}
	if err != nil {
		return "", err
	}
	s, err := stringify(v)
	if err != nil {
		return "", fmt.Errorf("%s: Fn::Sub variable %s: %w", path, name, err)
	}
	return s, nil
}

func (r *resolver) join(arg any, path string) (any, error) {
	args, ok := arg.([]any)
	if !ok || len(args) != 2 {
		return nil, fmt.Errorf("%s: Fn::Join expects [delimiter, list]", path)
	}
	delim, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("%s: Fn::Join delimiter must be a string", path)
	}
	items, err := r.value(args[1], path+"/1")
	if err != nil {
		return nil, err
	}
	var parts []string
	switch t := items.(type) {
	case []any:
		for _, e := range t {
			s, serr := stringify(e)
			if serr != nil {
				return nil, fmt.Errorf("%s: Fn::Join: %w", path, serr)
			}
			parts = append(parts, s)
		}
	case []string:
		parts = t
	default:
		return nil, fmt.Errorf("%s: Fn::Join second argument must be a list", path)
	}
	return strings.Join(parts, delim), nil
}

func (r *resolver) findInMap(arg any, path string) (any, error) {
	args, ok := arg.([]any)
	if !ok || len(args) != 3 {
		return nil, fmt.Errorf("%s: Fn::FindInMap expects [map, top key, second key]", path)
	}
	keys := make([]string, 3)
	for i, a := range args {
		rv, err := r.value(a, fmt.Sprintf("%s/%d", path, i))
		if err != nil {
			return nil, err
		}
		s, ok := rv.(string)
		if !ok {
			return nil, fmt.Errorf("%s: Fn::FindInMap keys must be strings", path)
		}
		keys[i] = s
	}
	mapping, ok := r.parsed.mappings[keys[0]].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: Fn::FindInMap: no mapping named %q", path, keys[0])
	}
	second, ok := mapping[keys[1]].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: Fn::FindInMap: mapping %q has no key %q", path, keys[0], keys[1])
	}
	v, ok := second[keys[2]]
	if !ok {
		return nil, fmt.Errorf("%s: Fn::FindInMap: mapping %q/%q has no key %q", path, keys[0], keys[1], keys[2])
	}
	return v, nil
}
