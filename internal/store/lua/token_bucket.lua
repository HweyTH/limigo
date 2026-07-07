-- Token bucket rate limiter.
-- Refills tokens based on elapsed time since the last request, then consumes one
-- token if available. Returns 1 if the request is allowed, 0 if the bucket is empty.
-- The refill timestamp is always updated regardless of whether the request is allowed,
-- to ensure accurate token accumulation between calls.
--
-- KEYS[1] - rate limit key (hash with fields: tokens, last_refill_ms)
-- ARGV[1] - bucket capacity (maximum number of tokens)
-- ARGV[2] - refill rate (tokens per second)

local capacity = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])

if capacity <= 0 then
    return redis.error_reply('capacity must be greater than zero')
end

if refill_rate <= 0 then
    return redis.error_reply('refill_rate must be greater than zero')
end

local time = redis.call('TIME')
local now_ms = time[1] * 1000 + math.floor(time[2] / 1000)

local token_count = tonumber(redis.call('HGET', KEYS[1], 'tokens')) or capacity
local last_refill_ms = tonumber(redis.call('HGET', KEYS[1], 'last_refill_ms')) or now_ms

local tokens_to_refill = refill_rate * (now_ms - last_refill_ms) / 1000
token_count = math.min(token_count + tokens_to_refill, capacity)

local full_refill_ms = (capacity / refill_rate) * 1000
local ttl_ms = math.floor(full_refill_ms * 2)
ttl_ms = math.max(ttl_ms, 60000)
ttl_ms = math.min(ttl_ms, 86400000)

local allowed = 0

redis.call('HSET', KEYS[1], 'last_refill_ms', now_ms)

if token_count >= 1 then
    redis.call('HSET', KEYS[1], 'tokens', token_count - 1)
    allowed = 1
else
    redis.call('HSET', KEYS[1], 'tokens', token_count)
end

redis.call('PEXPIRE', KEYS[1], ttl_ms)

return allowed