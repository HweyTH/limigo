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
-- a tag would pin all of them to one slot. Redis 7.4+: the script times
-- itself with os.clock(), because TIME is frozen for the whole execution.
-- See README "Single-key Lua scripts".
--
-- KEYS[1] - rate limit key
-- ARGV[1] - maximum number of requests allowed per window
-- ARGV[2] - window duration in milliseconds
-- ARGV[3] - optional; 'peek' reports the state a request arriving now would
--           see without recording one (the Admin API's quota inspection).
--           The same script answers both so the two cannot drift apart.

local t0 = os.clock()

local limit = tonumber(ARGV[1])
local peek = ARGV[3] == 'peek'

local count
if peek then
    count = tonumber(redis.call('GET', KEYS[1])) or 0
else
    count = redis.call('INCR', KEYS[1])
    if count == 1 then
        redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
    end
end

-- On the write path count includes this request, so it is allowed while
-- count <= limit; a peek asks whether one more would fit, count < limit.
-- Either way remaining is the admits still possible after what count holds.
local allowed = 0
if (peek and count < limit) or (not peek and count <= limit) then
    allowed = 1
end
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

local lua_us = math.floor((os.clock() - t0) * 1000000)

return {allowed, lua_us, remaining, reset_ms}
