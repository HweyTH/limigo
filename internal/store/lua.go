package store

import (
	"embed"
	"fmt"
)

//go:embed lua/*.lua
var luaFiles embed.FS

// LoadEmbeddedScripts returns the Lua source for the fixed window, sliding
// window, token bucket, and leaky bucket algorithms, plus the batched
// delta-sync variants used by node-local caching, embedded into the binary at
// build time so RedisStore has no runtime dependency on the working directory.
func LoadEmbeddedScripts() (fixedWindow, slidingWindow, tokenBucket, leakyBucket, fixedWindowSync, tokenBucketSync string, err error) {
	fxScript, err := luaFiles.ReadFile("lua/fixed_window.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/fixed_window.lua", err)
	}

	swScript, err := luaFiles.ReadFile("lua/sliding_window.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/sliding_window.lua", err)
	}

	tbScript, err := luaFiles.ReadFile("lua/token_bucket.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/token_bucket.lua", err)
	}

	lbScript, err := luaFiles.ReadFile("lua/leaky_bucket.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/leaky_bucket.lua", err)
	}

	fxSyncScript, err := luaFiles.ReadFile("lua/fixed_window_sync.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/fixed_window_sync.lua", err)
	}

	tbSyncScript, err := luaFiles.ReadFile("lua/token_bucket_sync.lua")
	if err != nil {
		return "", "", "", "", "", "", fmt.Errorf("read embedded lua script %q: %w", "lua/token_bucket_sync.lua", err)
	}

	return string(fxScript), string(swScript), string(tbScript), string(lbScript), string(fxSyncScript), string(tbSyncScript), nil
}
