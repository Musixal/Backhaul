package cmd

import (
	"github.com/musix/backhaul/internal/config"

	"github.com/sirupsen/logrus"
)

const ( // Default values
	defaultTransport      = config.TCP
	defaultToken          = "musix"
	defaultChannelSize    = 2048
	defaultRetryInterval  = 1 // only for client
	defaultConnectionPool = 8
	defaultLogLevel       = "info"
	defaultMuxSession     = 1
	defaultKeepAlive      = 20
)

func applyDefaults(cfg *config.Config) {
	// Transport
	switch cfg.Server.Transport {
	case config.TCP, config.TCPMUX, config.WS: // valid values
	case "":
		cfg.Server.Transport = defaultTransport
	default:
		logger.Warnf("invalid transport value '%s' for server, defaulting to '%s'", cfg.Server.Transport, defaultTransport)
		cfg.Server.Transport = defaultTransport
	}

	switch cfg.Client.Transport {
	case config.TCP, config.TCPMUX, config.WS: //valid values
	case "":
		cfg.Client.Transport = defaultTransport
	default:
		logger.Warnf("invalid transport value '%s' for client, defaulting to '%s'", cfg.Client.Transport, defaultTransport)
		cfg.Client.Transport = defaultTransport
	}

	// Token
	if cfg.Server.Token == "" {
		cfg.Server.Token = defaultToken
	}
	if cfg.Client.Token == "" {
		cfg.Client.Token = defaultToken
	}

	// Nodelay default is false if not valid value found

	// Channel size
	if cfg.Server.ChannelSize <= 0 {
		cfg.Server.ChannelSize = defaultChannelSize
	}

	// Loglevel
	if _, err := logrus.ParseLevel(cfg.Client.LogLevel); err != nil {
		cfg.Client.LogLevel = defaultLogLevel
	}

	if _, err := logrus.ParseLevel(cfg.Server.LogLevel); err != nil {
		cfg.Server.LogLevel = defaultLogLevel
	}

	// Retry interval
	if cfg.Client.RetryInterval <= 0 {
		cfg.Client.RetryInterval = defaultRetryInterval
	}

	// Connection pool
	if cfg.Server.ConnectionPool <= 0 {
		cfg.Server.ConnectionPool = defaultConnectionPool
	}

	// Mux Session
	if cfg.Server.MuxSession <= 0 {
		cfg.Server.MuxSession = defaultMuxSession
	}
	if cfg.Client.MuxSession <= 0 {
		cfg.Client.MuxSession = defaultMuxSession
	}

	// PPROF default is false if not valid value found

	// keep alive
	if cfg.Server.Keepalive <= 0 {
		cfg.Server.Keepalive = defaultKeepAlive
	}
	if cfg.Client.Keepalive <= 0 {
		cfg.Client.Keepalive = defaultKeepAlive
	}
}
