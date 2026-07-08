// Package rules compiles validated YAML rule configuration into executable
// limiters backed by a distributed store.
//
// Compile turns each config.Rule into a CompiledRule: a request matcher paired
// with a closure that evaluates the rule's configured algorithm (fixed
// window, sliding window, or token bucket) against a Store. It is the bridge
// between internal/config and internal/store.
package rules
