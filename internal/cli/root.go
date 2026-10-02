package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/i18n"
	"github.com/aligundogdu/matrixmigrate/internal/migration"
	"github.com/aligundogdu/matrixmigrate/internal/tui"
	"github.com/aligundogdu/matrixmigrate/internal/version"
)

var (
	cfgFile  string
	language string
	batch    bool
	verbose  bool
)

var rootCmd = &cobra.Command{
	Use:     "matrixmigrate",
	Short:   "Mattermost to Matrix migration tool",
	Version: version.GetFullVersion(),
	Long: `MatrixMigrate is a CLI tool for migrating from Mattermost to Matrix Synapse.

It supports multi-step migration with resumable checkpoints, SSH tunnel connections,
and provides both interactive TUI and batch modes.

Examples:
  # Start interactive TUI
  matrixmigrate

  # Start with Turkish interface
  matrixmigrate --lang tr

  # Run in batch mode
  matrixmigrate --batch export assets

  # Test connections
  matrixmigrate test mattermost
  matrixmigrate test matrix`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Initialize i18n
		if err := i18n.Init(language); err != nil {
			return fmt.Errorf("failed to initialize i18n: %w", err)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load config
		// A missing config.yaml already falls back to defaults inside config.Load, so an
		// error here is a file that exists but does not parse or validate: report it in
		// both modes rather than exit successfully.
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		// Override language from config if not set via flag
		if language == "en" && cfg.Language != "" {
			if err := i18n.Init(cfg.Language); err != nil {
				return err
			}
		}

		// Ensure data directories exist
		if err := cfg.EnsureDataDirs(); err != nil {
			return err
		}

		// If batch mode, show help
		if batch {
			return cmd.Help()
		}

		// Start TUI. It shows its own stopping notice, so the CLI one stays quiet.
		tuiRunning.Store(true)
		defer tuiRunning.Store(false)
		return tui.Run(cmd.Context(), cfg)
	},
}

// Execute runs the root command.
//
// SIGINT, SIGTERM and SIGHUP (a dropped SSH session) cancel the context the commands run
// under: an import step then finishes the item in flight, saves its progress and returns
// migration.ErrInterrupted. A second signal gets the default behaviour and kills the process at
// once. A command that runs to the end regardless, such as an export, still exits non-zero
// after a signal, so a calling script stops rather than starting the next step.
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Closed before the deferred stop() runs, so the cancellation stop() causes on a normal
	// exit is not mistaken for an interrupt.
	finished := make(chan struct{})
	defer close(finished)
	go announceInterrupt(ctx, stop, finished, os.Stderr)

	err := rootCmd.ExecuteContext(ctx)
	// Only a signal cancels ctx before this point: stop() runs on the first signal or on return.
	result := interruptedExit(ctx.Err() != nil, err)
	if err == nil && result != nil {
		fmt.Fprintf(os.Stderr, "⚠ %s\n", i18n.T("messages.interrupted_exit"))
	}
	return result
}

// interruptedExit is the result of a run: err unchanged, except that a command that returned
// nil after a signal reports ErrInterrupted.
func interruptedExit(signalled bool, err error) error {
	if err != nil || !signalled {
		return err
	}
	return fmt.Errorf("stopped by a signal: %w", migration.ErrInterrupted)
}

// tuiRunning is set while the TUI owns the terminal. The TUI reports an interrupt itself;
// a line written to stderr then would land in the middle of its screen.
var tuiRunning atomic.Bool

// announceInterrupt waits for the first interrupt, then restores the default signal handling
// (so a second Ctrl+C kills the process) and tells the user what is happening. It returns
// without a word once finished is closed, and prints nothing while the TUI is running.
func announceInterrupt(ctx context.Context, stop func(), finished <-chan struct{}, out io.Writer) {
	select {
	case <-finished:
		return
	case <-ctx.Done():
	}
	select {
	case <-finished:
		return
	default:
	}
	stop()
	if tuiRunning.Load() {
		return
	}
	fmt.Fprintf(out, "⚠ %s\n", i18n.T("messages.interrupt_received"))
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default is ./config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&language, "lang", "l", "en", "interface language (en, tr)")
	rootCmd.PersistentFlags().BoolVar(&batch, "batch", false, "run in batch mode (non-interactive)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")

	// Add subcommands
	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(importCmd)
	rootCmd.AddCommand(testCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(versionCmd)
}

// versionCmd shows detailed version information
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show detailed version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version.GetBuildInfo())
	},
}

// loadConfig is a helper to load config for subcommands
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		// config.Load reports what actually went wrong and names the file; prefixing every
		// failure with "not found" mislabels parse and validation errors, and prints an empty
		// path when --config was not given.
		return nil, err
	}

	if err := cfg.EnsureDataDirs(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// printSuccess prints a success message
func printSuccess(format string, args ...interface{}) {
	fmt.Printf("✓ "+format+"\n", args...)
}

// printInfo prints an info message
func printInfo(format string, args ...interface{}) {
	fmt.Printf("ℹ "+format+"\n", args...)
}

// printWarning prints a warning message
func printWarning(format string, args ...interface{}) {
	fmt.Printf("⚠ "+format+"\n", args...)
}

// printProgress prints a progress message
func printProgress(format string, args ...interface{}) {
	if verbose {
		fmt.Printf("  "+format+"\n", args...)
	}
}
