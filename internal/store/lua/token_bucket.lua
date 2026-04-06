-- Token bucket rate limiter.
-- Refills tokens based on elapsed time since the last request, then consumes one
-- token if available. Returns 1 if the request is allowed, 0 if the bucket is empty.
-- The refill timestamp is always updated regardless of whether the request is allowed,
-- to ensure accurate token accumulation between calls.
--
-- KEYS[1] - rate limit key (hash with fields: tokens, last_refill_ms)
-- ARGV[1] - bucket capacity (maximum number of tokens)
-- ARGV[2] - refill rate (tokens per second)

local time = redis.call('TIME')

local now_ms = time[1] * 1000 + math.floor(time[2]/1000)

local token_count = tonumber(redis.call('HGET', KEYS[1], 'tokens')) or tonumber(ARGV[1])
local last_refill_ms = tonumber(redis.call('HGET', KEYS[1], 'last_refill_ms')) or now_ms

local num_token_refill = tonumber(ARGV[2]) * (now_ms - last_refill_ms) / 1000

token_count = math.min(token_count+num_token_refill, tonumber(ARGV[1]))

redis.call("HSET", KEYS[1], 'last_refill_ms', now_ms)

if token_count >= 1 then
    redis.call("HSET", KEYS[1], 'tokens', token_count - 1)
    return 1
else
    return 0
end