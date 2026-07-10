-- Fixed window rate limiter — batched delta sync.
-- Reconciles a batch of already-admitted local requests with the authoritative
-- counter in Redis. Unlike fixed_window.lua, this script does not decide
-- allow/deny — the local node already made that decision optimistically
-- before this script runs. This script only folds the batch into Redis's
-- authoritative count and reports the resulting total so the calling node can
-- recalibrate its local baseline for the next flush interval.
--
-- KEYS[1] - rate limit key
-- ARGV[1] - delta (number of requests already admitted locally since the last sync)
-- ARGV[2] - window duration in milliseconds

local delta = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])

if delta <= 0 then
    local current = tonumber(redis.call('GET', KEYS[1])) or 0
    return current
end

local count = redis.call('INCRBY', KEYS[1], delta)

if count == delta then
    redis.call('PEXPIRE', KEYS[1], window_ms)
end

return count
