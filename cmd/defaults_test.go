package cmd

import (
	"testing"

	"github.com/musix/backhaul/config"
)

func TestApplyDefaultsRemoteAddrPool(t *testing.T) {
	cfg := &config.Config{
		Client: config.ClientConfig{
			RemoteAddrPool: []string{"203.0.113.10:8443", "203.0.113.11:8443"},
		},
	}

	applyDefaults(cfg)

	if cfg.Client.RemoteAddr != "203.0.113.10:8443" {
		t.Fatalf("RemoteAddr = %q, want first pool entry", cfg.Client.RemoteAddr)
	}
	if cfg.Client.RemoteAddrStrategy != defaultSourceStrategy {
		t.Fatalf("RemoteAddrStrategy = %q, want %q", cfg.Client.RemoteAddrStrategy, defaultSourceStrategy)
	}
	if cfg.Client.SourceIPStrategy != defaultSourceStrategy {
		t.Fatalf("SourceIPStrategy = %q, want %q", cfg.Client.SourceIPStrategy, defaultSourceStrategy)
	}
}

func TestApplyDefaultsKeepsExplicitRemoteAddr(t *testing.T) {
	cfg := &config.Config{
		Client: config.ClientConfig{
			RemoteAddr:     "198.51.100.10:8443",
			RemoteAddrPool: []string{"203.0.113.10:8443"},
		},
	}

	applyDefaults(cfg)

	if cfg.Client.RemoteAddr != "198.51.100.10:8443" {
		t.Fatalf("RemoteAddr = %q, want explicit value", cfg.Client.RemoteAddr)
	}
}
