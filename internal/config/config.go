package config

import (
	"fmt"

	"github.com/spf13/viper"
)

// Config holds all application configuration.
type Config struct {
	LLM       LLMConfig       `mapstructure:"llm"`
	DB        DBConfig        `mapstructure:"db"`
	Redis     RedisConfig     `mapstructure:"redis"`
	Workspace WorkspaceConfig `mapstructure:"workspace"`
}

type LLMConfig struct {
	Endpoint  string `mapstructure:"endpoint"`
	APIKey    string `mapstructure:"api_key"`
	Model     string `mapstructure:"model"`
	MaxTokens int    `mapstructure:"max_tokens"`
}

type DBConfig struct {
	DSN string `mapstructure:"dsn"`
}

type RedisConfig struct {
	Addr string `mapstructure:"addr"`
}

type WorkspaceConfig struct {
	Dir string `mapstructure:"dir"`
}

// Load reads configuration from file and environment.
// It looks for shirakami.yaml in the current directory, $HOME, or /etc/shirakami/.
//
// Environment variables (prefixed with SHIRAKAMI_) override file values:
//
//	SHIRAKAMI_LLM_API_KEY   → llm.api_key
//	SHIRAKAMI_LLM_ENDPOINT  → llm.endpoint
//	SHIRAKAMI_LLM_MODEL     → llm.model
//	SHIRAKAMI_DB_DSN        → db.dsn
//	SHIRAKAMI_REDIS_ADDR    → redis.addr
func Load(cfgFile string) (*Config, error) {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("shirakami")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("$HOME/.shirakami")
		viper.AddConfigPath("/etc/shirakami")
	}

	viper.SetEnvPrefix("SHIRAKAMI")
	viper.AutomaticEnv()

	// Explicit env → config key bindings for nested fields.
	_ = viper.BindEnv("llm.api_key", "SHIRAKAMI_LLM_API_KEY")
	_ = viper.BindEnv("llm.endpoint", "SHIRAKAMI_LLM_ENDPOINT")
	_ = viper.BindEnv("llm.model", "SHIRAKAMI_LLM_MODEL")
	_ = viper.BindEnv("db.dsn", "SHIRAKAMI_DB_DSN")
	_ = viper.BindEnv("redis.addr", "SHIRAKAMI_REDIS_ADDR")

	// defaults
	viper.SetDefault("workspace.dir", "/tmp/shirakami-workspace")
	viper.SetDefault("redis.addr", "localhost:6379")
	viper.SetDefault("llm.model", "gpt-4o")
	viper.SetDefault("llm.endpoint", "https://api.openai.com/v1")
	viper.SetDefault("llm.max_tokens", 128000)

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks that required fields are set.
func validate(cfg *Config) error {
	if cfg.LLM.APIKey == "" {
		return fmt.Errorf("config: llm.api_key is required (or set SHIRAKAMI_LLM_API_KEY)")
	}
	if cfg.DB.DSN == "" {
		return fmt.Errorf("config: db.dsn is required (or set SHIRAKAMI_DB_DSN)")
	}
	return nil
}
