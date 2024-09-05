package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config            *TcpConfig
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	tunnelChannel     chan net.Conn
	getNewConnChan    chan struct{}
	controlChannel    net.Conn
	restartMutex      sync.Mutex
	timeout           time.Duration
	heartbeatDuration time.Duration
	heartbeatSig      string
	chanSignal        string
}

type TcpConfig struct {
	BindAddr       string
	Nodelay        bool
	KeepAlive      time.Duration
	ConnectionPool int
	Token          string
	ChannelSize    int
	Ports          []string
}

func NewTCPServer(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &TcpTransport{
		config:            config,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		tunnelChannel:     make(chan net.Conn, config.ChannelSize),
		getNewConnChan:    make(chan struct{}, config.ChannelSize),
		controlChannel:    nil,              // will be set when a control connection is established
		timeout:           2 * time.Second,  // Default timeout
		heartbeatDuration: 30 * time.Second, // Default heartbeat duration
		heartbeatSig:      "0",              // Default heartbeat signal
		chanSignal:        "1",              // Default channel signal
	}

	return server
}

func (s *TcpTransport) Restart() {
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
	s.tunnelChannel = make(chan net.Conn, s.config.ChannelSize)
	s.getNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.controlChannel = nil

	go s.TunnelListener()

}

func (s *TcpTransport) portConfigReader() {
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

func (s *TcpTransport) TunnelListener() {
	listener, err := net.Listen("tcp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", s.config.BindAddr, err)
		return
	}

	// close the tun listener after context cancellation
	defer listener.Close()

	s.logger.Infof("server successfully started, listening on address: %s", listener.Addr().String())

	// try to establish a new channel
	if s.controlChannel == nil {
		go s.channelListener()
	}

	go func() {
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

				// new idea to drop all illegal packets
				if s.controlChannel != nil && s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String() != tcpConn.RemoteAddr().(*net.TCPAddr).IP.String() {
					s.logger.Warnf("suspicious packet from %v. expected address: %v. discarding packet...", tcpConn.RemoteAddr().(*net.TCPAddr).IP.String(), s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String())
					tcpConn.Close()
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
				case s.tunnelChannel <- conn:
					s.logger.Debugf("accepted incoming tunnel TCP connection from %s", tcpConn.RemoteAddr().String())

				case <-time.After(s.timeout): // Tunnel channel is full, discard the connection
					s.logger.Warnf("tunnel channel is full, discarding TCP connection from %s", tcpConn.LocalAddr().String())
					conn.Close()
				}
			}
		}
	}()

	<-s.ctx.Done()
}

func (s *TcpTransport) channelListener() {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			s.logger.Warn("control channel in not found, trying to establish a new session")
			incomingConnection := <-s.tunnelChannel
			msg, err := utils.ReceiveBinaryString(incomingConnection)
			if err != nil {
				s.logger.Error("error recieving channel signal")
				continue
			}

			if msg != s.config.Token {
				s.logger.Error("invalid security token")
				continue
			}

			err = utils.SendBinaryString(incomingConnection, s.config.Token)
			if err != nil {
				s.logger.Error("unable to send security token")
				continue
			}

			s.controlChannel = incomingConnection

			s.logger.Info("control channel established successfully")

			// call the functions
			go s.getNewConnection()
			go s.heartbeat()
			go s.poolChecker()
			go s.portConfigReader()
			return
		}
	}
}

func (s *TcpTransport) heartbeat() {
	ticker := time.NewTicker(s.heartbeatDuration)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.controlChannel == nil {
				go s.Restart()
				return
			}
			err := utils.SendBinaryString(s.controlChannel, s.heartbeatSig)
			if err != nil {
				s.logger.Error("unable to send heartbeat. restarting server...")
				go s.Restart()
				return
			}
			s.logger.Debug("heartbeat sent successfully")
		}
	}
}

func (s *TcpTransport) poolChecker() {
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
		time.Sleep(time.Millisecond * 200)
	}
}

func (s *TcpTransport) getNewConnection() {
	for {
		select {
		case <-s.ctx.Done():
			return

		case <-s.getNewConnChan:
			err := utils.SendBinaryString(s.controlChannel, s.chanSignal)
			if err != nil {
				s.logger.Error("error sending channel signal")
				go s.Restart()
				return
			}
		}
	}
}

func (s *TcpTransport) localListener(localPort int, remotePort int) {
	addr := fmt.Sprintf(":%d", localPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", addr, err)
		return
	}

	//close the local listener after context cancellation
	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	// make a channel and run the handler
	acceptChan := make(chan net.Conn, s.config.ChannelSize)
	go s.handleTCPSession(remotePort, acceptChan)

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

				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

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
	}()

	<-s.ctx.Done()
}

func (s *TcpTransport) handleTCPSession(remotePort int, acceptChan chan net.Conn) {
	for {
		select {
		case incomingConn := <-acceptChan:
		innerloop:
			for {
				select {
				case tunnelConnection := <-s.tunnelChannel:
					// Send the target port over the connection
					if err := utils.SendBinaryInt(tunnelConnection, uint16(remotePort)); err != nil {
						s.logger.Warnf("%v", err) // failed to send port number
						tunnelConnection.Close()
						continue innerloop
					}
					// Handle data exchange between connections
					go utils.ConnectionHandler(incomingConn, tunnelConnection, s.logger)
					break innerloop

				case <-time.After(s.timeout / 2):
					s.logger.Warn("no available tunnel connection. requesting tunnel connection")
					s.getNewConnChan <- struct{}{}
					continue innerloop

				case <-time.After(s.timeout):
					s.logger.Warn("no available tunnel connection. discard the local connection")
					incomingConn.Close()
					break innerloop

				case <-s.ctx.Done():
					return
				}
			}
		case <-s.ctx.Done():
			return
		}

	}
}
