package cfntmpl

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeRefValuer is a simple RefValuer for tests.
type fakeRefValuer struct {
	refs map[string]string
	atts map[string]string // key: "LogicalID.Attr"
}

func (f *fakeRefValuer) RefValue(logicalID string) (string, bool) {
	v, ok := f.refs[logicalID]
	return v, ok
}

func (f *fakeRefValuer) AttValue(logicalID, attr string) (string, bool) {
	v, ok := f.atts[logicalID+"."+attr]
	return v, ok
}

// makeSource builds a Source from an inline template string.
func makeSource(name, data string) Source {
	return Source{Name: name, Data: []byte(data)}
}

// mustParse calls Parse and fails if it returns an error.
func mustParse(t *testing.T, sources []Source) *Parsed {
	t.Helper()
	p, err := Parse(sources)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return p
}

// resolve is a convenience wrapper around Parsed.Resolve.
func resolve(t *testing.T, p *Parsed, refs RefValuer, params map[string]string) (*Template, error) {
	t.Helper()
	return p.Resolve(ResolveOptions{
		Parameters: params,
		Refs:       refs,
	})
}

// mustResolve calls Resolve and fails on error.
func mustResolve(t *testing.T, p *Parsed, refs RefValuer, params map[string]string) *Template {
	t.Helper()
	tmpl, err := resolve(t, p, refs, params)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	return tmpl
}

// ── A) Parse + YAML/JSON equivalence ─────────────────────────────────────────

const yamlEquivTemplate = `
Parameters:
  Env:
    Type: String
    Default: prod
  Port:
    Type: Number
    Default: 8080
  Tags:
    Type: CommaDelimitedList
    Default: "a, b"

Mappings:
  RegionMap:
    us-east-1:
      AMI: ami-12345678

Resources:
  MyBucket:
    Type: AWS::S3::Bucket
    Properties:
      BucketName: !Ref Env
      Port: !Ref Port
      Tags: !Ref Tags
      Sub1: !Sub "env=${Env}"
      Sub2: !Sub
        - "env=${Env}-${Extra}"
        - Extra: !Ref Env
      Joined: !Join
        - "-"
        - - !Ref Env
          - prod
      AMI: !FindInMap
        - RegionMap
        - us-east-1
        - AMI
      GetAtt1: !GetAtt MyBucket.BucketName
      GetAtt2: !GetAtt
        - MyBucket
        - BucketName
`

const jsonEquivTemplate = `{
  "Parameters": {
    "Env":  {"Type": "String",           "Default": "prod"},
    "Port": {"Type": "Number",           "Default": 8080},
    "Tags": {"Type": "CommaDelimitedList","Default": "a, b"}
  },
  "Mappings": {
    "RegionMap": {"us-east-1": {"AMI": "ami-12345678"}}
  },
  "Resources": {
    "MyBucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {
        "BucketName": {"Ref": "Env"},
        "Port":       {"Ref": "Port"},
        "Tags":       {"Ref": "Tags"},
        "Sub1":       {"Fn::Sub": "env=${Env}"},
        "Sub2":       {"Fn::Sub": ["env=${Env}-${Extra}", {"Extra": {"Ref": "Env"}}]},
        "Joined":     {"Fn::Join": ["-", [{"Ref": "Env"}, "prod"]]},
        "AMI":        {"Fn::FindInMap": ["RegionMap", "us-east-1", "AMI"]},
        "GetAtt1":    {"Fn::GetAtt": "MyBucket.BucketName"},
        "GetAtt2":    {"Fn::GetAtt": ["MyBucket", "BucketName"]}
      }
    }
  }
}`

func TestYAMLJSONEquivalence(t *testing.T) {
	fake := &fakeRefValuer{
		refs: map[string]string{"MyBucket": "my-bucket-ref"},
		atts: map[string]string{"MyBucket.BucketName": "my-bucket-name"},
	}

	pyaml := mustParse(t, []Source{makeSource("yaml.yaml", yamlEquivTemplate)})
	pjson := mustParse(t, []Source{makeSource("json.json", jsonEquivTemplate)})

	tyaml := mustResolve(t, pyaml, fake, nil)
	tjson := mustResolve(t, pjson, fake, nil)

	if len(tyaml.Resources) != 1 || len(tjson.Resources) != 1 {
		t.Fatalf("expected 1 resource each, got yaml=%d json=%d", len(tyaml.Resources), len(tjson.Resources))
	}
	if !reflect.DeepEqual(tyaml.Resources[0].Properties, tjson.Resources[0].Properties) {
		t.Errorf("YAML and JSON resolved properties differ:\nYAML: %#v\nJSON: %#v",
			tyaml.Resources[0].Properties, tjson.Resources[0].Properties)
	}
}

