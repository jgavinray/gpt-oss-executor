// Command gpt-oss-executor is the entry point for the OpenClaw executor.
// It loads configuration, wires up the agentic loop components, starts the
// OpenAI-compatible HTTP server, and handles graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jgavinray/gpt-oss-executor/internal/config"
	"github.com/jgavinray/gpt-oss-executor/internal/executor"
	"github.com/jgavinray/gpt-oss-executor/internal/httpserver"
	"github.com/jgavinray/gpt-oss-executor/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config/executor.yaml", "path to executor.yaml")
	flag.Parse()

	// Load and validate configuration.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("loading config %q: %w", *cfgPath, err)
	}

	// Construct structured logger.
	logger, err := logging.NewLogger(cfg.Logging.Level, cfg.Logging.Format, cfg.Logging.Output)
	if err != nil {
		return fmt.Errorf("initialising logger: %w", err)
	}

	// Construct error logger (appends to daily markdown file).
	var errLogger *logging.ErrorLogger
	if cfg.Logging.ErrorLogDir != "" && cfg.Logging.ErrorLogFilename != "" {
		errLogger = logging.NewErrorLogger(cfg.Logging.ErrorLogDir, cfg.Logging.ErrorLogFilename)
	}

	logger.Info("configuration loaded",
		slog.String("config", *cfgPath),
		slog.String("gpt_oss_url", cfg.Executor.GptOSSURL),
		slog.String("gateway_url", cfg.Executor.OpenClawGatewayURL),
		slog.String("parser_strategy", cfg.Parser.Strategy),
		slog.Int("max_iterations", cfg.Executor.MaxIterations),
	)

	// Construct the core agentic loop executor.
	exec, err := executor.New(cfg, logger, errLogger)
	if err != nil {
		return fmt.Errorf("initialising executor: %w", err)
	}

	// Construct and start the HTTP server.
	srv := httpserver.New(cfg, exec, logger)

	// Start listening in the background.
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			serverErr <- err
		}
	}()

	// Block until an OS signal or a server error.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("signal received, shutting down", slog.String("signal", sig.String()))
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	}

	// Graceful shutdown with background context (run() context is finished).
	if err := srv.Shutdown(context.Background()); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}
