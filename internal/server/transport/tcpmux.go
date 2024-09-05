package transport

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"

	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

type TcpMuxTransport struct {
	config       *TcpMuxConfig
	ctx          context.Context
	cancel       context.CancelFunc
	logger       *logrus.Logger
	smuxSession  []*smux.Session
	restartMutex sync.Mutex
	timeout      time.Duration
}

type TcpMuxConfig struct {
	BindAddr    string
	Nodelay     bool
	KeepAlive   time.Duration
	Token       string
	MuxSession  int
	ChannelSize int
	Ports       []string
}

func NewTcpMuxServer(parentCtx context.Context, config *TcpMuxConfig, logger *logrus.Logger) *TcpMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &TcpMuxTransport{
		config:      config,
		ctx:         ctx,
		cancel:      cancel,
		logger:      logger,
		timeout:     2 * time.Second, // Default timeout
		smuxSession: make([]*smux.Session, config.MuxSession),
	}

	return server
}

func (s *TcpMuxTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server is already restarting")
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
	s.smuxSession = make([]*smux.Session, s.config.MuxSession)

	go s.TunnelListener()

}

func (s *TcpMuxTransport) portConfigReader() {
	// port mapping for listening on each local port
	for _, portMapping := range s.config.Ports {
		parts := strings.Split(portMapping, "=")
		if len(parts) != 2 {
			s.logger.Fatalf("invalid port mapping: %s", portMapping)
			continue
		}

		localPortStr := strings.TrimSpace(parts[0])
		localPort, err := strconv.Atoi(localPortStr)
		if err != nil {
			s.logger.Fatalf("invalid local port in mapping: %s", localPortStr)
			continue
		}

		remotePortStr := strings.TrimSpace(parts[1])
		remotePort, err := strconv.Atoi(remotePortStr)
		if err != nil {
			s.logger.Fatalf("invalid remote port in mapping: %s", remotePortStr)
			continue
		}
		go s.localListener(localPort, remotePort)
	}
}
func (s *TcpMuxTransport) TunnelListener() {
	tunnelListener, err := net.Listen("tcp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", s.config.BindAddr, err)
		return
	}

	// close the tun listener after context cancellation
	defer tunnelListener.Close()

	s.logger.Infof("server successfully started, listening on address: %s", tunnelListener.Addr().String())

	var wg sync.WaitGroup
	for id := 0; id < s.config.MuxSession; id++ {
		wg.Add(1)
		go s.acceptStreamConn(tunnelListener, id, &wg)
	}
	wg.Wait()

	go s.portConfigReader()

	<-s.ctx.Done()
}

func (s *TcpMuxTransport) acceptStreamConn(listener net.Listener, id int, wg *sync.WaitGroup) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debugf("waiting to accept incoming tunnel connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept tunnel connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			//discard any non tcp connection
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("disarded non-TCP tunnel connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			// trying to enable tcpnodelay
			if s.config.Nodelay {
				if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
					s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP_NODELAY enabled successfully for %s", tcpConn.RemoteAddr().String())
				}
			}

			// config fot smux
			config := smux.Config{
				Version:           2,                // Smux protocol version
				KeepAliveInterval: 10 * time.Second, // Shorter keep-alive interval to quickly detect dead peers
				KeepAliveTimeout:  30 * time.Second, // Aggressive timeout to handle unresponsive connections
				MaxFrameSize:      8 * 1024,         // Smaller frame size to reduce latency and avoid fragmentation
				MaxReceiveBuffer:  4 * 1024 * 1024,  // 8MB buffer to balance memory usage and throughput
				MaxStreamBuffer:   1 * 1024 * 1024,  // 2MB buffer per stream for better latency
			}
			// smux server
			session, err := smux.Client(conn, &config)
			if err != nil {
				s.logger.Errorf("failed to create smux session: %v", err)
				conn.Close()
				continue
			}

			// auth
			stream, err := session.AcceptStream()
			if err != nil {
				s.logger.Errorf("failed to accept mux stream for auth: %v", err)
				session.Close()
				continue

			}
			token, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				s.logger.Error("failed to receive token")
				session.Close()
				continue
			}
			if token == s.config.Token {
				err = utils.SendBinaryString(stream, "ok")
				if err != nil {
					s.logger.Error("failed to reply to the token")
					session.Close()
					continue
				}
				s.smuxSession[id] = session
				s.logger.Info("mux sessions established successfully")

				// Gracefull shutdown
				defer session.Close()
				wg.Done()
				<-s.ctx.Done()
				return

			} else {
				_ = utils.SendBinaryString(stream, "error")
				s.logger.Error("failed to establish a new session. token error")
				session.Close()
				// for safety
				time.Sleep(2 * time.Second)
			}
		}
	}
}

func (s *TcpMuxTransport) localListener(localPort int, remotePort int) {
	addr := fmt.Sprintf("0.0.0.0:%d", localPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", addr, err)
		return
	}

	//close the local listener after context cancellation
	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	// channel
	acceptChan := make(chan net.Conn, s.config.ChannelSize)

	// handle channel connections
	go s.handleMUXSession(acceptChan, remotePort)

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
				if s.config.Nodelay {
					if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
						s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
					} else {
						s.logger.Tracef("TCP_NODELAY enabled successfully for %s", tcpConn.RemoteAddr().String())
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

func (s *TcpMuxTransport) handleMUXSession(acceptChan chan net.Conn, remotePort int) {
	for {
		select {
		case incomingConn := <-acceptChan:
			id := rand.Intn(s.config.MuxSession)
			if s.smuxSession[id] == nil || s.smuxSession[id].IsClosed() {
				s.logger.Error("tcpmux session is closed. Discarding connection.")
				incomingConn.Close()
				go s.Restart()
				return
			}

			stream, err := s.smuxSession[id].OpenStream()
			if err != nil {
				s.logger.Error("unable to open a new mux stream")
				incomingConn.Close()
				go s.Restart()
				return
			}
			// Send the target port over the connection
			if err := utils.SendBinaryInt(stream, uint16(remotePort)); err != nil {
				s.logger.Warnf("failed to send port: %d, error: %v", remotePort, err)
				incomingConn.Close()
				continue
			}

			go utils.ConnectionHandler(stream, incomingConn, s.logger)

		case <-s.ctx.Done():
			return
		}
	}
}
