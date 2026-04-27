package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/logger"
)

var (
	version = "0.1.0"
	cfgFile string
)

func main() {
	root := &cobra.Command{
		Use:     "shirakami",
		Short:   "Shirakami static analysis agent",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			log := logger.Must("production")
			defer log.Sync() //nolint:errcheck

			log.Sugar().Infow("starting analyze", "workspace", cfg.Workspace.Dir)
			fmt.Println("analyze: nothing to do yet")
			return nil
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: shirakami.yaml)")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
