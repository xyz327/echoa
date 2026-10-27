package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

const (
	SourceKindMySQL = "mysql"
	SourceKindRedis = "redis"
)

type Config struct {
	Server    ServerConfig     `mapstructure:"server"`
	Auth      AuthConfig       `mapstructure:"auth"`
	Sources   SourcesConfig    `mapstructure:"sources"`
	Endpoints []EndpointConfig `mapstructure:"endpoints"`
}

type ServerConfig struct {
	Port                    int      `mapstructure:"port"`
	ReadTimeoutMS           int      `mapstructure:"read_timeout_ms"`
	AllowedOrigins          []string `mapstructure:"allowed_origins"`
	MaxInflightPerPrincipal int      `mapstructure:"max_inflight_per_principal"`
}

type AuthConfig struct {
	Enabled          bool     `mapstructure:"enabled"`
	ResourceURL      string   `mapstructure:"resource_url"` // MCP 服务端自身对外访问 URL (如 https://mcp.example.com)
	Endpoint         string   `mapstructure:"endpoint"`     // Casdoor 地址 (如 https://casdoor.example.com)
	ClientID         string   `mapstructure:"client_id"`
	ClientSecret     string   `mapstructure:"client_secret"`
	Certificate      string   `mapstructure:"certificate"`
	Audience         string   `mapstructure:"audience"` // 预期 JWT Audience (aud)，留空默认使用 client_id
	JWKSURI          string   `mapstructure:"jwks_uri"` // 可选 JWKS 端点地址，留空默认推导为 <endpoint>/.well-known/jwks
	OrganizationName string   `mapstructure:"organization_name"`
	ApplicationName  string   `mapstructure:"application_name"`
	ScopesRequired   []string `mapstructure:"scopes_required"`
}

// SourcesConfig 按种类挂可命名的数据源。名字在全部种类之间唯一，endpoint 用这个名字引用。
type SourcesConfig struct {
	MySQL map[string]MySQLConfig `mapstructure:"mysql"`
	Redis map[string]RedisConfig `mapstructure:"redis"`
}

// EndpointConfig 把一组 source 暴露成一个独立的 MCP 端点。
// Path 为 /mysql1 时，客户端通过 POST /mysql1 进行 MCP 消息交互。
type EndpointConfig struct {
	Path    string   `mapstructure:"path"`
	Sources []string `mapstructure:"sources"`
}

// SourceRef 是解析后的数据源引用：名字、种类，以及对应种类的那一份配置。
type SourceRef struct {
	Name  string
	Kind  string
	MySQL MySQLConfig
	Redis RedisConfig
}

type MySQLSecurityConfig struct {
	MaxLimit       int      `mapstructure:"max_limit"`
	TableBlacklist []string `mapstructure:"table_blacklist"`
}

type MySQLConfig struct {
	DSN            string              `mapstructure:"dsn"`
	MaxOpenConns   int                 `mapstructure:"max_open_conns"`
	MaxIdleConns   int                 `mapstructure:"max_idle_conns"`
	QueryTimeoutMS int                 `mapstructure:"query_timeout_ms"`
	Security       MySQLSecurityConfig `mapstructure:"security"`
}

type RedisSecurityConfig struct {
	AllowedCommands []string `mapstructure:"allowed_commands"`
	BlockedCommands []string `mapstructure:"blocked_commands"`
}

