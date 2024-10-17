package transport

import (
	"context"
	"crypto/tls"
	"fmt"
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
	config            *QuicConfig
	quicConfig        *quic.Config
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	controlChannel    quic.Connection
	usageMonitor      *web.Usage
	activeMu          sync.Mutex
	restartMutex      sync.Mutex
	activeConnections int
}

type QuicConfig struct {
	RemoteAddr       string
	Token            string
	SnifferLog       string
	TunnelStatus     string
	Nodelay          bool
	Sniffer          bool
	KeepAlive        time.Duration
	RetryInterval    time.Duration
	DialTimeOut      time.Duration
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	ConnectionPool   int
	WebPort          int
	AggressivePool bool
}

func NewQuicClient(parentCtx context.Context, config *QuicConfig, logger *logrus.Logger) *QuicTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &QuicTransport{
		quicConfig: &quic.Config{
			Allow0RTT:       true,
			KeepAlivePeriod: 20 * time.Second,
			MaxIdleTimeout:  1600 * time.Second,
		},
		config:            config,
		parentctx:         parentCtx,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		controlChannel:    nil, // will be set when a control connection is established
		activeConnections: 0,
		activeMu:          sync.Mutex{},
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return client
}

func (c *QuicTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")
	if c.cancel != nil {
		c.cancel()
	}

	//Close tunnel channel connection
	c.closeControlChannel("restart")

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.activeConnections = 0
	c.activeMu = sync.Mutex{}

	go c.ChannelDialer(true)

}

func (c *QuicTransport) ChannelDialer(coldStart bool) {
	if coldStart && c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}
	c.config.TunnelStatus = "Disconnected (Quic)"
	c.logger.Info("attempting to establish a new quic control channel connection...")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			qConn, err := c.quicDialer(c.config.RemoteAddr)
			if err != nil {
				c.logger.Errorf("quic channel dialer: error dialing remote address %s: %v", c.config.RemoteAddr, err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			stream, err := qConn.OpenStreamSync(context.Background())
			if err != nil {
				c.logger.Error("failed to open stream for channel handshake: ", err)
				qConn.CloseWithError(1, "failed to open stream")
				continue
			}
			err = utils.SendBinaryString(stream, c.config.Token)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				stream.Close()
				qConn.CloseWithError(1, "failed to send security token")
				continue
			}

			// Set a read deadline for the token response
			if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				c.logger.Errorf("failed to set read deadline: %v", err)
				stream.Close()
				qConn.CloseWithError(1, "failed to set read deadline")
				continue
			}
			// Receive response
			message, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Errorf("failed to receive control channel response: %v", err)
				}
				stream.Close()
				qConn.CloseWithError(1, "close on timeout/response deadline")
				time.Sleep(c.config.RetryInterval)
				continue
			}
			// Resetting the deadline (removes any existing deadline)
			stream.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.controlChannel = qConn
				c.logger.Info("quic control channel established successfully")

				// close stream
				stream.Close()

				c.config.TunnelStatus = "Connected (Quic)"

				go c.channelListener()

				if coldStart {
					go c.poolChecker()
				}

				return
			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				stream.Close()
				qConn.CloseWithError(1, "invalid token error")
				continue
			}
		}
	}

}

func (c *QuicTransport) closeControlChannel(reason string) {
	if c.controlChannel != nil {
		_ = utils.SendBinaryByte(c.controlChannel, utils.SG_Closed)
		c.controlChannel.CloseWithError(0, fmt.Sprintf("control channel closed due to %s", reason))
		c.logger.Debugf("control channel closed due to %s", reason)
	}
}

func (c *QuicTransport) channelListener() {
	stream, err := c.controlChannel.OpenStreamSync(context.Background())
	if err != nil {
		c.logger.Error("failed to open stream in control channel")
		go c.ChannelDialer(false)
		return
	}

	tickerPing := time.NewTicker(3 * time.Second)
	tickerTimeout := time.NewTicker(300 * time.Second)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()

	msg := make([]byte, 1)
	msgChan := make(chan byte, 100)
	errChan := make(chan error, 100)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			_, err := stream.Read(msg)
			if err != nil {
				c.logger.Error("here: ", err)
				errChan <- err
				return
			}
			msgChan <- msg[0]
		}
	}()

	// close channel
	//	defer func() { c.closeControlChannel("context cancellation") }()

	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			return

		case <-tickerPing.C:
			streamtest, err := c.controlChannel.OpenStreamSync(context.Background())
			if err != nil {
				c.logger.Error("failed to open stream in control channel")
				go c.ChannelDialer(false)
				return
			}
			err = utils.SendBinaryByte(streamtest, utils.SG_HB)
			if err != nil {
				c.logger.Error("failed to send keepalive")
				go c.ChannelDialer(false)
				return
			}
			c.logger.Info("heartbeat signal sended successfully")

		case <-tickerTimeout.C:
			c.logger.Error("keepalive timeout")
			go c.ChannelDialer(false)
			return

		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				c.logger.Debug("channel signal received, initiating tunnel dialer")
				go c.tunnelDialer()
			case utils.SG_HB:
				c.logger.Debug("heartbeat signal received successfully")
				tickerTimeout.Reset(3 * time.Second)
			default:
				c.logger.Errorf("unexpected response from channel: %v. Restarting client...", msg)
				go c.ChannelDialer(false)
				return
			}
		case err := <-errChan:
			// Handle errors from the control channel
			c.logger.Error("failed to receive channel signal, restarting client: ", err)
			go c.ChannelDialer(false)
			return
		}
	}
}

