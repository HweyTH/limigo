// Package store defines distributed backing-store contracts for Limigo limiter
// algorithms.
//
// Store implementations preserve atomic rate limit semantics across nodes, such
// as by executing Redis Lua scripts server-side.
package store
