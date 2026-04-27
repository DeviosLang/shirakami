package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/DeviosLang/shirakami/internal/config"
	"github.com/DeviosLang/shirakami/internal/feedback"
	"github.com/DeviosLang/shirakami/internal/logger"
)

var (
	version = "0.1.0"
	cfgFile string
	addr    string
)

func main() {
	root := &cobra.Command{
		Use:     "shirakami-server",
		Short:   "Shirakami HTTP API server",
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			log := logger.Must("production")
			defer log.Sync() //nolint:errcheck

			log.Sugar().Infow("starting server", "addr", addr, "workspace", cfg.Workspace.Dir)

			mux := http.NewServeMux()
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, "ok")
			})
			mux.Handle("/metrics", feedback.Handler())

			log.Sugar().Infof("listening on %s", addr)
			return http.ListenAndServe(addr, mux)
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: shirakami.yaml)")
	root.Flags().StringVar(&addr, "addr", ":8080", "HTTP listen address")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
