package limigo

import (
	"os"

	goredis "github.com/redis/go-redis/v9"
)

var redisAddr = os.Getenv("REDIS_ADDR")

var redisClient = goredis.NewClient(&goredis.Options{
	Addr: redisAddr,
})
