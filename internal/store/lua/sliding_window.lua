-- Sliding window rate limiter.
-- Records the current request in a sorted set keyed by timestamp, evicts entries
-- outside the rolling window, and returns 1 if the request is within the limit,
-- 0 if it should be throttled. Time is sourced from Redis to avoid clock skew
-- across application nodes.
--
-- Returns {allowed, lua_us, remaining, reset_ms}: remaining is the quota
-- left in the rolling window after this request (never negative), reset_ms
-- the time until the oldest recorded request leaves the window — the moment
-- one unit of quota next comes back.
--
-- Key layout: this script reads and writes exactly KEYS[1] and never builds a
-- key name of its own. Redis Cluster only runs a script whose keys share one
-- hash slot, so one declared key makes it cluster-safe as-is. Do not add a
-- hash tag to the key prefix: keys are independent per rule and caller, and
-- a tag would pin all of them to one slot. Redis 7.4+: the script times
-- itself with os.clock(), because TIME is frozen for the whole execution.
-- See README "Single-key Lua scripts".
--
-- KEYS[1] - rate limit key (sorted set)
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds
-- ARGV[3] - optional; 'peek' reports the state a request arriving now would
--           see without recording one (the Admin API's quota inspection):
--           it counts the live window instead of evicting and adding.
local t0 = os.clock()

local time = redis.call('TIME')

local now_ms = time[1] * 1000 + math.floor(time[2]/1000)

local window_start = now_ms - tonumber(ARGV[2])

local limit = tonumber(ARGV[1])
local peek = ARGV[3] == 'peek'
local allowed = 0
local num_requests
local oldest

if peek then
    -- Entries at or before window_start are what the write path would evict;
    -- count strictly after it, and read the oldest survivor for reset.
    num_requests = redis.call('ZCOUNT', KEYS[1], '(' .. window_start, '+inf')
    if num_requests < limit then
        allowed = 1
    end
    oldest = redis.call('ZRANGEBYSCORE', KEYS[1], '(' .. window_start, '+inf', 'WITHSCORES', 'LIMIT', 0, 1)
else
    redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', window_start)
    num_requests = redis.call('ZCARD', KEYS[1])
    if num_requests < limit then
        local member = tostring(time[1]) .. ':' .. tostring(time[2])
        redis.call('ZADD', KEYS[1], now_ms, member)
        redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
        allowed = 1
        num_requests = num_requests + 1
    end
    oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
end

local remaining = limit - num_requests
if remaining < 0 then
    remaining = 0
end

-- The oldest entry's score plus the window is when it will be evicted.
local reset_ms = 0
if oldest[2] then
    reset_ms = tonumber(oldest[2]) + tonumber(ARGV[2]) - now_ms
    if reset_ms < 0 then
        reset_ms = 0
    end
end

local lua_us = math.floor((os.clock() - t0) * 1000000)

return {allowed, lua_us, remaining, reset_ms}