func TestUnknownYAMLTag(t *testing.T) {
	src := makeSource("bad.yaml", `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      X: !Foo bar
`)
	_, err := Parse([]Source{src})
	if err == nil {
		t.Fatal("expected error for unknown tag !Foo, got nil")
	}
	if !strings.Contains(err.Error(), "!Foo") {
		t.Errorf("error should mention the tag !Foo, got: %v", err)
	}
}

func TestDuplicateLogicalID(t *testing.T) {
	src1 := makeSource("a.yaml", `
Resources:
  MyRes:
    Type: AWS::S3::Bucket
`)
	src2 := makeSource("b.yaml", `
Resources:
  MyRes:
    Type: AWS::S3::Bucket
`)
	_, err := Parse([]Source{src1, src2})
	if err == nil {
		t.Fatal("expected error for duplicate logical ID")
	}
	if !strings.Contains(err.Error(), "MyRes") {
		t.Errorf("error should mention 'MyRes', got: %v", err)
	}
	if !strings.Contains(err.Error(), "a.yaml") {
		t.Errorf("error should mention source file 'a.yaml', got: %v", err)
	}
}

func TestDuplicateParameterName(t *testing.T) {
	src1 := makeSource("a.yaml", `
Parameters:
  Env:
    Type: String
Resources:
  R:
    Type: AWS::S3::Bucket
`)
	src2 := makeSource("b.yaml", `
Parameters:
  Env:
    Type: String
Resources:
  S:
    Type: AWS::S3::Bucket
`)
	_, err := Parse([]Source{src1, src2})
	if err == nil {
		t.Fatal("expected error for duplicate parameter name")
	}
	if !strings.Contains(err.Error(), "Env") {
		t.Errorf("error should mention 'Env', got: %v", err)
	}
}

func TestDuplicateMappingName(t *testing.T) {
	src1 := makeSource("a.yaml", `
Mappings:
  MyMap:
    key1:
      val: v1
Resources:
  R:
    Type: AWS::S3::Bucket
`)
	src2 := makeSource("b.yaml", `
Mappings:
  MyMap:
    key2:
      val: v2
Resources:
  S:
    Type: AWS::S3::Bucket
`)
	_, err := Parse([]Source{src1, src2})
	if err == nil {
		t.Fatal("expected error for duplicate mapping name")
	}
	if !strings.Contains(err.Error(), "MyMap") {
		t.Errorf("error should mention 'MyMap', got: %v", err)
	}
}

func TestTransformRejected(t *testing.T) {
	src := makeSource("sam.yaml", `
Transform: AWS::Serverless-2016-10-31
Resources:
  R:
    Type: AWS::S3::Bucket
`)
	_, err := Parse([]Source{src})
	if err == nil {
		t.Fatal("expected error for Transform")
	}
	// should mention transforms or SAM
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "transform") && !strings.Contains(lower, "sam") {
		t.Errorf("error should mention transforms/SAM, got: %v", err)
	}
}

func TestNoResourcesSection(t *testing.T) {
	src := makeSource("noRes.yaml", `
Parameters:
  Env:
    Type: String
`)
	_, err := Parse([]Source{src})
	if err == nil {
		t.Fatal("expected error for missing Resources")
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "resource") {
		t.Errorf("error should mention resources, got: %v", err)
	}
}

// ── B) Intrinsic resolution ───────────────────────────────────────────────────

// parametricTemplate builds a single-resource template with one parameter named p.
func parametricTemplate(paramName, paramType, defaultValue string) string {
	defLine := ""
	if defaultValue != "" {
		defLine = "    Default: " + defaultValue
	}
	return `
Parameters:
  ` + paramName + `:
    Type: ` + paramType + `
` + defLine + `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Value:
        Ref: ` + paramName + `
`
}

func resolveFirstProp(t *testing.T, tmplStr string, params map[string]string, fake RefValuer, key string) any {
	t.Helper()
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, params)
	if len(tmpl.Resources) == 0 {
		t.Fatal("no resources after resolve")
	}
	return tmpl.Resources[0].Properties[key]
}

