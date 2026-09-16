-- Sliding window rate limiter.
-- Records the current request in a sorted set keyed by timestamp, evicts entries
-- outside the rolling window, and returns 1 if the request is within the limit,
-- 0 if it should be throttled. Time is sourced from Redis to avoid clock skew
-- across application nodes.
--
-- Key layout: this script reads and writes exactly KEYS[1] and never builds a
-- key name of its own. Redis Cluster only runs a script whose keys share one
-- hash slot, so one declared key makes it cluster-safe as-is. Do not add a
-- hash tag to the key prefix: keys are independent per rule and caller, and
-- a tag would pin all of them to one slot. Calling TIME and then writing
-- needs Redis 5+ (effects replication). See README "Single-key Lua scripts".
--
-- KEYS[1] - rate limit key (sorted set)
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds
local t0 = redis.call('TIME')

local time = redis.call('TIME')

local now_ms = time[1] * 1000 + math.floor(time[2]/1000)

local window_start = now_ms - tonumber(ARGV[2])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', window_start)

local num_requests = redis.call('ZCARD', KEYS[1])

local allowed = 0

if num_requests < tonumber(ARGV[1]) then
    local member = tostring(time[1]) .. ':' .. tostring(time[2])
    redis.call('ZADD', KEYS[1], now_ms, member)
    redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
    allowed = 1
end

local t1 = redis.call('TIME')
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])

return {allowed, lua_us}
