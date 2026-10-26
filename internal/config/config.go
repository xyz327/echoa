package config

import (
	"os"
	"regexp"

	"github.com/spf13/viper"
)

type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Auth        AuthConfig        `mapstructure:"auth"`
	DataSources DataSourcesConfig `mapstructure:"datasources"`
}

type ServerConfig struct {
	Port          int `mapstructure:"port"`
	ReadTimeoutMS int `mapstructure:"read_timeout_ms"`
}

type AuthConfig struct {
	Enabled          bool     `mapstructure:"enabled"`
	ResourceURL      string   `mapstructure:"resource_url"` // MCP 服务端自身对外访问 URL (如 https://mcp.example.com)
	Endpoint         string   `mapstructure:"endpoint"`     // Casdoor 地址 (如 https://casdoor.example.com)
	ClientID         string   `mapstructure:"client_id"`
	ClientSecret     string   `mapstructure:"client_secret"`
	Certificate      string   `mapstructure:"certificate"`
	OrganizationName string   `mapstructure:"organization_name"`
	ApplicationName  string   `mapstructure:"application_name"`
	ScopesRequired   []string `mapstructure:"scopes_required"`
}

type DataSourcesConfig struct {
	MySQL MySQLConfig `mapstructure:"mysql"`
	Redis RedisConfig `mapstructure:"redis"`
}

type MySQLConfig struct {
	Enabled        bool   `mapstructure:"enabled"`
	DSN            string `mapstructure:"dsn"`
	MaxOpenConns   int    `mapstructure:"max_open_conns"`
	MaxIdleConns   int    `mapstructure:"max_idle_conns"`
	QueryTimeoutMS int    `mapstructure:"query_timeout_ms"`
	Security       struct {
		MaxLimit       int      `mapstructure:"max_limit"`
		TableBlacklist []string `mapstructure:"table_blacklist"`
	} `mapstructure:"security"`
}

type RedisConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	Addr           string   `mapstructure:"addr"`
	Password       string   `mapstructure:"password"`
	AllowedDBs     []int    `mapstructure:"allowed_dbs"`
	PoolSize       int      `mapstructure:"pool_size"`
	MinIdleConns   int      `mapstructure:"min_idle_conns"`
	MaxItemsLimit  int      `mapstructure:"max_items_limit"`
	QueryTimeoutMS int      `mapstructure:"query_timeout_ms"`
	Security       struct {
		AllowedCommands []string `mapstructure:"allowed_commands"`
		BlockedCommands []string `mapstructure:"blocked_commands"`
	} `mapstructure:"security"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	expandEnvs(&cfg)
	return &cfg, nil
}

var envPattern = regexp.MustCompile(`\$\{([a-zA-Z0-9_]+)(?::-([^}]*))?\}`)

func expandEnvWithDefault(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(m string) string {
		match := envPattern.FindStringSubmatch(m)
		if len(match) < 2 {
			return m
		}
		key := match[1]
		val, exists := os.LookupEnv(key)
		if exists && val != "" {
			return val
		}
		if len(match) >= 3 {
			return match[2] // default value
		}
		return ""
	})
}

func expandEnvs(cfg *Config) {
	cfg.Auth.ResourceURL = expandEnvWithDefault(cfg.Auth.ResourceURL)
	cfg.Auth.Endpoint = expandEnvWithDefault(cfg.Auth.Endpoint)
	cfg.Auth.ClientSecret = expandEnvWithDefault(cfg.Auth.ClientSecret)
	cfg.Auth.Certificate = expandEnvWithDefault(cfg.Auth.Certificate)
	cfg.DataSources.MySQL.DSN = expandEnvWithDefault(cfg.DataSources.MySQL.DSN)
	cfg.DataSources.Redis.Addr = expandEnvWithDefault(cfg.DataSources.Redis.Addr)
	cfg.DataSources.Redis.Password = expandEnvWithDefault(cfg.DataSources.Redis.Password)
	if cfg.DataSources.MySQL.Security.MaxLimit <= 0 {
		cfg.DataSources.MySQL.Security.MaxLimit = 200
	}
	if cfg.DataSources.MySQL.QueryTimeoutMS <= 0 {
		cfg.DataSources.MySQL.QueryTimeoutMS = 5000
	}
	if cfg.DataSources.Redis.MaxItemsLimit <= 0 {
		cfg.DataSources.Redis.MaxItemsLimit = 200
	}
	if cfg.DataSources.Redis.QueryTimeoutMS <= 0 {
		cfg.DataSources.Redis.QueryTimeoutMS = 3000
	}
	if cfg.DataSources.Redis.PoolSize <= 0 {
		cfg.DataSources.Redis.PoolSize = 20
	}
	if cfg.DataSources.Redis.MinIdleConns <= 0 {
		cfg.DataSources.Redis.MinIdleConns = 5
	}
	if len(cfg.Auth.ScopesRequired) == 0 {
		cfg.Auth.ScopesRequired = []string{"openid", "offline_access"}
	}
}
