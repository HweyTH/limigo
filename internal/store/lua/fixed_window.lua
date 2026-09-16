-- Fixed window rate limiter.
-- Increments the request counter for the current window and returns 1 if the
-- request is within the limit, 0 if it should be throttled.
-- The window resets automatically when the key expires.
--
-- Returns {allowed, lua_us, remaining, reset_ms}: remaining is the quota
-- left in this window after the increment (never negative), reset_ms the
-- time until the key expires and the window rolls over. Both are computed
-- here because the script already holds them; a second round-trip to read
-- them back would race the next increment.
--
-- Key layout: this script reads and writes exactly KEYS[1] and never builds a
-- key name of its own. Redis Cluster only runs a script whose keys share one
-- hash slot, so one declared key makes it cluster-safe as-is. Do not add a
-- hash tag to the key prefix: keys are independent per rule and caller, and
-- a tag would pin all of them to one slot. Calling TIME and then writing
-- needs Redis 5+ (effects replication). See README "Single-key Lua scripts".
--
-- KEYS[1] - rate limit key
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds

local t0 = redis.call('TIME')

local count = redis.call('INCR', KEYS[1])

if count == 1 then 
    redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end

local limit = tonumber(ARGV[1])
local remaining = limit - count
if remaining < 0 then
    remaining = 0
end

-- PTTL is -1 for a key with no expiry and -2 for a missing key; neither can
-- happen here (the key was just incremented and given an expiry on creation),
-- but clamp so a surprise reads as "no reset known" rather than a negative.
local reset_ms = redis.call('PTTL', KEYS[1])
if reset_ms < 0 then
    reset_ms = 0
end

local t1 = redis.call('TIME')
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])

if count <= limit then
    return {1, lua_us, remaining, reset_ms}
else
    return {0, lua_us, remaining, reset_ms}
end
