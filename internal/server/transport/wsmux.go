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
	"github.com/xtaci/smux"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsMuxTransport struct {
	config         *WsMuxConfig
	smuxConfig     *smux.Config
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChannel  chan *websocket.Conn
	controlChannel *websocket.Conn
	getNewConnChan chan struct{}
	heartbeatSig   string
	chanSignal     string
	usageMonitor   *web.Usage
	restartMutex   sync.Mutex
}

type WsMuxConfig struct {
	BindAddr         string
	Nodelay          bool
	KeepAlive        time.Duration
	Token            string
	ChannelSize      int
	Ports            []string
	MuxCon           int
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	Sniffer          bool
	WebPort          int
	SnifferLog       string
	TLSCertFile      string               // Path to the TLS certificate file
	TLSKeyFile       string               // Path to the TLS key file
	Mode             config.TransportType // ws or wss
	Heartbeat        time.Duration        // in seconds
	TunnelStatus     string
}

func NewWSMuxServer(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           config.MuxVersion,
			KeepAliveInterval: 10 * time.Second,
			KeepAliveTimeout:  30 * time.Second,
			MaxFrameSize:      config.MaxFrameSize,
			MaxReceiveBuffer:  config.MaxReceiveBuffer,
			MaxStreamBuffer:   config.MaxStreamBuffer,
		},
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChannel:  make(chan *websocket.Conn, config.ChannelSize),
		getNewConnChan: make(chan struct{}, config.ChannelSize),
		controlChannel: nil, // will be set when a control connection is established
		heartbeatSig:   "0", // Default heartbeat signal
		chanSignal:     "1", // Default channel signal
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
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

	// Close any open connections in the tunnel channel.
	s.controlChannel.Close()

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan *websocket.Conn, s.config.ChannelSize)
	s.getNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""

	go s.TunnelListener()

}

func (s *WsMuxTransport) getClosedSignal() {
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
				s.logger.Info("wsmux control channel has been closed by the client")
				go s.Restart()
				return
			}
		}
	}

}

func (s *WsMuxTransport) portConfigReader() {
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

func (s *WsMuxTransport) heartbeat() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.controlChannel == nil {
				s.logger.Warn("wsmux control channel is nil. Restarting server to re-establish connection...")
				go s.Restart()
				return
			}
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.heartbeatSig))
			if err != nil {
				s.logger.Errorf("failed to send heartbeat signal. Error: %v. Restarting server...", err)
				go s.Restart()
				return
			}
			s.logger.Debug("heartbeat signal sent successfully")

		case <-s.getNewConnChan:
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.chanSignal))
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}
		}
	}
}

func (s *WsMuxTransport) TunnelListener() {
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

				s.logger.Info("wsmux control channel established successfully")

				go s.heartbeat()
				go s.portConfigReader()
				go s.getClosedSignal()

				s.config.TunnelStatus = "Connected (Websocket)"

			} else if r.URL.Path == "/tunnel" {
				select {
				case s.tunnelChannel <- conn: // add conn to the tunnel channel
				default: // The channel is full, do nothing
					s.logger.Warn("tunnel channel is full, discard the connection")
					conn.Close()
				}
			}
		}),
	}

	if s.config.Mode == config.WSMUX {
		go func() {
			s.logger.Infof("wsmux server starting, listening on %s", addr)
			if s.controlChannel == nil {
				s.logger.Info("waiting for wsmux control channel connection")
			}
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	} else {
		go func() {
			s.logger.Infof("wssmux server starting, listening on %s", addr)
			if s.controlChannel == nil {
				s.logger.Info("waiting for wssmux control channel connection")
			}
			if err := server.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile); err != nil && err != http.ErrServerClosed {
				s.logger.Fatalf("failed to listen on %s: %v", addr, err)
			}
		}()
	}

	<-s.ctx.Done()

	close(s.tunnelChannel)

	// Gracefully shutdown the server
	s.logger.Infof("shutting down the websocket server on %s", addr)
	if err := server.Shutdown(context.Background()); err != nil {
		s.logger.Errorf("Failed to gracefully shutdown the server: %v", err)
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

	localChannel := make(chan net.Conn, s.config.ChannelSize)
	go s.handleLocalChan(localChannel, remoteAddr)

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())
	// start accepting incoming connections
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
				case localChannel <- conn:
					s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

				default: // channel is full, discard the connection
					s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
					tcpConn.Close()
				}
			}
		}
	}()

	<-s.ctx.Done()
	close(localChannel)
}

func (s *WsMuxTransport) handleLocalChan(localChan chan net.Conn, remoteAddr string) {
	for tunnelConn := range s.tunnelChannel {
		session, err := smux.Client(tunnelConn.NetConn(), s.smuxConfig)
		if err != nil {
			s.logger.Errorf("failed to create MUX session for connection %s: %v", tunnelConn.RemoteAddr().String(), err)
			tunnelConn.Close()
			return
		}
		next := make(chan struct{})
		go s.handleSession(session, localChan, remoteAddr, next)
		<-next
	}
}

func (s *WsMuxTransport) handleSession(session *smux.Session, localChan chan net.Conn, remoteAddr string, next chan struct{}) {
	counter := 0
	done := make(chan struct{}, s.config.MuxCon)

	for incomingConn := range localChan {
		stream, err := session.OpenStream()
		if err != nil {
			s.logger.Errorf("failed to open a new mux stream: %v", err)
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after openstream error: %v", err)
			}
			localChan <- incomingConn // back to local channel
			close(next)
			return
		}

		// Send the target port over the tunnel connection
		if err := utils.SendBinaryString(stream, remoteAddr); err != nil {
			s.logger.Errorf("failed to send address %v over stream: %v", remoteAddr, err)
			localChan <- incomingConn // back to local channel
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after sendbinarystring: %v", err)
			}
			close(next)
			return
		}

		// Handle data exchange between connections
		go func() {
			utils.TCPConnectionHandler(stream, incomingConn, s.logger, s.usageMonitor, incomingConn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
			done <- struct{}{}
		}()

		counter += 1

		if counter == s.config.MuxCon {
			close(next)

			select {
			case s.getNewConnChan <- struct{}{}: // Successfully requested a new connection
			default: // The channel is full, do nothing
				s.logger.Warn("getNewConnChan is full, cannot request a new connection")
			}

			for i := 0; i < s.config.MuxCon; i++ {
				<-done
			}
			close(done)
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after mux completed: %v", err)
			}
			return
		}
	}
}
