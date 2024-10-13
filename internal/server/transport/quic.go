package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"
	"github.com/quic-go/quic-go"

	"github.com/sirupsen/logrus"
)

type QuicTransport struct {
	config         *QuicConfig
	quicConfig     *quic.Config
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChan     chan quic.Connection
	localChan      chan LocalTCPConn
	getNewConnChan chan struct{}
	controlChannel quic.Connection
	usageMonitor   *web.Usage
	restartMutex   sync.Mutex
	coldStart      bool
}

type QuicConfig struct {
	BindAddr     string
	TunnelStatus string
	SnifferLog   string
	Token        string
	Ports        []string
	Nodelay      bool
	Sniffer      bool
	ChannelSize  int
	MuxCon       int
	WebPort      int
	KeepAlive    time.Duration
	Heartbeat    time.Duration // in seconds
	TLSCertFile  string        // Path to the TLS certificate file
	TLSKeyFile   string        // Path to the TLS key file

}

func NewQuicServer(parentCtx context.Context, config *QuicConfig, logger *logrus.Logger) *QuicTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &QuicTransport{
		quicConfig: &quic.Config{
			Allow0RTT:       true,
			KeepAlivePeriod: 20 * time.Second,
			MaxIdleTimeout:  1600 * time.Second,
		},
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChan:     make(chan quic.Connection, config.ChannelSize),
		getNewConnChan: make(chan struct{}, config.ChannelSize),
		localChan:      make(chan LocalTCPConn, config.ChannelSize),
		controlChannel: nil, // will be set when a control connection is established
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		coldStart:      true,
	}

	return server
}

func (s *QuicTransport) Restart() {
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
	// if s.controlChannel != nil {
	// 	s.controlChannel.Close()
	// }

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChan = make(chan quic.Connection, s.config.ChannelSize)
	s.getNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.localChan = make(chan LocalTCPConn, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""
	s.coldStart = true

	go s.TunnelListener()

}

func (s *QuicTransport) keepalive() {
	stream, err := s.controlChannel.AcceptStream(context.Background())
	if err != nil {
		s.logger.Error("failed to open stream for keepalive")
		go s.Restart()
		return
	}

	s.coldStart = false

	tickerPing := time.NewTicker(3 * time.Second)
	tickerTimeout := time.NewTicker(300 * time.Second)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()

	// Channel to receive the message or error
	resultChan := make(chan struct {
		message byte
		err     error
	})

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				message := make([]byte, 1)
				_, err := stream.Read(message)

				resultChan <- struct {
					message byte
					err     error
				}{message[0], err}

				if err != nil {
					return
				}
			}
		}
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.getNewConnChan:
			err = utils.SendBinaryByte(stream, utils.SG_Chan)
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				s.controlChannel = nil
				return
			}
		case <-tickerPing.C:
			streamtest, err := s.controlChannel.AcceptStream(context.Background())
			if err != nil {
				s.logger.Error("failed to open stream for keepalive")
				go s.Restart()
				return
			}
			err = utils.SendBinaryByte(streamtest, utils.SG_HB)
			if err != nil {
				s.logger.Error("failed to send keepalive")
				s.controlChannel = nil
				return
			}
			s.logger.Info("heartbeat signal sended successfully")
		case <-tickerTimeout.C:
			s.logger.Error("keepalive timeout")
			s.controlChannel = nil
			return

		case result := <-resultChan:
			if result.err != nil {
				s.logger.Errorf("failed to receive message from channel connection: %v", result.err)
				s.controlChannel = nil
				return
			}

			switch result.message {
			case utils.SG_HB:
				s.logger.Info("heartbeat signal received successfully")
				tickerTimeout.Reset(3 * time.Second)

			case utils.SG_Closed:
				s.logger.Info("control channel has been closed by the client")
				s.controlChannel = nil
				return
			default:
				s.logger.Errorf("unexpected response from channel: %v. Restarting client...", result.message)
				s.controlChannel = nil
				return
			}

		}

	}
}

func (s *QuicTransport) portConfigReader() {
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

func (s *QuicTransport) channelHandshake(qConn quic.Connection) {
	// Set a read deadline for the token response
	stream, err := qConn.AcceptStream(context.Background())
	if err != nil {
		s.logger.Error("failed to open stream for channel handshake: ", err)
		qConn.CloseWithError(1, "failed to open stream")
		return
	}

	if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		s.logger.Errorf("failed to set read deadline: %v", err)
		stream.Close()
		qConn.CloseWithError(1, "failed to set deadline")
		return
	}
	msg, err := utils.ReceiveBinaryString(stream)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			s.logger.Warn("timeout while waiting for control channel signal")
		} else {
			s.logger.Errorf("failed to receive control channel signal: %v", err)
		}
		stream.Close()
		qConn.CloseWithError(1, "close on timeout/response deadline")
		return
	}

	// Resetting the deadline (removes any existing deadline)
	stream.SetReadDeadline(time.Time{})

	if msg != s.config.Token {
		s.logger.Warnf("invalid security token received: %s, exptected: %s", msg, s.config.Token)
		stream.Close()
		qConn.CloseWithError(1, "close on invalid token")
		return
	}

	err = utils.SendBinaryString(stream, s.config.Token)
	if err != nil {
		s.logger.Errorf("failed to send security token: %v", err)
		stream.Close()
		qConn.CloseWithError(1, "failed to send security token")
		return
	}

	s.controlChannel = qConn

	// close stream
	stream.Close()

	s.logger.Info("QUIC control channel successfully established.")

	// call the functions
	if s.coldStart {
		go s.portConfigReader()
		go s.handleTunConn()
	}
	go s.keepalive()

	s.config.TunnelStatus = "Connected (QUIC)"
}

