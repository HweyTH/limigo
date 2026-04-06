-- Sliding window rate limiter.
-- Records the current request in a sorted set keyed by timestamp, evicts entries
-- outside the rolling window, and returns 1 if the request is within the limit,
-- 0 if it should be throttled. Time is sourced from Redis to avoid clock skew
-- across application nodes.
--
-- KEYS[1] - rate limit key (sorted set)
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds

local time = redis.call('TIME')

local now_ms = time[1] * 1000 + math.floor(time[2]/1000)

local window_start = now_ms - tonumber(ARGV[2])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', window_start)

local num_requests = redis.call('ZCARD', KEYS[1])

if num_requests < tonumber(ARGV[1]) then
    redis.call('ZADD', KEYS[1], now_ms, tostring(now_ms))
    redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))

    return 1
else
    return 0
end