func TestRefParameterDefault(t *testing.T) {
	fake := &fakeRefValuer{}
	v := resolveFirstProp(t, parametricTemplate("MyParam", "String", "hello"), nil, fake, "Value")
	if v != "hello" {
		t.Errorf("expected 'hello', got %v", v)
	}
}

func TestRefParameterOverride(t *testing.T) {
	fake := &fakeRefValuer{}
	v := resolveFirstProp(t, parametricTemplate("MyParam", "String", "hello"),
		map[string]string{"MyParam": "world"}, fake, "Value")
	if v != "world" {
		t.Errorf("expected 'world', got %v", v)
	}
}

func TestRefParameterNoDefault(t *testing.T) {
	fake := &fakeRefValuer{}
	p := mustParse(t, []Source{makeSource("t.yaml", parametricTemplate("MyParam", "String", ""))})
	_, err := resolve(t, p, fake, nil)
	if err == nil {
		t.Fatal("expected error for parameter without default")
	}
	if !strings.Contains(err.Error(), "--parameter") {
		t.Errorf("error should suggest --parameter, got: %v", err)
	}
}

func TestOverrideUnknownParameter(t *testing.T) {
	fake := &fakeRefValuer{}
	p := mustParse(t, []Source{makeSource("t.yaml", parametricTemplate("MyParam", "String", "default"))})
	_, err := resolve(t, p, fake, map[string]string{"NoSuchParam": "val"})
	if err == nil {
		t.Fatal("expected error for unknown parameter override")
	}
	if !strings.Contains(err.Error(), "NoSuchParam") {
		t.Errorf("error should mention 'NoSuchParam', got: %v", err)
	}
}

func TestNumberParameterStringCoercion(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Parameters:
  Port:
    Type: Number
    Default: 8080
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Subbed:
        Fn::Sub: "port=${Port}"
`
	v := resolveFirstProp(t, tmplStr, nil, fake, "Subbed")
	if v != "port=8080" {
		t.Errorf("expected 'port=8080', got %v", v)
	}
}

func TestCommaDelimitedListParameter(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Parameters:
  Tags:
    Type: CommaDelimitedList
    Default: "a, b"
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Joined:
        Fn::Join:
          - "-"
          - Ref: Tags
`
	v := resolveFirstProp(t, tmplStr, nil, fake, "Joined")
	if v != "a-b" {
		t.Errorf("expected 'a-b', got %v", v)
	}
}

