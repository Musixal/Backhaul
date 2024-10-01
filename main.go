package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/musix/backhaul/cmd"
	"github.com/musix/backhaul/internal/utils"
)

var (
	logger = utils.NewLogger("info")
)

// Define the version of the application -v
const version = "v0.3.3"

func main() {
	configPath := flag.String("c", "", "path to the configuration file (TOML format)")
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

	// Create a context for graceful shutdown handling
	ctx, cancel := context.WithCancel(context.Background())

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go cmd.Run(*configPath, ctx)

	// Hot Reload
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			logger.Fatal(err)
		}
		defer watcher.Close()

		// Add the config file to the watcher
		err = watcher.Add(*configPath)
		if err != nil {
			logger.Fatal(err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
					logger.Info("config changed, hot reloading: ", event.Name)

					// Cancel the previous context to stop the old running instance
					cancel()

					// Create a new context for the new instance
					newCtx, newCancel := context.WithCancel(context.Background())
					go cmd.Run(*configPath, newCtx)

					// Update the parent context and cancel function with the new ones
					ctx = newCtx
					cancel = newCancel
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Error("Watcher error: ", err)
			}
		}

	}()

	<-sigChan
	cancel()
	time.Sleep(1 * time.Second)
}