func (c *QuicTransport) tunnelDialer() {
	c.activeMu.Lock()
	c.activeConnections++
	c.activeMu.Unlock()

	if c.controlChannel == nil {
		c.logger.Warn("wsmux control channel is nil, cannot dial tunnel. Restarting client...")
		go c.Restart()
		return
	}
	c.logger.Debugf("initiating new wsmux tunnel connection to address %s", c.config.RemoteAddr)

	tunnelConn, err := c.quicDialer(c.config.RemoteAddr)
	if err != nil {
		c.logger.Errorf("failed to dial wsmux tunnel server: %v", err)
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
		return
	}
	c.handleTunnelConn(tunnelConn)
}

func (c *QuicTransport) handleTunnelConn(session quic.Connection) {
	defer func() {
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			stream, err := session.AcceptStream(context.Background())
			if err != nil {
				c.logger.Trace("session is closed: ", err)
				//session.Close()
				return
			}

			remoteAddr, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				c.logger.Errorf("unable to get port from stream connection %s: %v", session.RemoteAddr().String(), err)
				if err := session.CloseWithError(1, "recieve port error"); err != nil {
					c.logger.Errorf("failed to close mux stream: %v", err)
				}
				return
			}

			go c.localDialer(stream, remoteAddr)
		}
	}
}

func (c *QuicTransport) localDialer(stream quic.Stream, remoteAddr string) {
	// Extract the port
	parts := strings.Split(remoteAddr, ":")
	var port int
	var err error
	if len(parts) < 2 {
		port, err = strconv.Atoi(parts[0])
		if err != nil {
			c.logger.Info("failed to find the remote port, ", err)
			stream.Close()
			return
		}
		remoteAddr = fmt.Sprintf("127.0.0.1:%d", port)
	} else {
		port, err = strconv.Atoi(parts[1])
		if err != nil {
			c.logger.Info("failed to find the remote port, ", err)
			stream.Close()
			return
		}
	}
	localConnection, err := c.tcpDialer(remoteAddr)
	if err != nil {
		c.logger.Errorf("connecting to local address %s is not possible", remoteAddr)
		stream.Close()
		return
	}

	c.logger.Debugf("connected to local address %s successfully", remoteAddr)
	utils.QConnectionHandler(localConnection, stream, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
}

func (c *QuicTransport) tcpDialer(address string) (*net.TCPConn, error) {
	// Resolve the address to a TCP address
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}

	// options
	dialer := &net.Dialer{
		Timeout:   c.config.DialTimeOut, // Set the connection timeout
		KeepAlive: c.config.KeepAlive,   // Set the keep-alive duration
	}

	// Dial the TCP connection with a timeout
	conn, err := dialer.Dial("tcp", tcpAddr.String())
	if err != nil {
		return nil, err
	}

	// Type assert the net.Conn to *net.TCPConn
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("failed to convert net.Conn to *net.TCPConn")
	}

	if !c.config.Nodelay {
		err = tcpConn.SetNoDelay(false)
		if err != nil {
			tcpConn.Close()
			return nil, err
		}
	}

	return tcpConn, nil
}

func (c *QuicTransport) poolChecker() {
	ticker := time.NewTicker(time.Millisecond * 350)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-ticker.C:
			c.logger.Tracef("active connections: %d", c.activeConnections)
			if c.activeConnections < c.config.ConnectionPool/2 {
				neededConn := c.config.ConnectionPool - c.activeConnections
				for i := 0; i < neededConn; i++ {
					go c.tunnelDialer()
				}

			}

		}

	}

}

func (c *QuicTransport) generateClientTLSConfig() *tls.Config {
	// Skip certificate verification for simplicity (not recommended for production)
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"}, // Set your supported protocol here
	}
}

// quicDialer establishes a QUIC connection to a given address
func (c *QuicTransport) quicDialer(address string) (quic.Connection, error) {
	// Resolve the address to a UDP address
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %v", err)
	}

	// Create a UDP connection for QUIC to use
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP: %v", err)
	}

	// Set a deadline for the dial operation based on DialTimeout
	err = udpConn.SetDeadline(time.Now().Add(c.config.DialTimeOut))
	if err != nil {
		udpConn.Close()
		return nil, fmt.Errorf("failed to set deadline: %v", err)
	}

	// Dial the QUIC connection
	tlsConfig := c.generateClientTLSConfig()
	quicConn, err := quic.Dial(context.Background(), udpConn, udpAddr, tlsConfig, c.quicConfig)
	if err != nil {
		udpConn.Close()
		return nil, fmt.Errorf("failed to dial QUIC connection: %v", err)
	}

	return quicConn, nil
}