func TestPseudoParameters(t *testing.T) {
	tests := []struct {
		pseudo string
		want   string
	}{
		{"AWS::Region", DefaultRegion},
		{"AWS::AccountId", AccountID},
		{"AWS::Partition", "aws"},
		{"AWS::URLSuffix", "amazonaws.com"},
	}
	for _, tc := range tests {
		t.Run(tc.pseudo, func(t *testing.T) {
			tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Value:
        Ref: ` + tc.pseudo + `
`
			fake := &fakeRefValuer{}
			v := resolveFirstProp(t, tmplStr, nil, fake, "Value")
			if v != tc.want {
				t.Errorf("Ref %s: expected %q, got %v", tc.pseudo, tc.want, v)
			}
		})
	}
}

func TestNoValueDropsMapKey(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Keep: present
      Drop:
        Ref: AWS::NoValue
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	props := tmpl.Resources[0].Properties
	if _, ok := props["Drop"]; ok {
		t.Errorf("key 'Drop' should have been dropped by AWS::NoValue")
	}
	if props["Keep"] != "present" {
		t.Errorf("key 'Keep' should still be 'present', got %v", props["Keep"])
	}
}

func TestNoValueDropsListElement(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Items:
        - first
        - Ref: AWS::NoValue
        - last
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	items, ok := tmpl.Resources[0].Properties["Items"].([]any)
	if !ok {
		t.Fatalf("Items is not []any: %T", tmpl.Resources[0].Properties["Items"])
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items after NoValue drop, got %d: %v", len(items), items)
	}
}

func TestFnSubVariants(t *testing.T) {
	fake := &fakeRefValuer{
		refs: map[string]string{"MyRes": "res-val"},
		atts: map[string]string{"MyRes.Attr": "att-val"},
	}
	tmplStr := `
Parameters:
  Env:
    Type: String
    Default: staging
Resources:
  MyRes:
    Type: AWS::S3::Bucket
  R:
    Type: AWS::S3::Bucket
    Properties:
      FromParam:
        Fn::Sub: "env=${Env}"
      FromRef:
        Fn::Sub: "res=${MyRes}"
      FromAtt:
        Fn::Sub: "att=${MyRes.Attr}"
      Literal:
        Fn::Sub: "lit=${!Literal}"
      WithLocals:
        Fn::Sub:
          - "x=${X}"
          - X:
              Ref: Env
      LocalOverridesParam:
        Fn::Sub:
          - "env=${Env}"
          - Env: "override"
      NestedInLocals:
        Fn::Sub:
          - "nested=${V}"
          - V:
              Fn::Sub: "inner=${Env}"
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	props := tmpl.Resources[1].Properties // R is second alphabetically; "MyRes" < "R"

	tests := []struct{ key, want string }{
		{"FromParam", "env=staging"},
		{"FromRef", "res=res-val"},
		{"FromAtt", "att=att-val"},
		{"Literal", "lit=${Literal}"},
		{"WithLocals", "x=staging"},
		{"LocalOverridesParam", "env=override"},
		{"NestedInLocals", "nested=inner=staging"},
	}
	for _, tc := range tests {
		if props[tc.key] != tc.want {
			t.Errorf("Fn::Sub[%s]: expected %q, got %v", tc.key, tc.want, props[tc.key])
		}
	}
}

func TestFnJoin(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Parameters:
  Env:
    Type: String
    Default: prod
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      Simple:
        Fn::Join:
          - "-"
          - - hello
            - world
      WithSub:
        Fn::Join:
          - "."
          - - Fn::Sub: "pre-${Env}"
            - suffix
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	props := tmpl.Resources[0].Properties

	if props["Simple"] != "hello-world" {
		t.Errorf("Simple: expected 'hello-world', got %v", props["Simple"])
	}
	if props["WithSub"] != "pre-prod.suffix" {
		t.Errorf("WithSub: expected 'pre-prod.suffix', got %v", props["WithSub"])
	}
}

func TestFnFindInMap(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Mappings:
  RegionMap:
    us-east-1:
      AMI: ami-abc
    eu-west-1:
      AMI: ami-def
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      AMI:
        Fn::FindInMap:
          - RegionMap
          - us-east-1
          - AMI
`
	v := resolveFirstProp(t, tmplStr, nil, fake, "AMI")
	if v != "ami-abc" {
		t.Errorf("expected 'ami-abc', got %v", v)
	}
}

func TestFnFindInMapMissingMap(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      X:
        Fn::FindInMap:
          - NoSuchMap
          - key1
          - key2
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	_, err := resolve(t, p, fake, nil)
	if err == nil {
		t.Fatal("expected error for missing map")
	}
	if !strings.Contains(err.Error(), "NoSuchMap") {
		t.Errorf("error should mention 'NoSuchMap', got: %v", err)
	}
}

func TestFnFindInMapMissingKey(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Mappings:
  MyMap:
    existing:
      val: ok
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      X:
        Fn::FindInMap:
          - MyMap
          - missing
          - val
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	_, err := resolve(t, p, fake, nil)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should mention 'missing', got: %v", err)
	}
}

func TestGetAttDottedVsList(t *testing.T) {
	fake := &fakeRefValuer{
		atts: map[string]string{"MyRes.MyAttr": "attr-val"},
	}
	tmplStr := `
Resources:
  MyRes:
    Type: AWS::S3::Bucket
  R:
    Type: AWS::S3::Bucket
    Properties:
      Dotted:
        Fn::GetAtt: MyRes.MyAttr
      Listed:
        Fn::GetAtt:
          - MyRes
          - MyAttr
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	// R is sorted after MyRes
	props := tmpl.Resources[1].Properties
	if props["Dotted"] != "attr-val" {
		t.Errorf("Dotted GetAtt: expected 'attr-val', got %v", props["Dotted"])
	}
	if props["Listed"] != "attr-val" {
		t.Errorf("Listed GetAtt: expected 'attr-val', got %v", props["Listed"])
	}
}

func TestGetAttMultiPartAttribute(t *testing.T) {
	// list form with 3 parts: ["F","FunctionMetadata","FunctionARN"]
	// → attr = "FunctionMetadata.FunctionARN"
	fake := &fakeRefValuer{
		atts: map[string]string{"F.FunctionMetadata.FunctionARN": "arn:fn"},
	}
	tmplStr := `
Resources:
  F:
    Type: AWS::CloudFront::Function
  R:
    Type: AWS::S3::Bucket
    Properties:
      ARN:
        Fn::GetAtt:
          - F
          - FunctionMetadata
          - FunctionARN
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	props := tmpl.Resources[1].Properties
	if props["ARN"] != "arn:fn" {
		t.Errorf("multi-part GetAtt: expected 'arn:fn', got %v", props["ARN"])
	}
}

func TestUnsupportedIntrinsics(t *testing.T) {
	tests := []struct {
		name      string
		intrinsic string
	}{
		{"ImportValue", `{"Fn::ImportValue": "some-export"}`},
		{"If", `{"Fn::If": ["Cond", "a", "b"]}`},
		{"Select", `{"Fn::Select": [0, ["a"]]}`},
		{"GetAZs", `{"Fn::GetAZs": ""}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmplStr := `{"Resources": {"R": {"Type": "AWS::S3::Bucket", "Properties": {"V": ` + tc.intrinsic + `}}}}`
			fake := &fakeRefValuer{}
			p := mustParse(t, []Source{makeSource("t.json", tmplStr)})
			_, err := resolve(t, p, fake, nil)
			if err == nil {
				t.Fatalf("expected UnsupportedIntrinsicError for %s", tc.name)
			}
			var uie *UnsupportedIntrinsicError
			if !errors.As(err, &uie) {
				t.Errorf("expected *UnsupportedIntrinsicError, got %T: %v", err, err)
			} else if !strings.Contains(uie.Name, "Fn::"+tc.name) && uie.Name != "Fn::"+tc.name {
				// For ImportValue: Fn::ImportValue; If: Fn::If, etc.
				t.Errorf("UnsupportedIntrinsicError.Name should contain the intrinsic name, got %q", uie.Name)
			}
		})
	}
}

