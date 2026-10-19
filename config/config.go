package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Collector CollectorConfig `mapstructure:"collector"`
	Network   NetworkConfig   `mapstructure:"network"`
	Server    ServerConfig    `mapstructure:"server"`
	Storage   StorageConfig   `mapstructure:"storage"`
}

type CollectorConfig struct {
	// StreamInterval is nettop's update period in seconds. Shorter means more
	// connections are seen while still open, so fewer bytes end up "unclassified".
	StreamInterval int `mapstructure:"stream_interval"`
	// FlushInterval is how often aggregated rows are written, in seconds.
	FlushInterval   int      `mapstructure:"flush_interval"`
	IgnoreProcesses []string `mapstructure:"ignore_processes"`
	// LookupASN resolves the owner of internet addresses (PTR + Team Cymru
	// DNS). Turn off to send no lookups at all.
	LookupASN bool `mapstructure:"lookup_asn"`
}

// NetworkConfig classifies remote addresses. Anything matching none of the
// lists is "internet".
type NetworkConfig struct {
	LAN     []string `mapstructure:"lan"`
	VPN     []string `mapstructure:"vpn"`
	Tailnet []string `mapstructure:"tailnet"`
}

type ServerConfig struct {
	Port int    `mapstructure:"port"`
	Host string `mapstructure:"host"`
}

type StorageConfig struct {
	Path      string          `mapstructure:"path"`
	Retention RetentionConfig `mapstructure:"retention"`
}

type RetentionConfig struct {
	MinuteDays   int `mapstructure:"minute_days"`
	EndpointDays int `mapstructure:"endpoint_days"`
}

func Default() *Config {
	base := baseDir()
	return &Config{
		Collector: CollectorConfig{
			StreamInterval:  5,
			FlushInterval:   60,
			IgnoreProcesses: []string{"kernel_task"},
			LookupASN:       true,
		},
		Network: NetworkConfig{
			LAN: []string{
				"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
				"224.0.0.0/4", "255.255.255.255/32",
				"fc00::/7", "fe80::/10", "ff00::/8",
			},
			VPN:     []string{},
			Tailnet: []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"},
		},
		Server: ServerConfig{
			Port: 8099,
			Host: "127.0.0.1",
		},
		Storage: StorageConfig{
			Path: filepath.Join(base, "data", "netwatch.db"),
			Retention: RetentionConfig{
				MinuteDays:   14,
				EndpointDays: 180,
			},
		},
	}
}

func baseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
