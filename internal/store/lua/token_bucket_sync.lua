-- Token bucket rate limiter — batched delta sync.
-- Reconciles a batch of already-admitted local requests with the authoritative
-- bucket state in Redis. Refills tokens based on elapsed time exactly like
-- token_bucket.lua, then subtracts delta tokens already consumed by the local
-- node's optimistic admits. Unlike token_bucket.lua, this script does not
-- decide allow/deny — the local node already made that decision before this
-- script runs. This script only keeps Redis's authoritative token count in
-- sync and reports the resulting count so the calling node can recalibrate
-- its local baseline for the next flush interval. The result can be negative,
-- which signals the local node over-admitted relative to the true bucket
-- state during the accuracy window between syncs.
--
-- KEYS[1] - rate limit key (hash with fields: tokens, last_refill_ms)
-- ARGV[1] - bucket capacity (maximum number of tokens)
-- ARGV[2] - refill rate (tokens per second)
-- ARGV[3] - delta (number of tokens already consumed locally since the last sync)
local t0 = redis.call('TIME')

local capacity = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local delta = tonumber(ARGV[3])

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
token_count = token_count - delta

-- Keep idle buckets long enough to refill fully while bounding Redis memory growth.
local full_refill_ms = (capacity / refill_rate) * 1000
local ttl_ms = math.floor(full_refill_ms * 2)
ttl_ms = math.max(ttl_ms, 60000)
ttl_ms = math.min(ttl_ms, 86400000)

redis.call('HSET', KEYS[1], 'last_refill_ms', now_ms)
redis.call('HSET', KEYS[1], 'tokens', token_count)
redis.call('PEXPIRE', KEYS[1], ttl_ms)

local t1 = redis.call('TIME')
local lua_us = (t1[1] - t0[1]) * 1000000 + (t1[2] - t0[2])

-- Returned as a string: Redis truncates Lua numbers to integers on return,
-- which would silently drop the fractional part of a partially-refilled or
-- partially-consumed token count.
return {tostring(token_count), lua_us}
