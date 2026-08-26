package main

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type configEnv struct {
	Domains          []string `env:"DOMAIN" envSeparator:"," envDefault:"debuginfod.pwndbg.re,releases.pwndbg.re"`
	LetsencryptEmail string   `env:"LETSENCRYPT_EMAIL" envDefault:"patryk.sondej@gmail.com"`
	ListenPort       int      `env:"LISTEN_PORT" envDefault:"8031"`
	ListenIP         string   `env:"LISTEN_IP" envDefault:"127.0.0.1"`
	CertCachePath    string   `env:"CERT_CACHE_PATH" envDefault:"./cert-cache"`
	ClickhouseDSN    string   `env:"CLICKHOUSE_DSN" envDefault:"clickhouse://127.0.0.1:9000?username=cypis&password=cypis"`
	LogLevel         string   `env:"LOG_LEVEL" envDefault:"info"`

	// On-disk file cache. CACHE_ENABLED=false turns it off entirely (the proxy
	// then streams straight from upstream). It is needed as the kill switch
	// because an empty CACHE_PATH is NOT one: env/v11 falls back to envDefault
	// on an empty value, so the path variable alone disables nothing.
	CacheEnabled  bool   `env:"CACHE_ENABLED" envDefault:"true"`
	CachePath     string `env:"CACHE_PATH" envDefault:"./cache"`
	CacheMaxBytes int64  `env:"CACHE_MAX_BYTES" envDefault:"53687091200"` // 50 GiB

	// Measures CACHE_PATH usage and free space on its partition into
	// cache_stats. Paced with eviction - measuring more often buys nothing,
	// since the scan walks the whole directory tree. It also runs when
	// CACHE_MAX_BYTES <= 0: eviction is off then, which is exactly when knowing
	// how fast the directory grows matters most.
	CacheStatsInterval time.Duration `env:"CACHE_STATS_INTERVAL" envDefault:"10m"`

	// The /stats page. Its queries scan the whole access_log range, so they run
	// once per StatsInterval in the background and the handler serves finished
	// HTML from memory. Refreshing more often buys nothing: the data is bucketed
	// by day.
	StatsEnabled  bool          `env:"STATS_ENABLED" envDefault:"true"`
	StatsDays     int           `env:"STATS_DAYS" envDefault:"360"`
	StatsInterval time.Duration `env:"STATS_INTERVAL" envDefault:"1h"`
}

var Config configEnv

func ParseConfig() error {
	return env.Parse(&Config)
}
