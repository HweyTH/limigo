-- Fixed window rate limiter.
-- Increments the request counter for the current window and returns 1 if the
-- request is within the limit, 0 if it should be throttled.
-- The window resets automatically when the key expires.
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

local t1 = redis.call('TIME')
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])

if count <= tonumber(ARGV[1]) then 
    return {1, lua_us}
else 
    return {0, lua_us}
end
