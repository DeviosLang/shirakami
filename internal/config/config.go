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
	// IndexMode controls how the symbol graph index is used during analysis.
	// Valid values: "off" (default, pure LLM), "shadow", "hybrid", "deterministic".
	// Can be overridden per-run via the --index-mode CLI flag.
	// Environment variable: SHIRAKAMI_INDEX_MODE
	IndexMode string `mapstructure:"index_mode"`
	// MaxRounds caps the cross-repo hop iterations (orchestrator rounds).
	// 0 = default (10, deep mode). Set to 3 for fast mode.
	// Environment variable: SHIRAKAMI_MAX_ROUNDS
	MaxRounds int `mapstructure:"max_rounds"`
	// P1StepBudget is the step budget for P1-priority Workers.
	// 0 = use global default (300). Recommended: 150 to balance coverage vs. speed.
	// P0 Workers always use the full budget; P2 Workers are hard-capped at 50.
	// Environment variable: SHIRAKAMI_P1_STEP_BUDGET
	P1StepBudget int `mapstructure:"p1_step_budget"`
	// P0StepBudget is the step budget for P0-priority Workers.
	// 0 = no cap (legacy behavior, 300 steps). Recommended: 200 to prevent a single
	// large-diff Worker from monopolising all concurrency for 40+ minutes.
	// Environment variable: SHIRAKAMI_P0_STEP_BUDGET
	P0StepBudget int `mapstructure:"p0_step_budget"`
	// Server holds HTTP server configuration (used in server mode).
	Server ServerConfig `mapstructure:"server"`
	// Metrics holds Prometheus export settings.
	Metrics MetricsConfig `mapstructure:"metrics"`
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
	// RequestTimeout is the per-request timeout for LLM calls in seconds.
	// A new context with this deadline is applied to each CreateChatCompletion call.
	// Default: 120 (2 minutes). Set to 0 to disable (inherit caller context only).
	// Environment variable: SHIRAKAMI_LLM_REQUEST_TIMEOUT
	RequestTimeout int `mapstructure:"request_timeout"`
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

// ServerConfig holds HTTP API server settings.
type ServerConfig struct {
	// Addr is the listen address for the HTTP server (default: ":8080").
	// Environment variable: SHIRAKAMI_SERVER_ADDR
	Addr string `mapstructure:"addr"`

	// MetricsAddr is the listen address for the Prometheus /metrics endpoint.
	// It is intentionally separated from the main API port so that /metrics is
	// never exposed through the external-facing Service/LoadBalancer.
	// Default: ":9091". Environment variable: SHIRAKAMI_SERVER_METRICS_ADDR
	MetricsAddr string `mapstructure:"metrics_addr"`

	// MaxConcurrentAnalyses limits how many analysis jobs run simultaneously.
	// Useful for NFS-backed workspaces where concurrent git operations conflict.
	// Default: 1. Environment variable: SHIRAKAMI_SERVER_MAX_CONCURRENT
	MaxConcurrentAnalyses int `mapstructure:"max_concurrent_analyses"`

	// DefaultModes is the list of analysis modes to run when a request omits the
	// modes field. Valid values: "chain", "e2e", "ut". Default: ["chain","e2e","ut"].
	// Environment variable: SHIRAKAMI_SERVER_DEFAULT_MODES (comma-separated)
	DefaultModes []string `mapstructure:"default_modes"`
}

// MetricsConfig controls how Prometheus metrics are exported.
type MetricsConfig struct {
	// PushgatewayURL is the address of a Prometheus Pushgateway.
	// When set, metrics are pushed to this URL at startup, on task completion,
	// and periodically (see PushIntervalSeconds).
	// Example: "http://21.215.89.245:8080"
	// Environment variable: SHIRAKAMI_METRICS_PUSHGATEWAY_URL
	PushgatewayURL string `mapstructure:"pushgateway_url"`

	// PushIntervalSeconds is the interval (in seconds) between periodic pushes.
	// Default: 30. Set to 0 to disable periodic pushing (task-triggered only).
	// Environment variable: SHIRAKAMI_METRICS_PUSH_INTERVAL
	PushIntervalSeconds int `mapstructure:"push_interval_seconds"`

	// JobName is the Prometheus job label attached to all pushed metrics.
	// Default: "shirakami".
	// Environment variable: SHIRAKAMI_METRICS_JOB_NAME
	JobName string `mapstructure:"job_name"`
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
	_ = viper.BindEnv("llm.request_timeout", "SHIRAKAMI_LLM_REQUEST_TIMEOUT")
	_ = viper.BindEnv("db.dsn", "SHIRAKAMI_DB_DSN")
	_ = viper.BindEnv("redis.addr", "SHIRAKAMI_REDIS_ADDR")
	_ = viper.BindEnv("redis.password", "SHIRAKAMI_REDIS_PASSWORD")
	_ = viper.BindEnv("webhook.secret", "SHIRAKAMI_WEBHOOK_SECRET")
	_ = viper.BindEnv("webhook.gitlab_token", "SHIRAKAMI_GITLAB_TOKEN")
	_ = viper.BindEnv("webhook.github_token", "SHIRAKAMI_GITHUB_TOKEN")
	_ = viper.BindEnv("index_mode", "SHIRAKAMI_INDEX_MODE")
	_ = viper.BindEnv("max_rounds", "SHIRAKAMI_MAX_ROUNDS")
	_ = viper.BindEnv("p1_step_budget", "SHIRAKAMI_P1_STEP_BUDGET")
	_ = viper.BindEnv("p0_step_budget", "SHIRAKAMI_P0_STEP_BUDGET")
	_ = viper.BindEnv("server.addr", "SHIRAKAMI_SERVER_ADDR")
	_ = viper.BindEnv("server.metrics_addr", "SHIRAKAMI_SERVER_METRICS_ADDR")
	_ = viper.BindEnv("server.max_concurrent_analyses", "SHIRAKAMI_SERVER_MAX_CONCURRENT")
	_ = viper.BindEnv("server.default_modes", "SHIRAKAMI_SERVER_DEFAULT_MODES")
	_ = viper.BindEnv("metrics.pushgateway_url", "SHIRAKAMI_METRICS_PUSHGATEWAY_URL")
	_ = viper.BindEnv("metrics.push_interval_seconds", "SHIRAKAMI_METRICS_PUSH_INTERVAL")
	_ = viper.BindEnv("metrics.job_name", "SHIRAKAMI_METRICS_JOB_NAME")

	// defaults
	viper.SetDefault("workspace.dir", "/tmp/shirakami-workspace")
	viper.SetDefault("redis.addr", "localhost:6379")
	viper.SetDefault("llm.model", "gpt-4o")
	viper.SetDefault("llm.endpoint", "https://api.openai.com/v1")
	viper.SetDefault("llm.max_tokens", 128000)
	viper.SetDefault("llm.request_timeout", 120)
	viper.SetDefault("index_mode", "off")
	viper.SetDefault("max_rounds", 3)
	viper.SetDefault("p1_step_budget", 150)
	viper.SetDefault("server.addr", ":8080")
	viper.SetDefault("server.metrics_addr", ":9091")
	viper.SetDefault("server.max_concurrent_analyses", 1)
	viper.SetDefault("server.default_modes", []string{"chain", "e2e", "ut"})
	viper.SetDefault("metrics.push_interval_seconds", 30)
	viper.SetDefault("metrics.job_name", "shirakami")

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
