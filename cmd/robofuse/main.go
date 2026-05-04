package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/robofuse/robofuse/internal/config"
	"github.com/robofuse/robofuse/internal/logger"
	"github.com/robofuse/robofuse/pkg/sync"
	"github.com/spf13/cobra"
)

var version = "1.1.2"

var (
	cfgPath          string
	logLevel         string
	rebuildOrganized bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "robofuse",
		Short: "Real-Debrid STRM file generator",
		Long: `robofuse generates .strm files from Real-Debrid torrents for use
with media players like Infuse, Jellyfin, and Emby.`,
		Version: version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if logLevel != "" {
				logger.SetLogLevel(logLevel)
			}
			return nil
		},
	}

	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "", "Path to config file")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().BoolVar(&rebuildOrganized, "rebuild-organized", false, "Delete organized directory and rebuild from scratch")

	rootCmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run sync once and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			config.SetInstance(cfg)
			printBanner()
			runSync(cfg, false)
			return nil
		},
	})

	rootCmd.AddCommand(&cobra.Command{
		Use:   "watch",
		Short: "Run sync continuously",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			config.SetInstance(cfg)
			printBanner()
			runWatch(cfg)
			return nil
		},
	})

	rootCmd.AddCommand(&cobra.Command{
		Use:   "dry-run",
		Short: "Preview changes without making them",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			config.SetInstance(cfg)
			printBanner()
			runSync(cfg, true)
			return nil
		},
	})

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printBanner() {
	banner := `
  ██████╗  ██████╗ ██████╗  ██████╗ ███████╗██╗   ██╗███████╗███████╗
  ██╔══██╗██╔═══██╗██╔══██╗██╔═══██╗██╔════╝██║   ██║██╔════╝██╔════╝
  ██████╔╝██║   ██║██████╔╝██║   ██║█████╗  ██║   ██║███████╗█████╗  
  ██╔══██╗██║   ██║██╔══██╗██║   ██║██╔══╝  ██║   ██║╚════██║██╔══╝  
  ██║  ██║╚██████╔╝██████╔╝╚██████╔╝██║     ╚██████╔╝███████║███████╗
  ╚═╝  ╚═╝ ╚═════╝ ╚═════╝  ╚═════╝ ╚═╝      ╚═════╝ ╚══════╝╚══════╝
                                                          v` + version + `
`
	fmt.Println(banner)
}

func runSync(cfg *config.Config, dryRun bool) {
	log := logger.Default()
	service := sync.New(cfg)

	if rebuildOrganized && !dryRun {
		log.Info().Str("dir", cfg.OrganizedDir).Msg("Rebuilding organized directory")
		os.RemoveAll(cfg.OrganizedDir)
		os.Remove(cfg.CacheDir + "/organizer_db.json")
	}

	result, err := service.Run(dryRun)
	if err != nil {
		log.Error().Err(err).Msg("Sync failed")
		os.Exit(1)
	}
	service.WaitForProbes()

	summary := sync.FormatSummary(result, sync.SummaryOptions{
		DryRun:     dryRun,
		IncludeOrg: cfg.PttRename && !dryRun,
	})
	log.Info().Msg(summary)
}

func runWatch(cfg *config.Config) {
	log := logger.Default()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	service := sync.New(cfg)
	if err := service.Watch(ctx); err != nil {
		log.Error().Err(err).Msg("Watch mode failed")
		os.Exit(1)
	}
}
