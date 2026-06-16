package cffunc

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrKeyNotFound is returned by KVS operations when the key is absent.
// The cloudfront-js runtime surfaces it as a JS exception that user code
// can catch around cf.kvs().get().
var ErrKeyNotFound = errors.New("key not found")

// KVS is an in-memory CloudFront KeyValueStore. It is safe for concurrent use
// and its contents are swappable atomically (hot reload / re-seed).
type KVS struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewKVS returns an empty store.
func NewKVS() *KVS {
	return &KVS{data: map[string]string{}}
}

// Get returns the value for key.
func (k *KVS) Get(key string) (string, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.data[key]
	return v, ok
}

// Len reports the number of keys.
func (k *KVS) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.data)
}

// Replace atomically swaps the store's contents.
func (k *KVS) Replace(data map[string]string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.data = data
}

// ParseSeed parses a KVS seed document. It accepts the AWS KeyValueStore bulk
// format ({"data":[{"key":..,"value":..}]}) and a plain {"key":"value"} object.
func ParseSeed(raw []byte) (map[string]string, error) {
	var bulk struct {
		Data []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &bulk); err == nil && bulk.Data != nil {
		out := make(map[string]string, len(bulk.Data))
		for _, e := range bulk.Data {
			out[e.Key] = e.Value
		}
		return out, nil
	}

	var flat map[string]string
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("KVS seed must be the bulk format {\"data\":[{\"key\",\"value\"}]} or a flat {\"key\":\"value\"} object: %w", err)
	}
	return flat, nil
}
