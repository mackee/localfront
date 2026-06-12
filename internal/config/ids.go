package config

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/mackee/localfront/internal/cfntmpl"
)

// Generated identifiers are derived deterministically from template logical
// IDs so they are stable across restarts and across machines.

const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func idSuffix(salt, logicalID string) string {
	sum := sha256.Sum256([]byte(salt + ":" + logicalID))
	out := make([]byte, 13)
	for i := range out {
		out[i] = idAlphabet[int(sum[i])%len(idAlphabet)]
	}
	return string(out)
}

// distributionID mimics CloudFront distribution IDs (E + 13 alphanumerics).
func distributionID(logicalID string) string {
	return "E" + idSuffix("distribution", logicalID)
}

// publicKeyID mimics CloudFront public key IDs (K + 13 alphanumerics).
func publicKeyID(logicalID string) string {
	return "K" + idSuffix("public-key", logicalID)
}

// oacID mimics origin access control IDs.
func oacID(logicalID string) string {
	return "E" + idSuffix("origin-access-control", logicalID)
}

// uuidID derives a UUID-formatted ID, as used by cache policies, origin
// request policies, response headers policies, key groups, and KVS IDs.
func uuidID(salt, logicalID string) string {
	sum := sha256.Sum256([]byte(salt + ":" + logicalID))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func distributionARN(id string) string {
	return "arn:aws:cloudfront::" + cfntmpl.AccountID + ":distribution/" + id
}

func functionARN(name string) string {
	return "arn:aws:cloudfront::" + cfntmpl.AccountID + ":function/" + name
}

func keyValueStoreARN(id string) string {
	return "arn:aws:cloudfront::" + cfntmpl.AccountID + ":key-value-store/" + id
}