type RedisConfig struct {
	Addr           string              `mapstructure:"addr"`
	Username       string              `mapstructure:"username"`
	Password       string              `mapstructure:"password"`
	AllowedDBs     []int               `mapstructure:"allowed_dbs"`
	PoolSize       int                 `mapstructure:"pool_size"`
	MinIdleConns   int                 `mapstructure:"min_idle_conns"`
	MaxItemsLimit  int                 `mapstructure:"max_items_limit"`
	QueryTimeoutMS int                 `mapstructure:"query_timeout_ms"`
	Security       RedisSecurityConfig `mapstructure:"security"`
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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Lookup 按名字查数据源。名字在 mysql 与 redis 之间共用一个命名空间。
func (c *Config) Lookup(name string) (SourceRef, bool) {
	if cfg, ok := c.Sources.MySQL[name]; ok {
		return SourceRef{Name: name, Kind: SourceKindMySQL, MySQL: cfg}, true
	}
	if cfg, ok := c.Sources.Redis[name]; ok {
		return SourceRef{Name: name, Kind: SourceKindRedis, Redis: cfg}, true
	}
	return SourceRef{}, false
}

func (c *Config) Validate() error {
	seen := map[string]string{}
	for name := range c.Sources.MySQL {
		if err := validateSourceName(name); err != nil {
			return fmt.Errorf("sources.mysql: %w", err)
		}
		seen[name] = SourceKindMySQL
	}
	for name := range c.Sources.Redis {
		if err := validateSourceName(name); err != nil {
			return fmt.Errorf("sources.redis: %w", err)
		}
		if kind, dup := seen[name]; dup {
			return fmt.Errorf("source name %q is declared as both %s and redis", name, kind)
		}
		seen[name] = SourceKindRedis
	}

	if len(c.Endpoints) == 0 {
		return fmt.Errorf("endpoints: at least one endpoint is required")
	}

	paths := map[string]bool{}
	for i, ep := range c.Endpoints {
		if !singleSegmentPath(ep.Path) {
			return fmt.Errorf("endpoints[%d]: path %q must be a single segment like /mysql1", i, ep.Path)
		}
		if ep.Path == "/health" || strings.HasPrefix(ep.Path, "/.") {
			return fmt.Errorf("endpoints[%d]: path %q is reserved", i, ep.Path)
		}
		if paths[ep.Path] {
			return fmt.Errorf("endpoints[%d]: duplicate path %q", i, ep.Path)
		}
		paths[ep.Path] = true

		if len(ep.Sources) == 0 {
			return fmt.Errorf("endpoints[%d] %s: sources must not be empty", i, ep.Path)
		}
		used := map[string]bool{}
		for _, name := range ep.Sources {
			if _, ok := c.Lookup(name); !ok {
				return fmt.Errorf("endpoints[%d] %s: unknown source %q", i, ep.Path, name)
			}
			if used[name] {
				return fmt.Errorf("endpoints[%d] %s: source %q listed twice", i, ep.Path, name)
			}
			used[name] = true
		}
	}
	return nil
}

// singleSegmentPath 允许 /mysql1 这样的单段路径，拒绝根路径、尾部斜杠和多段路径。
func singleSegmentPath(path string) bool {
	if len(path) < 2 || path[0] != '/' || strings.HasSuffix(path, "/") {
		return false
	}
	return strings.Count(path, "/") == 1
}

var sourceNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

func validateSourceName(name string) error {
	if !sourceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid source name %q (want [a-zA-Z][a-zA-Z0-9_-]{0,63})", name)
	}
	return nil
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
	if cfg.Server.MaxInflightPerPrincipal <= 0 {
		cfg.Server.MaxInflightPerPrincipal = 5
	}
	cfg.Auth.ResourceURL = expandEnvWithDefault(cfg.Auth.ResourceURL)
	cfg.Auth.Endpoint = expandEnvWithDefault(cfg.Auth.Endpoint)
	cfg.Auth.ClientSecret = expandEnvWithDefault(cfg.Auth.ClientSecret)
	cfg.Auth.Certificate = expandEnvWithDefault(cfg.Auth.Certificate)
	cfg.Auth.Audience = expandEnvWithDefault(cfg.Auth.Audience)
	cfg.Auth.JWKSURI = expandEnvWithDefault(cfg.Auth.JWKSURI)

	for name, src := range cfg.Sources.MySQL {
		src.DSN = expandEnvWithDefault(src.DSN)
		if src.Security.MaxLimit <= 0 {
			src.Security.MaxLimit = 200
		}
		if src.QueryTimeoutMS <= 0 {
			src.QueryTimeoutMS = 5000
		}
		if src.MaxOpenConns <= 0 {
			src.MaxOpenConns = 20
		}
		if src.MaxIdleConns <= 0 {
			src.MaxIdleConns = 5
		}
		cfg.Sources.MySQL[name] = src
	}

	for name, src := range cfg.Sources.Redis {
		src.Addr = expandEnvWithDefault(src.Addr)
		src.Username = expandEnvWithDefault(src.Username)
		src.Password = expandEnvWithDefault(src.Password)
		if src.MaxItemsLimit <= 0 {
			src.MaxItemsLimit = 200
		}
		if src.QueryTimeoutMS <= 0 {
			src.QueryTimeoutMS = 3000
		}
		if src.PoolSize <= 0 {
			src.PoolSize = 20
		}
		if src.MinIdleConns <= 0 {
			src.MinIdleConns = 5
		}
		cfg.Sources.Redis[name] = src
	}

	if len(cfg.Auth.ScopesRequired) == 0 {
		cfg.Auth.ScopesRequired = []string{"openid", "offline_access"}
	}
}
