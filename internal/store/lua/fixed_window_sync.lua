-- Fixed window rate limiter — batched delta sync.
-- Reconciles a batch of already-admitted local requests with the authoritative
-- counter in Redis. Unlike fixed_window.lua, this script does not decide
-- allow/deny — the local node already made that decision optimistically
-- before this script runs. This script only folds the batch into Redis's
-- authoritative count and reports the resulting total so the calling node can
-- recalibrate its local baseline for the next flush interval.
--
-- Key layout: this script reads and writes exactly KEYS[1] and never builds a
-- key name of its own. Redis Cluster only runs a script whose keys share one
-- hash slot, so one declared key makes it cluster-safe as-is. Do not add a
-- hash tag to the key prefix: keys are independent per rule and caller, and
-- a tag would pin all of them to one slot. Calling TIME and then writing
-- needs Redis 5+ (effects replication). See README "Single-key Lua scripts".
--
-- KEYS[1] - rate limit key
-- ARGV[1] - delta (number of requests already admitted locally since the last sync)
-- ARGV[2] - window duration in milliseconds

local delta = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])

local t0 = redis.call('TIME')

if delta <= 0 then
    local current = tonumber(redis.call('GET', KEYS[1])) or 0
    local t1 = redis.call('TIME')
    local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])
    return {current, lua_us}
end

local count = redis.call('INCRBY', KEYS[1], delta)

if count == delta then
    redis.call('PEXPIRE', KEYS[1], window_ms)
end

local t1 = redis.call('TIME')
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])

return {count, lua_us}
