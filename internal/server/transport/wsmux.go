package transport

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

type WsMuxTransport struct {
	config       *WsMuxConfig
	parentctx    context.Context
	ctx          context.Context
	cancel       context.CancelFunc
	logger       *logrus.Logger
	smuxSession  []*smux.Session
	restartMutex sync.Mutex
	timeout      time.Duration
	usageMonitor *web.Usage
	muxCounter   int
	mu           sync.Mutex
}

type WsMuxConfig struct {
	BindAddr         string
	Nodelay          bool
	KeepAlive        time.Duration
	Token            string
	MuxSession       int
	ChannelSize      int
	Ports            []string
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	Sniffer          bool
	WebPort          int
	SnifferLog       string
	TunnelStatus     string
	Mode             config.TransportType // ws or wss
	TLSCertFile      string               // Path to the TLS certificate file
	TLSKeyFile       string               // Path to the TLS key file
}

func NewWSMuxServer(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &WsMuxTransport{
		config:       config,
		parentctx:    parentCtx,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		timeout:      2 * time.Second, // Default timeout
		smuxSession:  make([]*smux.Session, config.MuxSession),
		usageMonitor: web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		muxCounter:   0,
		mu:           sync.Mutex{},
	}

	return server
}

func (s *WsMuxTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")
	if s.cancel != nil {
		s.cancel()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.smuxSession = make([]*smux.Session, s.config.MuxSession)
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""
	s.muxCounter = 0
	s.mu = sync.Mutex{}

	go s.TunnelListener()

}

func (s *WsMuxTransport) portConfigReader() {
	// port mapping for listening on each local port
	for _, portMapping := range s.config.Ports {
		parts := strings.Split(portMapping, "=")
		if len(parts) != 2 {
			s.logger.Fatalf("invalid port mapping format: %s", portMapping)
			continue
		}

		localAddrStr := strings.TrimSpace(parts[0])
		if _, err := strconv.Atoi(localAddrStr); err == nil {
			localAddrStr = ":" + localAddrStr
		}

		remoteAddr := strings.TrimSpace(parts[1])

		go s.localListener(localAddrStr, remoteAddr)
	}
}

func (s *WsMuxTransport) TunnelListener() {
	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}
	s.config.TunnelStatus = "Disconnected (WSMux)"

	addr := s.config.BindAddr
	upgrader := websocket.Upgrader{
		ReadBufferSize:  16 * 1024,
		WriteBufferSize: 16 * 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// Create an HTTP server
	server := &http.Server{
		Addr:        addr,
		IdleTimeout: 600 * time.Second, // HERE
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.logger.Tracef("received http request from %s", r.RemoteAddr)

			// Read the "Authorization" header
			authHeader := r.Header.Get("Authorization")
			if authHeader != fmt.Sprintf("Bearer %v", s.config.Token) {
				s.logger.Warnf("unauthorized request from %s, closing connection", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized) // Send 401 Unauthorized response
				return
			}

			wsConn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				s.logger.Errorf("failed to upgrade connection from %s: %v", r.RemoteAddr, err)
				return
			}

			if r.URL.Path == "/channel" {
				if s.muxCounter >= s.config.MuxSession {
					s.logger.Info("MUX session is completed, drop the tunnel connection")
					wsConn.Close()
					return
				}
				var wg sync.WaitGroup
				s.mu.Lock()
				wg.Add(1)
				go s.acceptStreamConn(wsConn, s.muxCounter, &wg)
				wg.Wait()
				s.muxCounter++
				s.mu.Unlock()

				if s.muxCounter == s.config.MuxSession {
					s.logger.Info("MUX session completed, starting to listen on local ports")
					s.config.TunnelStatus = "Connected (WSMux)"
					go s.portConfigReader()
				}
			}

		}),
	}

	if s.config.Mode == config.WSMUX {
		go func() {
			s.logger.Infof("wsmux server starting, listening on %s", addr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			s.logger.Infof("wssmux server starting, listening on %s", addr)
			if err := server.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	}

	<-s.ctx.Done()

	// Gracefully shutdown the server
	s.logger.Infof("shutting down the websocket server on %s", addr)
	if err := server.Shutdown(context.Background()); err != nil {
		s.logger.Errorf("failed to gracefully shutdown the server: %v", err)
	}

}

func (s *WsMuxTransport) acceptStreamConn(wsConn *websocket.Conn, id int, wg *sync.WaitGroup) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			// config fot smux
			config := smux.Config{
				Version:           s.config.MuxVersion, // Smux protocol version
				KeepAliveInterval: 10 * time.Second,    // Shorter keep-alive interval to quickly detect dead peers
				KeepAliveTimeout:  30 * time.Second,    // Aggressive timeout to handle unresponsive connections
				MaxFrameSize:      s.config.MaxFrameSize,
				MaxReceiveBuffer:  s.config.MaxReceiveBuffer,
				MaxStreamBuffer:   s.config.MaxStreamBuffer,
			}
			// smux server
			session, err := smux.Client(wsConn.UnderlyingConn(), &config)
			if err != nil {
				s.logger.Errorf("failed to create MUX session for connection %s: %v", wsConn.RemoteAddr().String(), err)
				wsConn.Close()
				continue
			}

			s.smuxSession[id] = session
			s.logger.Infof("successfully established MUX session with ID %d for connection %s", id, wsConn.RemoteAddr().String())

			// Graceful shutdown
			defer func() {
				if err := session.Close(); err != nil {
					s.logger.Warnf("failed to close MUX session with ID %d: %v", id, err)
				} else {
					s.logger.Infof("MUX session with ID %d closed successfully", id)
				}
			}()

			wg.Done()
			<-s.ctx.Done()
		}
	}
}

