package main

import (
	"io"
	"reflect"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

// TestParseOptionsRedisTarget pins how the standalone and cluster address
// inputs combine: a cluster list selects the cluster client, and giving it
// alongside an explicit standalone address is an error rather than a silent
// preference for one of them.
func TestParseOptionsRedisTarget(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		wantAddr     string
		wantCluster  []string
		wantErrMatch string
	}{
		{
			name:     "defaults to standalone localhost",
			wantAddr: "localhost:6379",
		},
		{
			name:        "cluster flag parses a comma list, trimmed, blanks dropped",
			args:        []string{"-redis-cluster-addrs", " a:7000, b:7001,,c:7002 ,"},
			wantAddr:    "localhost:6379",
			wantCluster: []string{"a:7000", "b:7001", "c:7002"},
		},
		{
			name:        "cluster env is honoured",
			env:         map[string]string{"LIMIGO_REDIS_CLUSTER_ADDRS": "a:7000,b:7001"},
			wantAddr:    "localhost:6379",
			wantCluster: []string{"a:7000", "b:7001"},
		},
		{
			name:         "cluster flag with explicit redis-addr flag is rejected",
			args:         []string{"-redis-addr", "x:6379", "-redis-cluster-addrs", "a:7000"},
			wantErrMatch: "mutually exclusive",
		},
		{
			name:         "cluster flag with REDIS_ADDR env is rejected",
			args:         []string{"-redis-cluster-addrs", "a:7000"},
			env:          map[string]string{"REDIS_ADDR": "x:6379"},
			wantErrMatch: "mutually exclusive",
		},
		{
			name:     "blank cluster list means standalone",
			args:     []string{"-redis-cluster-addrs", " , "},
			wantAddr: "localhost:6379",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			opts, err := parseOptions(tt.args, getenv, io.Discard)
			if tt.wantErrMatch != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMatch) {
					t.Fatalf("parseOptions error = %v, want one containing %q", err, tt.wantErrMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOptions: %v", err)
			}
			if opts.redisAddr != tt.wantAddr {
				t.Errorf("redisAddr = %q, want %q", opts.redisAddr, tt.wantAddr)
			}
			if !reflect.DeepEqual(opts.redisClusterAddrs, tt.wantCluster) {
				t.Errorf("redisClusterAddrs = %v, want %v", opts.redisClusterAddrs, tt.wantCluster)
			}
		})
	}
}

// TestNewRedisClientSelectsType is the reason the store takes a
// redis.Scripter: with a cluster list the runtime must hand it a
// *redis.ClusterClient, and without one a standalone *redis.Client. Neither
// is connected to here.
func TestNewRedisClientSelectsType(t *testing.T) {
	t.Run("standalone", func(t *testing.T) {
		client, target := newRedisClient(options{redisAddr: "localhost:6379"})
		t.Cleanup(func() { _ = client.Close() })
		if _, ok := client.(*goredis.Client); !ok {
			t.Fatalf("client type = %T, want *redis.Client", client)
		}
		if target != "address localhost:6379" {
			t.Errorf("target = %q", target)
		}
	})
	t.Run("cluster", func(t *testing.T) {
		client, target := newRedisClient(options{redisAddr: "localhost:6379", redisClusterAddrs: []string{"a:7000", "b:7001"}})
		t.Cleanup(func() { _ = client.Close() })
		if _, ok := client.(*goredis.ClusterClient); !ok {
			t.Fatalf("client type = %T, want *redis.ClusterClient", client)
		}
		if target != "cluster a:7000,b:7001" {
			t.Errorf("target = %q", target)
		}
	})
}
