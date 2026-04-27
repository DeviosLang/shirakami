package config

import (
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
	Endpoint string `mapstructure:"endpoint"`
	APIKey   string `mapstructure:"api_key"`
	Model    string `mapstructure:"model"`
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
func Load(cfgFile string) (*Config, error) {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("shirakami")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("$HOME")
		viper.AddConfigPath("/etc/shirakami")
	}

	viper.SetEnvPrefix("SHIRAKAMI")
	viper.AutomaticEnv()

	// defaults
	viper.SetDefault("workspace.dir", "/tmp/shirakami-workspace")
	viper.SetDefault("redis.addr", "localhost:6379")
	viper.SetDefault("llm.model", "gpt-4o")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
