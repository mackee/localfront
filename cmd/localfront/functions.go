package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/config"
	"github.com/mackee/localfront/internal/origin"
)

// reachableResources returns the LogicalIDs of CloudFront Functions and
// KeyValueStores reachable from enabled distributions. Disabled distributions
// are never routed (see buildRoutes), so their functions are not compiled and
// their stores do not require the object store — mirroring how routing ignores
// them. Functions/stores not associated with any enabled behavior are also
// excluded, since CloudFront never executes them.
func reachableResources(cfg *config.Config) (funcs, kvs map[string]bool) {
	funcs = map[string]bool{}
	kvs = map[string]bool{}
	for _, d := range cfg.Distributions {
		if !d.Enabled {
			continue
		}
		behaviors := make([]*config.Behavior, 0, len(d.Behaviors)+1)
		behaviors = append(behaviors, d.DefaultBehavior)
		behaviors = append(behaviors, d.Behaviors...)
		for _, b := range behaviors {
			if b == nil {
				continue
			}
			for _, fn := range []*config.Function{b.ViewerRequest, b.ViewerResponse} {
				if fn == nil {
					continue
				}
				funcs[fn.LogicalID] = true
				for _, store := range fn.KeyValueStores {
					kvs[store.LogicalID] = true
				}
			}
		}
	}
	return funcs, kvs
}

// buildFunctions compiles the CloudFront Functions reachable from enabled
// distributions, wiring each to its associated (seeded) KeyValueStore. On any
// compile error it closes the functions it already built and returns the error.
func buildFunctions(cfg *config.Config, s3 origin.Fetcher, seeds map[string]string, logger *slog.Logger) (map[string]*cffunc.Function, error) {
	reachableFuncs, reachableKVS := reachableResources(cfg)
	stores, err := buildKVSStores(cfg, reachableKVS, s3, seeds, logger)
	if err != nil {
		return nil, err
	}

	funcs := map[string]*cffunc.Function{}
	for _, fn := range cfg.Functions {
		if !reachableFuncs[fn.LogicalID] {
			// Compiling a function only a disabled distribution references would
			// make an unreachable function's compile error fatal to startup.
			continue
		}
		var kvs *cffunc.KVS
		if len(fn.KeyValueStores) > 0 {
			kvs = stores[fn.KeyValueStores[0].LogicalID]
			if len(fn.KeyValueStores) > 1 {
				logger.Warn("function associates multiple KeyValueStores; only the first is used by cloudfront-js cf.kvs()",
					"function", fn.LogicalID)
			}
		}
		name := fn.Name
		compiled, err := cffunc.Compile(cffunc.Options{
			Name:    name,
			Code:    fn.Code,
			Runtime: fn.Runtime,
			KVS:     kvs,
			Log:     func(msg string) { logger.Debug("cloudfront function console", "function", name, "message", msg) },
		})
		if err != nil {
			closeFunctions(funcs)
			return nil, fmt.Errorf("compiling function %s: %w", fn.LogicalID, err)
		}
		funcs[fn.LogicalID] = compiled
	}
	return funcs, nil
}

// closeFunctions releases every compiled function's runtimes.
func closeFunctions(funcs map[string]*cffunc.Function) {
	for _, f := range funcs {
		f.Close()
	}
}

// buildKVSStores creates and seeds the in-memory KeyValueStores. A matching
// --kvs-seed replaces the store's contents entirely, and when one is present
// the ImportSource is skipped: the local seed stands in for the production
// import, so an offline run needs neither the object store nor a reachable
// ImportSource. Otherwise the store is seeded from its ImportSource (fetched
// from the object store). A store reachable only from disabled distributions is
// created empty: its ImportSource is not fetched, so it does not force the
// object-store requirement.
func buildKVSStores(cfg *config.Config, reachableKVS map[string]bool, s3 origin.Fetcher, seeds map[string]string, logger *slog.Logger) (map[string]*cffunc.KVS, error) {
	stores := map[string]*cffunc.KVS{}
	usedSeeds := map[string]bool{}

	for _, kvs := range cfg.KeyValueStores {
		data := map[string]string{}

		switch key, path, hasSeed := lookupSeed(seeds, kvs.Name, kvs.LogicalID); {
		case hasSeed:
			seeded, err := loadKVSSeedFile(path)
			if err != nil {
				return nil, fmt.Errorf("KeyValueStore %s seed file %s: %w", kvs.Name, path, err)
			}
			data = seeded
			usedSeeds[key] = true
			logger.Info("KeyValueStore seeded from file", "store", kvs.Name, "file", path, "keys", len(data))
		case kvs.ImportSourceARN != "" && reachableKVS[kvs.LogicalID]:
			if s3 == nil {
				return nil, fmt.Errorf("KeyValueStore %s has an ImportSource but no object store is configured (set --s3-endpoint, or provide a local --kvs-seed)", kvs.Name)
			}
			imported, err := loadKVSImportSource(s3, kvs.ImportSourceARN)
			if err != nil {
				return nil, fmt.Errorf("KeyValueStore %s ImportSource: %w", kvs.Name, err)
			}
			data = imported
			logger.Info("KeyValueStore seeded from ImportSource", "store", kvs.Name, "keys", len(data))
		}

		store := cffunc.NewKVS()
		store.Replace(data)
		stores[kvs.LogicalID] = store
	}

	for key := range seeds {
		if !usedSeeds[key] {
			logger.Warn("--kvs-seed refers to a store that is not defined in the templates", "store", key)
		}
	}
	return stores, nil
}

// lookupSeed finds the --kvs-seed entry for a store, matched by its name first
// and then its logical ID (the precedence buildKVSStores applies). It returns
// the matched key, the seed file path, and whether a seed was found.
func lookupSeed(seeds map[string]string, name, logicalID string) (key, path string, ok bool) {
	for _, k := range []string{name, logicalID} {
		if k == "" {
			continue
		}
		if p, found := seeds[k]; found {
			return k, p, true
		}
	}
	return "", "", false
}

func loadKVSImportSource(s3 origin.Fetcher, arn string) (map[string]string, error) {
	bucket, key, err := parseS3ObjectARN(arn)
	if err != nil {
		return nil, err
	}
	resp, err := s3.Fetch(context.Background(), &origin.Request{Bucket: bucket, Key: key, Method: "GET"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching s3://%s/%s returned status %d", bucket, key, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return cffunc.ParseSeed(raw)
}

func loadKVSSeedFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return cffunc.ParseSeed(raw)
}

// parseS3ObjectARN splits an S3 object ARN (arn:aws:s3:::bucket/key) into its
// bucket and key.
func parseS3ObjectARN(arn string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(arn, "arn:aws:s3:::")
	if !ok {
		return "", "", fmt.Errorf("not an S3 object ARN: %q", arn)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("S3 ARN %q must be arn:aws:s3:::<bucket>/<key>", arn)
	}
	return bucket, key, nil
}
