-- Leaky bucket rate limiter (GCRA — Generic Cell Rate Algorithm).
-- Admits requests on a steady schedule instead of allowing bursts up to a
-- capacity: each key tracks a single Theoretical Arrival Time (TAT), the
-- earliest time the next request is due under perfectly smooth spacing.
-- A request is allowed if it arrives no earlier than tolerance milliseconds
-- before its TAT; allowing it advances the TAT by one emission interval.
-- Denied requests do not advance the schedule, so a rejected burst does not
-- push future requests further out than the configured rate allows.
--
-- Returns a two-element array {allowed, retry_after_ms}. retry_after_ms is 0
-- when allowed, or the exact number of milliseconds (rounded up) until this
-- key's schedule will next admit a request when denied — GCRA already knows
-- this precisely, so callers get an exact backoff instead of guessing.
--
-- KEYS[1] - rate limit key (string holding the TAT in fractional milliseconds)
-- ARGV[1] - emission interval in milliseconds (window / limit)
-- ARGV[2] - tolerance in milliseconds (emission_interval * (burst - 1))

local emission_interval_ms = tonumber(ARGV[1])
local tolerance_ms = tonumber(ARGV[2])

if emission_interval_ms <= 0 then
    return redis.error_reply('emission_interval_ms must be greater than zero')
end

if tolerance_ms < 0 then
    return redis.error_reply('tolerance_ms must not be negative')
end

local time = redis.call('TIME')
local now_ms = time[1] * 1000 + time[2] / 1000

local tat = tonumber(redis.call('GET', KEYS[1])) or now_ms
if tat < now_ms then
    tat = now_ms
end

local allow_at = tat - tolerance_ms

if now_ms >= allow_at then
    local new_tat = tat + emission_interval_ms
    local ttl_ms = math.max(math.ceil(new_tat - now_ms), 1)
    redis.call('SET', KEYS[1], tostring(new_tat), 'PX', ttl_ms)
    return {1, 0}
end

return {0, math.ceil(allow_at - now_ms)}
