-- Fixed window rate limiter.
-- Increments the request counter for the current window and returns 1 if the
-- request is within the limit, 0 if it should be throttled.
-- The window resets automatically when the key expires.
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