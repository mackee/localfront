package cfntmpl

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// shortFormTags maps CloudFormation YAML short-form tags to their long-form
// key. Every short form is normalized so the resolver only ever sees the long
// form; whether the function is actually supported is decided at resolve time.
var shortFormTags = map[string]string{
	"!Ref":         "Ref",
	"!GetAtt":      "Fn::GetAtt",
	"!Sub":         "Fn::Sub",
	"!Join":        "Fn::Join",
	"!FindInMap":   "Fn::FindInMap",
	"!Select":      "Fn::Select",
	"!Split":       "Fn::Split",
	"!ImportValue": "Fn::ImportValue",
	"!GetAZs":      "Fn::GetAZs",
	"!Base64":      "Fn::Base64",
	"!Cidr":        "Fn::Cidr",
	"!If":          "Fn::If",
	"!And":         "Fn::And",
	"!Or":          "Fn::Or",
	"!Not":         "Fn::Not",
	"!Equals":      "Fn::Equals",
	"!Condition":   "Condition",
}

// nodeToAny converts a YAML node tree into plain Go values
// (map[string]any / []any / scalars), turning CloudFormation short-form tags
// into their long-form single-key maps, e.g. !Ref X => map["Ref"]"X".
// JSON input never contains short forms, so this is a no-op for it.
func nodeToAny(n *yaml.Node) (any, error) {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil, nil
		}
		return nodeToAny(n.Content[0])
	}
	if n.Kind == yaml.AliasNode {
		return nodeToAny(n.Alias)
	}
	if long, ok := shortFormTags[n.Tag]; ok {
		inner, err := taggedContentToAny(n)
		if err != nil {
			return nil, err
		}
		return map[string]any{long: inner}, nil
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		return nil, fmt.Errorf("line %d: unsupported YAML tag %q", n.Line, n.Tag)
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			var key string
			if err := keyNode.Decode(&key); err != nil {
				return nil, fmt.Errorf("line %d: mapping key must be a scalar: %w", keyNode.Line, err)
			}
			v, err := nodeToAny(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		return m, nil
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeToAny(c)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
		}
		return s, nil
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node kind %d", n.Line, n.Kind)
	}
}

// taggedContentToAny decodes the content of a short-form-tagged node as if it
// had no tag: `!Ref Bucket` is the scalar "Bucket", `!Join [",", [...]]` is a
// sequence, `!Sub ["tpl", {k: v}]` keeps both elements.
func taggedContentToAny(n *yaml.Node) (any, error) {
	clone := *n
	clone.Tag = "" // let yaml.v3 re-resolve the implicit tag from the value
	switch n.Kind {
	case yaml.ScalarNode:
		var v any
		if err := clone.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return v, nil
	case yaml.SequenceNode, yaml.MappingNode:
		clone.Tag = defaultTagFor(n.Kind)
		return nodeToAny(&clone)
	default:
		return nil, fmt.Errorf("line %d: unsupported node kind for tag %q", n.Line, n.Tag)
	}
}

func defaultTagFor(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "!!seq"
	case yaml.MappingNode:
		return "!!map"
	default:
		return ""
	}
}
