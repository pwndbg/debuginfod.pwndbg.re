package main

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type configEnv struct {
	ClickhouseDSN string `env:"CLICKHOUSE_DSN" envDefault:"clickhouse://127.0.0.1:9000?username=cypis&password=cypis"`
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`

	StatsEnabled bool `env:"STATS_ENABLED" envDefault:"true"`
	// StatsDays is the longest window collected. The switcher offers every entry
	// of statsViewLengths that fits inside it.
	StatsDays     int           `env:"STATS_DAYS" envDefault:"360"`
	StatsInterval time.Duration `env:"STATS_INTERVAL" envDefault:"1h"`
}

var Config configEnv

func ParseConfig() error {
	return env.Parse(&Config)
}
