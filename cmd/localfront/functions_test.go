package main

import (
	"os"
	"path/filepath"
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
	if configUsesKVSImportSource(cfg, nil) {
		t.Error("import source reachable only from a disabled distribution should not require S3")
	}
	cfg.Distributions[0].Enabled = true
	if !configUsesKVSImportSource(cfg, nil) {
		t.Error("import source reachable from an enabled distribution should require S3")
	}
	// A local --kvs-seed replaces the ImportSource, so the object store is no
	// longer required for that store (matched by name or logical ID).
	if configUsesKVSImportSource(cfg, map[string]string{"store": "seed.json"}) {
		t.Error("a --kvs-seed for the store should exempt it from the S3 requirement")
	}
	if configUsesKVSImportSource(cfg, map[string]string{"Kvs": "seed.json"}) {
		t.Error("a --kvs-seed matched by logical ID should exempt it from the S3 requirement")
	}
}

// A --kvs-seed for a store with an ImportSource must let it build offline: the
// seed replaces the import, so no object store is fetched even when s3 is nil.
func TestBuildKVSStores_SeedReplacesImportSource(t *testing.T) {
	kvs := &config.KeyValueStore{LogicalID: "Kvs", Name: "store", ImportSourceARN: "arn:aws:s3:::b/seed.json"}
	fn := &config.Function{LogicalID: "Fn", KeyValueStores: []*config.KeyValueStore{kvs}}
	cfg := &config.Config{
		Distributions:  []*config.Distribution{{LogicalID: "D1", Enabled: true, DefaultBehavior: fnBehavior(fn)}},
		Functions:      []*config.Function{fn},
		KeyValueStores: []*config.KeyValueStore{kvs},
	}

	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	_, reachableKVS := reachableResources(cfg)
	// s3 is nil: the ImportSource must be skipped because a seed covers the store.
	stores, err := buildKVSStores(t.Context(), cfg, reachableKVS, nil, map[string]string{"store": seedPath}, testLogger())
	if err != nil {
		t.Fatalf("buildKVSStores with a seed should not require S3, got: %v", err)
	}
	store, ok := stores["Kvs"]
	if !ok {
		t.Fatal("store Kvs was not built")
	}
	if got, _ := store.Get("hello"); got != "world" {
		t.Errorf("store value = %q, want world (seeded from file)", got)
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
	funcs, err := buildFunctions(t.Context(), cfg, nil, nil, testLogger())
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
