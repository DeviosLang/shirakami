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
	Contracts []ContractEntry `mapstructure:"contracts"`
	Webhook   WebhookConfig   `mapstructure:"webhook"`
}

// ContractEntry declares a known cross-repo call relationship.
// These are injected into Worker prompts as hints so the LLM does not need
// to discover them via ripgrep (faster, more reliable for known relationships).
type ContractEntry struct {
	Provider ContractEndpoint `mapstructure:"provider"`
	Consumer ContractEndpoint `mapstructure:"consumer"`
}

// ContractEndpoint identifies one side of a cross-repo contract.
type ContractEndpoint struct {
	Repo string `mapstructure:"repo"` // repo short name (matches workspace.repos[].name)
	Path string `mapstructure:"path"` // HTTP path, gRPC method, or MQ topic
	Func string `mapstructure:"func"` // function/handler name (optional, for display)
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
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
}

type RepoConfig struct {
	Name   string `mapstructure:"name"`
	URL    string `mapstructure:"url"`
	Branch string `mapstructure:"branch"`
	Role   string `mapstructure:"role"`
}

type WorkspaceConfig struct {
	Dir   string       `mapstructure:"dir"`
	Repos []RepoConfig `mapstructure:"repos"`
}

// WebhookConfig holds GitLab/GitHub webhook settings.
type WebhookConfig struct {
	// Secret is the shared HMAC secret used to verify incoming webhook payloads.
	// For GitLab: compared against X-Gitlab-Token header (plain text).
	// For GitHub: used to verify X-Hub-Signature-256 (HMAC-SHA256).
	Secret string `mapstructure:"secret"`

	// CommentOnMR enables posting analysis results as MR/PR comments.
	CommentOnMR bool `mapstructure:"comment_on_mr"`

	// GitLabToken is the GitLab personal access token used to post comments.
	GitLabToken string `mapstructure:"gitlab_token"`

	// GitHubToken is the GitHub personal access token used to post PR comments.
	GitHubToken string `mapstructure:"github_token"`
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
	_ = viper.BindEnv("redis.password", "SHIRAKAMI_REDIS_PASSWORD")
	_ = viper.BindEnv("webhook.secret", "SHIRAKAMI_WEBHOOK_SECRET")
	_ = viper.BindEnv("webhook.gitlab_token", "SHIRAKAMI_GITLAB_TOKEN")
	_ = viper.BindEnv("webhook.github_token", "SHIRAKAMI_GITHUB_TOKEN")

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
