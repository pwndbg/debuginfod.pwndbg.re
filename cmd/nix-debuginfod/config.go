package main

import "github.com/caarlos0/env/v11"

type configEnv struct {
	ListenAddr string `env:"LISTEN_ADDR" envDefault:"127.0.0.1:8034"`
	LogLevel   string `env:"LOG_LEVEL" envDefault:"info"`
	// TLSEnabled serves HTTPS with a certificate generated at startup. On by
	// default because Go's HTTP/2 server only runs over TLS, and HTTP/2 is what
	// carries 103 Early Hints. Turn it off to debug with plain curl.
	TLSEnabled    bool   `env:"TLS_ENABLED" envDefault:"true"`
	ClickhouseDSN string `env:"CLICKHOUSE_DSN" envDefault:"clickhouse://127.0.0.1:9000?username=cypis&password=cypis"`

	// ImagePath holds the .erofs files. MountRoot is where store paths are
	// mounted and must be the real /nix/store: absolute symlinks between store
	// paths only resolve if the target sits where it says it does.
	ImagePath string `env:"IMAGE_PATH" envDefault:"/var/lib/cache/nix-images"`
	MountRoot string `env:"MOUNT_ROOT" envDefault:"/nix/store"`
	// EntryMountPath takes the per-build-ID images, which need no canonical name
	// because nothing ever links into one.
	EntryMountPath string `env:"ENTRY_MOUNT_PATH" envDefault:"/var/lib/cache/nix-entry"`
	// MaxFetches bounds how many distinct store paths are downloaded and packed
	// into erofs at once. Each one is a NAR download plus an mkfs.erofs process.
	MaxFetches int `env:"MAX_FETCHES" envDefault:"4"`
}

var Config configEnv

func ParseConfig() error {
	return env.Parse(&Config)
}