func TestUnresolvableRefProducesPlaceholderAndWarning(t *testing.T) {
	fake := &fakeRefValuer{} // returns nothing
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      V:
        Ref: Unknown
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	props := tmpl.Resources[0].Properties
	v, ok := props["V"].(string)
	if !ok {
		t.Fatalf("expected string prop, got %T", props["V"])
	}
	if !IsUnresolved(v) {
		t.Errorf("value %q should be marked as unresolved", v)
	}
	if !strings.HasPrefix(v, UnresolvedPrefix) {
		t.Errorf("value %q should start with UnresolvedPrefix", v)
	}
	if len(tmpl.Warnings) == 0 {
		t.Error("expected at least one warning for unresolvable reference")
	}
}

func TestUnresolvableRefWarningDeduplication(t *testing.T) {
	// Warnings carry the property path and are deduplicated per message: two
	// refs to the same unknown ID from different paths keep one warning each,
	// so every offending property is named exactly once.
	fake := &fakeRefValuer{}
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Properties:
      V1:
        Ref: Unknown
      V2:
        Ref: Unknown
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	tmpl := mustResolve(t, p, fake, nil)
	var v1, v2 int
	for _, w := range tmpl.Warnings {
		if !strings.Contains(w, "Unknown") {
			continue
		}
		if strings.Contains(w, "/V1") {
			v1++
		}
		if strings.Contains(w, "/V2") {
			v2++
		}
	}
	if v1 != 1 || v2 != 1 {
		t.Errorf("expected one warning per property path, got V1=%d V2=%d: %v", v1, v2, tmpl.Warnings)
	}
}

func TestResourceWithConditionErrors(t *testing.T) {
	fake := &fakeRefValuer{}
	tmplStr := `
Resources:
  R:
    Type: AWS::S3::Bucket
    Condition: SomeCondition
    Properties:
      X: y
`
	p := mustParse(t, []Source{makeSource("t.yaml", tmplStr)})
	// Include all resources so the condition error fires
	_, err := p.Resolve(ResolveOptions{
		Refs: fake,
	})
	if err == nil {
		t.Fatal("expected error for resource with Condition")
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "condition") {
		t.Errorf("error should mention 'Condition', got: %v", err)
	}
}
