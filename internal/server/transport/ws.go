package transport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsTransport struct {
	config            *WsConfig
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	tunnelChannel     chan TunnelChannel
	getNewConnChan    chan struct{}
	controlChannel    *websocket.Conn
	restartMutex      sync.Mutex
	heartbeatDuration time.Duration
	heartbeatSig      string
	chanSignal        string
	chanMu            sync.Mutex
	usageMonitor      *web.Usage
}

type WsConfig struct {
	BindAddr     string
	Nodelay      bool
	KeepAlive    time.Duration
	Token        string
	ChannelSize  int
	Ports        []string
	Sniffer      bool
	WebPort      int
	SnifferLog   string
	TLSCertFile  string               // Path to the TLS certificate file
	TLSKeyFile   string               // Path to the TLS key file
	Mode         config.TransportType // ws or wss
	Heartbeat    int                  // in seconds
	TunnelStatus string
}

type TunnelChannel struct {
	conn *websocket.Conn
	ping chan struct{}
	mu   *sync.Mutex
}

func NewWSServer(parentCtx context.Context, config *WsConfig, logger *logrus.Logger) *WsTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &WsTransport{
		config:            config,
		parentctx:         parentCtx,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		tunnelChannel:     make(chan TunnelChannel, config.ChannelSize),
		getNewConnChan:    make(chan struct{}, config.ChannelSize),
		controlChannel:    nil,                                           // will be set when a control connection is established
		heartbeatDuration: time.Duration(config.Heartbeat) * time.Second, // Default heartbeat duration
		heartbeatSig:      "0",                                           // Default heartbeat signal
		chanSignal:        "1",                                           // Default channel signal
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return server
}
func (s *WsTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")
	if s.cancel != nil {
		s.cancel()
	}

	// Close any open connections in the tunnel channel.
	go s.cleanupConnections()

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan TunnelChannel, s.config.ChannelSize)
	s.getNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""

	go s.TunnelListener()

}

// cleanupConnections closes all active connections in the tunnel channel.
func (s *WsTransport) cleanupConnections() {
	if s.controlChannel != nil {
		s.logger.Debug("control channel have been closed.")
		s.controlChannel.Close()
	}
	for {
		select {
		case conn := <-s.tunnelChannel:
			if conn.conn != nil {
				conn.conn.Close()
				s.logger.Trace("existing tunnel connections have been closed.")
			}
		default:
			return
		}
	}
}

func (s *WsTransport) getClosedSignal() {
	for {
		// Channel to receive the message or error
		resultChan := make(chan struct {
			message []byte
			err     error
		})
		go func() {
			_, message, err := s.controlChannel.ReadMessage()
			resultChan <- struct {
				message []byte
				err     error
			}{message, err}
		}()

		select {
		case <-s.ctx.Done():
			return

		case result := <-resultChan:
			if result.err != nil {
				s.logger.Errorf("failed to receive message from tunnel connection: %v", result.err)
				go s.Restart()
				return
			}
			if string(result.message) == "closed" {
				s.logger.Info("control channel has been closed by the client")
				go s.Restart()
				return
			}
		}
	}

}

func (s *WsTransport) portConfigReader() {
	// port mapping for listening on each local port
	for _, portMapping := range s.config.Ports {
		var localAddr string
		parts := strings.Split(portMapping, "=")
		if len(parts) < 2 {
			port, err := strconv.Atoi(parts[0])
			if err != nil {
				s.logger.Fatalf("invalid port mapping format: %s", portMapping)
			}
			localAddr = fmt.Sprintf(":%d", port)
			parts = append(parts, strconv.Itoa(port))
			s.logger.Info(parts[1])
		} else {
			localAddr = strings.TrimSpace(parts[0])
			if _, err := strconv.Atoi(localAddr); err == nil {
				localAddr = ":" + localAddr // :3080 format
			}
		}

		remoteAddr := strings.TrimSpace(parts[1])

		go s.localListener(localAddr, remoteAddr)
	}
}

func (s *WsTransport) heartbeat() {
	ticker := time.NewTicker(s.heartbeatDuration)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.controlChannel == nil {
				s.logger.Warn("control channel is nil. Restarting server to re-establish connection...")
				go s.Restart()
				return
			}
			s.chanMu.Lock() // race condition vs getNewConnection func
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.heartbeatSig))
			s.chanMu.Unlock()
			if err != nil {
				s.logger.Errorf("Failed to send heartbeat signal. Error: %v. Restarting server...", err)
				go s.Restart()
				return
			}
			s.logger.Debug("heartbeat signal sent successfully")
		}
	}
}

