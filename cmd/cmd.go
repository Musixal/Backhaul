package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/musix/backhaul/internal/client"
	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/server"
	"github.com/musix/backhaul/internal/utils"

	"github.com/BurntSushi/toml"
)

var (
	logger = utils.NewLogger("info")
)

func Run(configPath string) {
	// Load and parse the configuration file
	cfg, err := loadConfig(configPath)
	if err != nil {
		logger.Fatalf("failed to load configuration: %v", err)
	}

	// Apply default values to the configuration
	applyDefaults(&cfg)

	// Create a context for graceful shutdown handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Determine whether to run as a server or client
	if cfg.Server.BindAddr != "" {
		srv := server.NewServer(&cfg.Server, ctx) // server
		go srv.Start()

		// Wait for shutdown signal
		<-sigChan
		srv.Stop()
		time.Sleep(1 * time.Second)
		logger.Println("shutting down server...")

	} else if cfg.Client.RemoteAddr != "" {
		clnt := client.NewClient(&cfg.Client, ctx) // client
		go clnt.Start()

		// Wait for shutdown signal
		<-sigChan
		clnt.Stop()
		time.Sleep(1 * time.Second)
		logger.Println("shutting down client...")
	} else {
		logger.Fatalf("neither server nor client configuration is properly set.")
	}
}

// loadConfig loads and parses the TOML configuration file.
func loadConfig(configPath string) (config.Config, error) {
	var cfg config.Config
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
