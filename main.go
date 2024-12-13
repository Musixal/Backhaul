package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/musix/backhaul/cmd"
	"github.com/musix/backhaul/internal/utils"
)

var (
	logger     = utils.NewLogger("info")
	configPath *string
	ctx        context.Context
	cancel     context.CancelFunc
)

// Define the version of the application
const version = "v0.6.6"

func main() {
	configPath = flag.String("c", "", "path to the configuration file (TOML format)")
	showVersion := flag.Bool("v", false, "print the version and exit")

	flag.Parse()

	// If the version flag is provided, print the version and exit
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	// Check if the configPath is provided
	if *configPath == "" {
		logger.Fatalf("Usage: %s -c /path/to/config.toml", flag.CommandLine.Name())
	}

	// Apply temporary TCP optimizations at startup
	cmd.ApplyTCPTuning()

	// Create a context for graceful shutdown handling
	ctx, cancel = context.WithCancel(context.Background())

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go cmd.Run(*configPath, ctx)
	go hotReload()

	<-sigChan

	cancel()

	time.Sleep(1 * time.Second)
}

func hotReload() {
	// Get initial modification time of the config file
	lastModTime, err := getLastModTime(*configPath)
	if err != nil {
		logger.Fatalf("Error getting modification time: %v", err)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			modTime, err := getLastModTime(*configPath)
			if err != nil {
				logger.Errorf("Error checking file modification time: %v", err)
				continue
			}

			// If the modification time has changed, reload the app
			if modTime.After(lastModTime) {
				logger.Info("Config file changed, reloading application")

				// Cancel the previous context to stop the old running instance
				cancel()

				time.Sleep(2 * time.Second)

				// Create a new context for the new instance
				newCtx, newCancel := context.WithCancel(context.Background())
				go cmd.Run(*configPath, newCtx)

				// Update the last modification time and the context
				lastModTime = modTime
				ctx = newCtx
				cancel = newCancel
			}
		}
	}
}

func getLastModTime(file string) (time.Time, error) {
	absPath, _ := filepath.Abs(file)
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return time.Time{}, err
	}
	return fileInfo.ModTime(), nil
}