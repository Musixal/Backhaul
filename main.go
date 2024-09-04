package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/musix/backhaul/cmd"
)

// Define the version of the application
const version = "v0.1"

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
		log.Fatalf("Usage: %s -c /path/to/config.toml", flag.CommandLine.Name())
	}

	cmd.Run(*configPath)
}
