// Package limigo wires process-level dependencies for the Limigo service.
package limigo

import (
	"os"

	goredis "github.com/redis/go-redis/v9"
)

// redisAddr contains the Redis endpoint configured for the process.
var redisAddr = os.Getenv("REDIS_ADDR")

// redisClient is the process-wide Redis client used by Limigo.
var redisClient = goredis.NewClient(&goredis.Options{
	Addr: redisAddr,
})
