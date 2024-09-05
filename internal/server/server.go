package server

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/server/transport"
	"github.com/musix/backhaul/internal/utils"

	"github.com/sirupsen/logrus"
)

type Server struct {
	config *config.ServerConfig
	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Logger
}

func NewServer(cfg *config.ServerConfig, parentCtx context.Context) *Server {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Server{
		config: cfg,
		ctx:    ctx,
		cancel: cancel,
		logger: utils.NewLogger(cfg.LogLevel),
	}
}

func (s *Server) Start() {
	// for pprof and debugging
	if s.config.PPROF {
		go func() {
			s.logger.Info("pprof started at port 6060")
			http.ListenAndServe("0.0.0.0:6060", nil)
		}()
	}

	if s.config.Transport == config.TCP {
		tcpConfig := &transport.TcpConfig{
			BindAddr:       s.config.BindAddr,
			Nodelay:        s.config.Nodelay,
			KeepAlive:      time.Duration(s.config.Keepalive) * time.Second,
			ConnectionPool: s.config.ConnectionPool,
			Token:          s.config.Token,
			ChannelSize:    s.config.ChannelSize,
			Ports:          s.config.Ports,
		}

		tcpServer := transport.NewTCPServer(s.ctx, tcpConfig, s.logger)
		go tcpServer.TunnelListener()

	} else if s.config.Transport == config.TCPMUX {
		tcpMuxConfig := &transport.TcpMuxConfig{
			BindAddr:    s.config.BindAddr,
			Nodelay:     s.config.Nodelay,
			KeepAlive:   time.Duration(s.config.Keepalive) * time.Second,
			Token:       s.config.Token,
			MuxSession:  s.config.MuxSession,
			ChannelSize: s.config.ChannelSize,
			Ports:       s.config.Ports,
		}

		tcpMuxServer := transport.NewTcpMuxServer(s.ctx, tcpMuxConfig, s.logger)
		go tcpMuxServer.TunnelListener()

	} else if s.config.Transport == config.WS {
		wsConfig := &transport.WsConfig{
			BindAddr:       s.config.BindAddr,
			Nodelay:        s.config.Nodelay,
			KeepAlive:      time.Duration(s.config.Keepalive) * time.Second,
			ConnectionPool: s.config.ConnectionPool,
			Token:          s.config.Token,
			ChannelSize:    s.config.ChannelSize,
			Ports:          s.config.Ports,
		}

		wsServer := transport.NewWSServer(s.ctx, wsConfig, s.logger)
		go wsServer.TunnelListener()

	}

	<-s.ctx.Done()
	s.logger.Info("all workers stopped successfully")
}

// Stop shuts down the server gracefully
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}
