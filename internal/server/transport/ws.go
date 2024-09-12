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

	"github.com/musix/backhaul/internal/utils"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsTransport struct {
	config            *WsConfig
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	tunnelChannel     chan *websocket.Conn
	getNewConnChan    chan struct{}
	controlChannel    *websocket.Conn
	restartMutex      sync.Mutex
	timeout           time.Duration
	heartbeatDuration time.Duration
	heartbeatSig      string
	chanSignal        string
	mu                sync.Mutex
	usageMonitor      *utils.Usage
}

type WsConfig struct {
	BindAddr       string
	Nodelay        bool
	KeepAlive      time.Duration
	ConnectionPool int
	Token          string
	ChannelSize    int
	Ports          []string
	Sniffing       bool
	WebPort        int
	SnifferLog     string
}

func NewWSServer(parentCtx context.Context, config *WsConfig, logger *logrus.Logger) *WsTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &WsTransport{
		config:            config,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		tunnelChannel:     make(chan *websocket.Conn, config.ChannelSize),
		getNewConnChan:    make(chan struct{}, config.ChannelSize),
		controlChannel:    nil,              // will be set when a control connection is established
		timeout:           2 * time.Second,  // Default timeout
		heartbeatDuration: 30 * time.Second, // Default heartbeat duration
		heartbeatSig:      "0",              // Default heartbeat signal
		chanSignal:        "1",              // Default channel signal
		usageMonitor:      utils.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, logger),
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

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan *websocket.Conn, s.config.ChannelSize)
	s.getNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = utils.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.logger)

	go s.TunnelListener()

}
func (s *WsTransport) portConfigReader() {
	// port mapping for listening on each local port
	for _, portMapping := range s.config.Ports {
		parts := strings.Split(portMapping, "=")
		if len(parts) != 2 {
			s.logger.Fatalf("invalid port mapping format: %s", portMapping)
			continue
		}

		localAddrStr := strings.TrimSpace(parts[0])
		// Check if localAddrStr is just a port (without an address)
		if _, err := strconv.Atoi(localAddrStr); err == nil {
			// If it's just a port, prefix it with ":"
			localAddrStr = ":" + localAddrStr
		}

		remotePortStr := strings.TrimSpace(parts[1])
		remotePort, err := strconv.Atoi(remotePortStr)
		if err != nil {
			s.logger.Fatalf("invalid remote port in mapping: %s", remotePortStr)
			continue
		}

		go s.localListener(localAddrStr, remotePort)
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
			s.mu.Lock()
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.heartbeatSig))
			s.mu.Unlock()
			if err != nil {
				s.logger.Errorf("Failed to send heartbeat signal. Error: %v. Restarting server...", err)
				go s.Restart()
				return
			}
			s.logger.Debug("heartbeat signal sent successfully")
		}
	}
}

func (s *WsTransport) poolChecker() {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			if len(s.tunnelChannel) < s.config.ConnectionPool {
				for i := 0; i < s.config.ConnectionPool-len(s.tunnelChannel); i++ {
					s.getNewConnChan <- struct{}{}
				}
			}
		}
		// Time interval to check pool status
		time.Sleep(time.Millisecond * 300)
	}
}

func (s *WsTransport) getNewConnection() {
	for {
		select {
		case <-s.ctx.Done():
			return

		case <-s.getNewConnChan:
			s.mu.Lock()
			err := s.controlChannel.WriteMessage(websocket.TextMessage, []byte(s.chanSignal))
			s.mu.Unlock()
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}
		}
	}
}

func (s *WsTransport) TunnelListener() {
	addr := s.config.BindAddr

	upgrader := websocket.Upgrader{
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
				go s.poolChecker()
				go s.portConfigReader()

				if s.config.Sniffing {
					go s.usageMonitor.Monitor()
				}

				return
			}

			select {
			case s.tunnelChannel <- conn:
				s.logger.Debugf("websocket connection accepted from %s", conn.RemoteAddr().String())
			default:
				s.logger.Warnf("websocket tunnel channel is full, closing connection from %s", conn.RemoteAddr().String())
				conn.Close()
			}
		}),
	}

	go func() {
		s.logger.Infof("websocket server starting, listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Fatalf("failed to listen on %s: %v", addr, err)
		}
	}()

	<-s.ctx.Done()

	// Gracefully shutdown the server
	s.logger.Infof("shutting down the webSocket server on %s", addr)
	if err := server.Shutdown(context.Background()); err != nil {
		s.logger.Errorf("Failed to gracefully shutdown the server: %v", err)
	}
}

func (s *WsTransport) localListener(localAddr string, remotePort int) {
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
	go s.handleWSSession(remotePort, acceptChan)

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
			if s.config.Nodelay {
				if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
					s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP_NODELAY enabled for %s", tcpConn.RemoteAddr().String())
				}
			}
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(s.config.KeepAlive)

			if len(s.tunnelChannel) < s.config.ConnectionPool {
				s.getNewConnChan <- struct{}{}
			}

			select {
			case acceptChan <- tcpConn:
				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			case <-time.After(s.timeout): // channel is full, discard the connection
				s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
				tcpConn.Close()
			}
		}
	}
}

func (s *WsTransport) handleWSSession(remotePort int, acceptChan chan net.Conn) {
	for {
		select {
		case incomingConn := <-acceptChan:
		innerloop:
			for {
				select {
				case tunnelConnection := <-s.tunnelChannel:
					// Send the target port over the WebSocket connection
					if err := utils.SendWebSocketInt(tunnelConnection, uint16(remotePort)); err != nil {
						s.logger.Warnf("%v", err) // failed to send port number
						tunnelConnection.Close()
						continue innerloop
					}
					// Handle data exchange between connections
					go utils.WSToTCPConnHandler(tunnelConnection, incomingConn, s.logger, s.usageMonitor, incomingConn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffing)
					break innerloop

				case <-time.After(s.timeout):
					s.logger.Warn("tunnel connection unavailable, attempting to restart server...")
					incomingConn.Close()
					go s.Restart()
					return

				case <-s.ctx.Done():
					return
				}
			}
		case <-s.ctx.Done():
			return
		}
	}
}