func (s *WsTransport) getNewConnection() {
	for {
		select {
		case <-s.ctx.Done():
			return

		case <-s.getNewConnChan:
			s.chanMu.Lock()
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.chanSignal))
			s.chanMu.Unlock()
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}
		}
	}
}

func (s *WsTransport) TunnelListener() {
	// for  webui
	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}

	s.config.TunnelStatus = "Disconnected (Websocket)"

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

			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				s.logger.Errorf("failed to upgrade connection from %s: %v", r.RemoteAddr, err)
				return
			}

			if r.URL.Path == "/channel" && s.controlChannel == nil {
				s.controlChannel = conn

				s.logger.Info("control channel established successfully")

				go s.getNewConnection()
				go s.heartbeat()
				go s.portConfigReader()
				go s.getClosedSignal()

				s.config.TunnelStatus = "Connected (Websocket)"

				return
			}

			wsConn := TunnelChannel{
				conn: conn,
				ping: make(chan struct{}),
				mu:   &sync.Mutex{},
			}
			select {
			case s.tunnelChannel <- wsConn:
				go s.pingSender(&wsConn)
				s.logger.Debugf("websocket connection accepted from %s", conn.RemoteAddr().String())
			default:
				s.logger.Warnf("websocket tunnel channel is full, closing connection from %s", conn.RemoteAddr().String())
				conn.Close()
			}
		}),
	}

	if s.config.Mode == config.WS {
		go func() {
			s.logger.Infof("ws server starting, listening on %s", addr)
			if s.controlChannel == nil {
				s.logger.Info("waiting for ws control channel connection")
			}
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			s.logger.Infof("wss server starting, listening on %s", addr)
			if s.controlChannel == nil {
				s.logger.Info("waiting for wss control channel connection")
			}
			if err := server.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	}

	<-s.ctx.Done()

	// Gracefully shutdown the server
	s.logger.Infof("shutting down the webSocket server on %s", addr)
	if err := server.Shutdown(context.Background()); err != nil {
		s.logger.Errorf("Failed to gracefully shutdown the server: %v", err)
	}
}

func (s *WsTransport) localListener(localAddr string, remoteAddr string) {
	portListener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", localAddr, err)
		return
	}

	//close local listener after context cancellation
	defer portListener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", portListener.Addr().String())

	// make a channel
	acceptChan := make(chan net.Conn, s.config.ChannelSize)

	// start accepting incoming connections
	go s.acceptLocConn(portListener, acceptChan)
	go s.handleWSSession(remoteAddr, acceptChan)

	<-s.ctx.Done()
}

func (s *WsTransport) acceptLocConn(listener net.Listener, acceptChan chan net.Conn) {
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
			case s.getNewConnChan <- struct{}{}:
				// Successfully requested a new connection
			default:
				// The channel is full, do nothing
				s.logger.Warn("getNewConnChan is full, cannot request a new connection")
			}

			select {
			case acceptChan <- tcpConn:
				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default: // channel is full, discard the connection
				s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
				tcpConn.Close()
			}
		}
	}
}

func (s *WsTransport) handleWSSession(remoteAddr string, acceptChan chan net.Conn) {
	for {
		select {
		case incomingConn := <-acceptChan:
		innerloop:
			for {
				select {
				case tunnelConnection := <-s.tunnelChannel:
					close(tunnelConnection.ping)
					tunnelConnection.mu.Lock()
					if err := tunnelConnection.conn.WriteMessage(websocket.TextMessage, []byte(remoteAddr)); err != nil {
						s.logger.Debugf("%v", err) // failed to send port number
						tunnelConnection.conn.Close()
						continue innerloop
					}
					// Handle data exchange between connections
					go utils.WSToTCPConnHandler(tunnelConnection.conn, incomingConn, s.logger, s.usageMonitor, incomingConn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
					break innerloop

				case <-time.After(time.Second * 30):
					s.logger.Warn("tunnel connection unavailable, request a new connection")
					s.getNewConnChan <- struct{}{}
					continue innerloop

				case <-s.ctx.Done():
					return
				}
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *WsTransport) pingSender(conn *TunnelChannel) {
	ticker := time.NewTicker(s.heartbeatDuration) // Send periodic pings to the client

	defer ticker.Stop()

	for {
		select {
		case <-conn.ping:
			s.logger.Trace("Ping channel closed")
			return
		case <-ticker.C:
			// Try to acquire the lock without blocking
			locked := conn.mu.TryLock()
			if !locked {
				// If the lock is held by another operation, stop the pingSender
				s.logger.Trace("Write operation in progress, stopping pingSender")
				return
			}

			if err := conn.conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
				conn.mu.Unlock()
				conn.conn.Close()
				return
			}
			conn.mu.Unlock()
			s.logger.Trace("Ping sent to the client")
		}
	}
}
