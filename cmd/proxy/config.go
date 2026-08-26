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
	// Client classification from api.github.com/meta. Refreshed daily: GitHub
	// republishes the document continuously, but the actions ranges are Azure
	// allocations that move on the scale of weeks, so a shorter period costs
	// requests and changes nothing.
	//
	// Turning this off means nothing is ever classified: every request is
	// logged as "unclassified" and /stats counts all of it, CI included. There
	// is no second code path - the filter in the queries is unconditional and
	// simply finds no github_actions tags to drop.
	GHRangesEnabled  bool          `env:"GH_RANGES_ENABLED" envDefault:"true"`
	GHRangesInterval time.Duration `env:"GH_RANGES_INTERVAL" envDefault:"24h"`
	// Which api.github.com/meta keys to treat as CI. actions_macos is listed
	// separately by GitHub and is not included by default - it is a handful of
	// prefixes and macOS runners are not what floods a debuginfod.
	GHRangesServices []string `env:"GH_RANGES_SERVICES" envSeparator:"," envDefault:"actions"`

	StatsEnabled  bool          `env:"STATS_ENABLED" envDefault:"true"`
	StatsDays     int           `env:"STATS_DAYS" envDefault:"360"`
	StatsInterval time.Duration `env:"STATS_INTERVAL" envDefault:"1h"`
}

var Config configEnv

func ParseConfig() error {
	return env.Parse(&Config)
}
