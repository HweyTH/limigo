// Package limiter provides in-process rate limiting algorithms behind a common
// interface.
//
// It includes token bucket, sliding window log, and fixed window counter
// implementations for callers that do not need a distributed backing store.
package limiter
