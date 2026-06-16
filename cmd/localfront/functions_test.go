package main

import (
	"testing"

	"github.com/mackee/localfront/internal/config"
)

func fnBehavior(fn *config.Function) *config.Behavior {
	return &config.Behavior{
		AllowedMethods: []string{"GET"},
		CachePolicy:    &config.CachePolicy{},
		ViewerRequest:  fn,
	}
}

// An S3 origin on a disabled distribution is never routed, so it must not force
// the --s3-endpoint requirement.
func TestConfigUsesS3_IgnoresDisabled(t *testing.T) {
	s3Origin := &config.Origin{ID: "s3", S3: &config.S3Origin{Bucket: "b"}}
	disabled := &config.Distribution{LogicalID: "D1", Origins: []*config.Origin{s3Origin}}
	if configUsesS3(&config.Config{Distributions: []*config.Distribution{disabled}}) {
		t.Error("configUsesS3 should be false when the only S3 distribution is disabled")
	}
	enabled := &config.Distribution{LogicalID: "D2", Enabled: true, Origins: []*config.Origin{s3Origin}}
	if !configUsesS3(&config.Config{Distributions: []*config.Distribution{enabled}}) {
		t.Error("configUsesS3 should be true for an enabled S3 distribution")
	}
}

func TestReachableResources_OnlyEnabled(t *testing.T) {
	kvsA := &config.KeyValueStore{LogicalID: "KvsA"}
	kvsB := &config.KeyValueStore{LogicalID: "KvsB"}
	fnA := &config.Function{LogicalID: "FnA", KeyValueStores: []*config.KeyValueStore{kvsA}}
	fnB := &config.Function{LogicalID: "FnB", KeyValueStores: []*config.KeyValueStore{kvsB}}

	cfg := &config.Config{
		Distributions: []*config.Distribution{
			{LogicalID: "D1", Enabled: true, DefaultBehavior: fnBehavior(fnA)},
			{LogicalID: "D2", Enabled: false, DefaultBehavior: fnBehavior(fnB)},
		},
		Functions:      []*config.Function{fnA, fnB},
		KeyValueStores: []*config.KeyValueStore{kvsA, kvsB},
	}
	funcs, kvs := reachableResources(cfg)
	if !funcs["FnA"] || funcs["FnB"] {
		t.Errorf("reachable funcs = %v, want only FnA", funcs)
	}
	if !kvs["KvsA"] || kvs["KvsB"] {
		t.Errorf("reachable kvs = %v, want only KvsA", kvs)
	}
}

// A KVS ImportSource reachable only from a disabled distribution must not force
// the object-store requirement (mirroring configUsesS3).
func TestConfigUsesKVSImportSource_IgnoresDisabledOnly(t *testing.T) {
	kvs := &config.KeyValueStore{LogicalID: "Kvs", Name: "store", ImportSourceARN: "arn:aws:s3:::b/seed.json"}
	fn := &config.Function{LogicalID: "Fn", KeyValueStores: []*config.KeyValueStore{kvs}}
	cfg := &config.Config{
		Distributions:  []*config.Distribution{{LogicalID: "D1", DefaultBehavior: fnBehavior(fn)}},
		Functions:      []*config.Function{fn},
		KeyValueStores: []*config.KeyValueStore{kvs},
	}
	if configUsesKVSImportSource(cfg) {
		t.Error("import source reachable only from a disabled distribution should not require S3")
	}
	cfg.Distributions[0].Enabled = true
	if !configUsesKVSImportSource(cfg) {
		t.Error("import source reachable from an enabled distribution should require S3")
	}
}

// A broken function referenced only by a disabled distribution must not abort
// startup, and must not be compiled.
func TestBuildFunctions_SkipsUnreachable(t *testing.T) {
	const validCode = "function handler(event) { return event.request; }"
	const brokenCode = "function handler(event) { return ; ;; ) }"
	reachable := &config.Function{LogicalID: "Reachable", Name: "reachable", Code: validCode}
	unreachable := &config.Function{LogicalID: "Unreachable", Name: "unreachable", Code: brokenCode}

	cfg := &config.Config{
		Distributions: []*config.Distribution{
			{LogicalID: "D1", Enabled: true, DefaultBehavior: fnBehavior(reachable)},
			{LogicalID: "D2", Enabled: false, DefaultBehavior: fnBehavior(unreachable)},
		},
		Functions: []*config.Function{reachable, unreachable},
	}
	funcs, err := buildFunctions(cfg, nil, nil, testLogger())
	if err != nil {
		t.Fatalf("buildFunctions should ignore a broken function on a disabled distribution, got: %v", err)
	}
	defer closeFunctions(funcs)
	if _, ok := funcs["Reachable"]; !ok {
		t.Error("reachable function should be compiled")
	}
	if _, ok := funcs["Unreachable"]; ok {
		t.Error("unreachable function should not be compiled")
	}
}