func (s *WsMuxTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", localAddr, err)
		return
	}

	//close local listener after context cancellation
	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	// channel
	acceptChan := make(chan net.Conn, s.config.ChannelSize)

	// handle channel connections
	go s.handleMUXSession(acceptChan, remoteAddr)

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return

			default:
				s.logger.Debugf("waiting to accept incoming connection on %s", listener.Addr().String())
				conn, err := listener.Accept()
				if err != nil {
					s.logger.Debugf("failed to accept connection on %s: %v", listener.Addr().String(), err)
					continue
				}

				// discard any non-tcp connection
				tcpConn, ok := conn.(*net.TCPConn)
				if !ok {
					s.logger.Warnf("disarded non-TCP connection from %s", conn.RemoteAddr().String())
					conn.Close()
					continue
				}

				// trying to enable tcpnodelay
				if !s.config.Nodelay {
					if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
						s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
					} else {
						s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
					}
				}

				tcpConn.SetKeepAlive(true)
				tcpConn.SetKeepAlivePeriod(s.config.KeepAlive)

				select {
				case acceptChan <- tcpConn:
					s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

				case <-time.After(s.timeout): // channel is full, discard the connection
					s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
					tcpConn.Close()
				}

			}
		}
	}()

	<-s.ctx.Done()
}

func (s *WsMuxTransport) handleMUXSession(acceptChan chan net.Conn, remoteAddr string) {
	for {
		select {
		case incomingConn := <-acceptChan:
			id := rand.Intn(s.config.MuxSession)
			if s.smuxSession[id] == nil || s.smuxSession[id].IsClosed() {
				s.logger.Errorf("MUX session with ID %d is closed or nil. Discarding incoming connection from %s.", id, incomingConn.RemoteAddr().String())
				incomingConn.Close()
				s.logger.Info("attempting to restart server...")
				go s.Restart()
				return
			}

			stream, err := s.smuxSession[id].OpenStream()
			if err != nil {
				s.logger.Errorf("failed to open a new mux stream for session ID %d: %v", id, err)
				incomingConn.Close()
				s.logger.Info("attempting to restart server...")
				go s.Restart()
				return
			}
			// Send the target port over the connection
			if err := utils.SendBinaryString(stream, remoteAddr); err != nil {
				s.logger.Warnf("failed to send address %d over stream for session ID %d: %v", remoteAddr, id, err)
				incomingConn.Close()
				continue
			}

			go utils.ConnectionHandler(stream, incomingConn, s.logger, s.usageMonitor, incomingConn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)

		case <-s.ctx.Done():
			return
		}
	}
}
