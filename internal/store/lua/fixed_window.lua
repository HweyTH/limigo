-- Fixed window rate limiter.
-- Increments the request counter for the current window and returns 1 if the
-- request is within the limit, 0 if it should be throttled.
-- The window resets automatically when the key expires.
--
-- KEYS[1] - rate limit key
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds

local count = redis.call('INCR', KEYS[1])

if count == 1 then 
    redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end

if count <= tonumber(ARGV[1]) then 
    return 1
else 
    return 0
end