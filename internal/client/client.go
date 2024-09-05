package client

import (
	"context"
	"time"

	"github.com/musix/backhaul/internal/utils"

	"github.com/musix/backhaul/internal/config"

	"github.com/musix/backhaul/internal/client/transport"

	"net/http"
	_ "net/http/pprof"

	"github.com/sirupsen/logrus"
)

// Client encapsulates the client configuration and state
type Client struct {
	config *config.ClientConfig
	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Logger
}

func NewClient(cfg *config.ClientConfig, parentCtx context.Context) *Client {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Client{
		config: cfg,
		ctx:    ctx,
		cancel: cancel,
		logger: utils.NewLogger(cfg.LogLevel),
	}
}

// Run starts the client and begins dialing the tunnel server
func (c *Client) Start() {
	// for pprof
	if c.config.PPROF {
		go func() {
			c.logger.Info("pprof started at port 6060")
			http.ListenAndServe("0.0.0.0:6060", nil)
		}()
	}

	c.logger.Infof("client with remote address %s started successfully", c.config.RemoteAddr)

	if c.config.Transport == config.TCP {
		tcpConfig := &transport.TcpConfig{
			RemoteAddr:    c.config.RemoteAddr,
			Nodelay:       c.config.Nodelay,
			KeepAlive:     time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval: time.Duration(c.config.RetryInterval) * time.Second,
			Token:         c.config.Token,
			Forwarder:     c.forwarderReader(c.config.Forwarder),
		}
		tcpClient := transport.NewTCPClient(c.ctx, tcpConfig, c.logger)
		go tcpClient.ChannelDialer()

	} else if c.config.Transport == config.TCPMUX {
		tcpMuxConfig := &transport.TcpMuxConfig{
			RemoteAddr:    c.config.RemoteAddr,
			Nodelay:       c.config.Nodelay,
			KeepAlive:     time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval: time.Duration(c.config.RetryInterval) * time.Second,
			Token:         c.config.Token,
			MuxSession:    c.config.MuxSession,

			Forwarder: c.forwarderReader(c.config.Forwarder),
		}
		tcpMuxClient := transport.NewMuxClient(c.ctx, tcpMuxConfig, c.logger)
		go tcpMuxClient.MuxDialer()

	} else if c.config.Transport == config.WS {
		WsConfig := &transport.WsConfig{
			RemoteAddr:    c.config.RemoteAddr,
			Nodelay:       c.config.Nodelay,
			KeepAlive:     time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval: time.Duration(c.config.RetryInterval) * time.Second,
			Token:         c.config.Token,
			Forwarder:     c.forwarderReader(c.config.Forwarder),
		}
		WsClient := transport.NewWSClient(c.ctx, WsConfig, c.logger)
		go WsClient.ChannelDialer()
	}

	<-c.ctx.Done()

	c.logger.Info("all workers stopped successfully")
}
func (c *Client) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}
