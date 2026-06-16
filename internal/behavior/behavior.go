// Package behavior implements the CloudFront cache-behavior semantics that sit
// between host routing and the origin fetch: path-pattern selection, the
// cache / origin-request policy rules that decide what reaches the origin,
// response headers policies, compression, and the CloudFront-Viewer-* headers.
//
// The functions here are deliberately free of HTTP transport concerns so they
// can be unit-tested in isolation; the data plane wires them into the request
// pipeline.
package behavior

import (
	"strings"

	"github.com/mackee/localfront/internal/config"
)

// Select returns the behavior that serves path. Cache behaviors are evaluated
// in template order and the first whose path pattern matches wins (CloudFront
// uses listing order, not specificity). When none match, the default behavior
// serves the request.
func Select(d *config.Distribution, path string) *config.Behavior {
	for _, b := range d.Behaviors {
		if MatchPath(b.PathPattern, path) {
			return b
		}
	}
	return d.DefaultBehavior
}

// MatchPath reports whether a CloudFront path pattern matches a request path.
//
// Patterns use two wildcards — '*' (zero or more characters) and '?' (exactly
// one character) — match case-sensitively, and may be written with or without
// a leading slash (CloudFront treats "images/*" and "/images/*" alike). Both
// wildcards span '/' boundaries, unlike shell globbing.
func MatchPath(pattern, path string) bool {
	return matchGlob(strings.TrimPrefix(pattern, "/"), strings.TrimPrefix(path, "/"))
}

// matchGlob is an iterative wildcard matcher with backtracking on '*'. It runs
// in linear time for the patterns CloudFront accepts and never recurses.
func matchGlob(pattern, s string) bool {
	var (
		p, str       int
		star         = -1
		starMatchEnd int
	)
	for str < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[str]):
			p++
			str++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			starMatchEnd = str
			p++
		case star != -1:
			// Backtrack: let the last '*' swallow one more character.
			p = star + 1
			starMatchEnd++
			str = starMatchEnd
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