func (s *QuicTransport) generateTLSConfig() *tls.Config {
	// You should replace this with proper certificate and key files
	cert, err := tls.LoadX509KeyPair(s.config.TLSCertFile, s.config.TLSKeyFile)
	if err != nil {
		log.Fatal(err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3"},
	}
}

func (s *QuicTransport) TunnelListener() {
	// for  webui
	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}
	s.config.TunnelStatus = "Disconnected (QUIC)"

	// Create a UDP connection
	udpAddr, err := net.ResolveUDPAddr("udp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to resolve UDP address: %v", err)
	}

	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on UDP: %v", err)
	}
	defer udpConn.Close()

	// Create a QUIC listener
	listener, err := quic.Listen(udpConn, s.generateTLSConfig(), s.quicConfig)
	if err != nil {
		s.logger.Fatalf("failed to create QUIC listener: %v", err)
	}

	s.logger.Infof("listening for QUIC connections on %s...", s.config.BindAddr)

	defer listener.Close()

	go s.acceptTunCon(listener)

	<-s.ctx.Done()

	if s.controlChannel != nil {
		//s.controlChannel.Close()
	}
}

func (s *QuicTransport) acceptTunCon(listener *quic.Listener) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debugf("waiting for accept incoming tunnel connection on %s", listener.Addr().String())
			conn, err := listener.Accept(context.Background())
			if err != nil {
				s.logger.Debugf("failed to accept tunnel connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			// Drop all suspicious packets from other address rather than server
			if s.controlChannel != nil && s.controlChannel.RemoteAddr().(*net.UDPAddr).IP.String() != conn.RemoteAddr().(*net.UDPAddr).IP.String() {
				s.logger.Debugf("suspicious packet from %v. expected address: %v. discarding packet...", conn.RemoteAddr().(*net.UDPAddr).IP.String(), s.controlChannel.RemoteAddr().(*net.UDPAddr).IP.String())
				//	conn.Close()
				continue
			}

			// try to establish a new channel
			if s.controlChannel == nil {
				s.logger.Info("control channel not found, attempting to establish a new session")
				go s.channelHandshake(conn)
				continue
			}

			select {
			case s.tunnelChan <- conn: // ok
				s.logger.Debugf("accepted tunnel connection from %s", conn.RemoteAddr().String())
			default:
				s.logger.Warnf("tunnel listener channel is full, discarding TCP connection from %s", conn.LocalAddr().String())
			}
		}
	}

}

func (s *QuicTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", localAddr, err)
		return
	}

	//close local listener after context cancellation
	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptLocalCon(listener, remoteAddr)

	<-s.ctx.Done()
}

func (s *QuicTransport) acceptLocalCon(listener net.Listener, remoteAddr string) {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
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

			// trying to disable tcpnodelay
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
			case s.localChan <- LocalTCPConn{conn: conn, remoteAddr: remoteAddr}:
				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default: // channel is full, discard the connection
				s.logger.Warnf("local listener channel is full, discarding TCP connection from %s", tcpConn.LocalAddr().String())
				tcpConn.Close()
			}

		}
	}

}

func (s *QuicTransport) handleTunConn() {
	next := make(chan struct{})
	for {
		select {
		case <-s.ctx.Done():
			return

		case tunConn := <-s.tunnelChan:
			go s.handleSession(tunConn, next)
			<-next
		case <-time.After(1 * time.Second):
			s.logger.Info("no tunnel conn: ", len(s.tunnelChan))
		}
	}
}

func (s *QuicTransport) handleSession(session quic.Connection, next chan struct{}) {
	counter := 0
	done := make(chan struct{}, s.config.MuxCon)

	for {
		select {
		case <-s.ctx.Done():
			return
		case incomingConn := <-s.localChan:
			stream, err := session.OpenStream()
			if err != nil {
				s.logger.Errorf("failed to open a new mux stream: %v", err)
				if err := session.CloseWithError(1, "open stream error"); err != nil {
					s.logger.Errorf("failed to close mux stream: %v", err)
				}
				s.localChan <- incomingConn // back to local channel
				next <- struct{}{}
				return
			}

			// Send the target port over the tunnel connection
			err = utils.SendBinaryString(stream, incomingConn.remoteAddr)
			if err != nil {
				s.logger.Errorf("failed to send address %v over stream: %v", incomingConn.remoteAddr, err)

				if err := session.CloseWithError(1, "send port error"); err != nil {
					s.logger.Errorf("failed to close mux stream: %v", err)
				}
				s.localChan <- incomingConn // back to local channel
				next <- struct{}{}
				return
			}

			// Handle data exchange between connections
			go func() {
				utils.QConnectionHandler(incomingConn.conn, stream, s.logger, s.usageMonitor, incomingConn.conn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
				done <- struct{}{}
			}()

			counter += 1

			if counter == s.config.MuxCon {
				next <- struct{}{}

				select {
				case s.getNewConnChan <- struct{}{}:
					// Successfully requested a new connection
				default:
					// The channel is full, do nothing
					s.logger.Warn("channel is full, cannot request a new connection")
				}

				for i := 0; i < s.config.MuxCon; i++ {
					<-done
				}

				close(done)

				if err := session.CloseWithError(0, "session done"); err != nil {
					s.logger.Errorf("failed to close mux stream after session completed: %v", err)
				}
				return
			}
		}
	}
}
