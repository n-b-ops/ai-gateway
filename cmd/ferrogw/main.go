// Package main is the entry point for the ferrogw gateway server and CLI.
package main

import (
	"os"

	"github.com/ferro-labs/ai-gateway/internal/bootstrap"
	"github.com/ferro-labs/ai-gateway/internal/cli"
	"github.com/spf13/cobra"

	// Register built-in plugins so they can be loaded from config.
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/budget"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/cache"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/cachealign"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/logger"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/maxtoken"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/ratelimit"
	_ "github.com/ferro-labs/ai-gateway/internal/plugins/wordfilter"
)

var rootCmd = &cobra.Command{
	Use:   "ferrogw",
	Short: "Ferro Labs AI Gateway",
	Long:  "High-performance AI gateway with smart routing, plugins, and admin dashboard.",
	// Default: start the server (backward compatible).
	Run: func(_ *cobra.Command, _ []string) {
		bootstrap.Serve()
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the gateway server",
	Run: func(_ *cobra.Command, _ []string) {
		bootstrap.Serve()
	},
}

func main() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(cli.InitCmd)
	rootCmd.AddCommand(cli.ValidateCmd)
	rootCmd.AddCommand(cli.PluginsCmd)
	rootCmd.AddCommand(cli.DoctorCmd)
	rootCmd.AddCommand(cli.StatusCmd)
	rootCmd.AddCommand(cli.VersionCmd)
	rootCmd.AddCommand(cli.AdminCmd)

	// Persistent flags for CLI commands.
	rootCmd.PersistentFlags().String("gateway-url", "",
		"Gateway base URL (env: FERROGW_URL, default: http://localhost:8080)")
	rootCmd.PersistentFlags().String("api-key", "",
		"Admin API key (env: FERROGW_API_KEY)")
	rootCmd.PersistentFlags().String("format", "table",
		"Output format: table, json, or yaml")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
