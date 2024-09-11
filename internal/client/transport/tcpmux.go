package transport

import (
	"context"
	"fmt"
	"net"
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
	usageMonitor *utils.Usage
}

type TcpMuxConfig struct {
	RemoteAddr       string
	Nodelay          bool
	KeepAlive        time.Duration
	RetryInterval    time.Duration
	Token            string
	MuxSession       int
	Forwarder        map[int]string
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	Sniffing         bool
	WebPort          int
	SnifferLog       string
}

func NewMuxClient(parentCtx context.Context, config *TcpMuxConfig, logger *logrus.Logger) *TcpMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpMuxTransport{
		config:       config,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		smuxSession:  make([]*smux.Session, config.MuxSession),
		timeout:      5 * time.Second, // Default timeout
		usageMonitor: utils.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, logger),
	}

	return client
}

func (c *TcpMuxTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")
	if c.cancel != nil {
		c.cancel()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.smuxSession = make([]*smux.Session, c.config.MuxSession)
	c.usageMonitor = utils.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.logger)

	go c.MuxDialer()

}

func (c *TcpMuxTransport) MuxDialer() {
	for id := 0; id < c.config.MuxSession; id++ {
	innerloop:
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				c.logger.Debugf("initiating new mux session to address %s (session ID: %d)", c.config.RemoteAddr, id)
				// Dial to the tunnel server
				tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr, c.config.Nodelay)
				if err != nil {
					c.logger.Errorf("failed to dial tunnel server at %s: %v", c.config.RemoteAddr, err)
					time.Sleep(c.config.RetryInterval)
					continue
				}

				// config fot smux
				config := smux.Config{
					Version:           c.config.MuxVersion, // Smux protocol version
					KeepAliveInterval: 10 * time.Second,    // Shorter keep-alive interval to quickly detect dead peers
					KeepAliveTimeout:  30 * time.Second,    // Aggressive timeout to handle unresponsive connections
					MaxFrameSize:      c.config.MaxFrameSize,
					MaxReceiveBuffer:  c.config.MaxReceiveBuffer,
					MaxStreamBuffer:   c.config.MaxStreamBuffer,
				}

				// SMUX server
				session, err := smux.Server(tunnelTCPConn, &config)
				if err != nil {
					c.logger.Errorf("failed to create mux session: %v", err)
					continue
				}
				// auth
				stream, err := session.OpenStream()
				if err != nil {
					c.logger.Errorf("unable to open a new mux stream for auth: %v", err)
					session.Close()
					continue
				}

				err = utils.SendBinaryString(stream, c.config.Token)
				if err != nil {
					c.logger.Errorf("Failed to send token: %v", err)
					session.Close()
					continue
				}

				msg, err := utils.ReceiveBinaryString(stream)
				if err == nil && msg == "ok" {
					c.smuxSession[id] = session
					c.logger.Infof("Mux session established successfully (session ID: %d)", id)
					go c.handleMUXStreams(id)
					break innerloop
				} else {
					c.logger.Errorf("Failed to establish a new session. Token error or unexpected response: %v", err)
				}

			}
		}
	}

	if c.config.Sniffing {
		go c.usageMonitor.Monitor()
	}
}

func (c *TcpMuxTransport) handleMUXStreams(id int) {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			stream, err := c.smuxSession[id].AcceptStream()
			if err != nil {
				c.logger.Errorf("Failed to accept mux stream for session ID %d: %v", id, err)
				c.logger.Info("attempting to restart client...")
				go c.Restart()
				return

			}
			go c.handleTCPSession(stream)
		}
	}
}

func (c *TcpMuxTransport) tcpDialer(address string, tcpnodelay bool) (*net.TCPConn, error) {
	// Resolve the address to a TCP address
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}

	// options
	dialer := &net.Dialer{
		Timeout:   c.timeout,          // Set the connection timeout
		KeepAlive: c.config.KeepAlive, // Set the keep-alive duration
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

	if tcpnodelay {
		// Enable TCP_NODELAY
		err = tcpConn.SetNoDelay(true)
		if err != nil {
			tcpConn.Close()
			return nil, err
		}
	}

	return tcpConn, nil
}

func (c *TcpMuxTransport) handleTCPSession(tcpsession net.Conn) {
	select {
	case <-c.ctx.Done():
		return
	default:
		port, err := utils.ReceiveBinaryInt(tcpsession)

		if err != nil {
			c.logger.Tracef("Unable to get the port from the %s connection: %v", tcpsession.RemoteAddr().String(), err)
			tcpsession.Close()
			return
		}
		go c.localDialer(tcpsession, port)

	}
}

func (c *TcpMuxTransport) localDialer(tunnelConnection net.Conn, port uint16) {
	select {
	case <-c.ctx.Done():
		return
	default:
		localAddress, ok := c.config.Forwarder[int(port)]
		if !ok {
			localAddress = fmt.Sprintf("127.0.0.1:%d", port)
		}

		localConnection, err := c.tcpDialer(localAddress, c.config.Nodelay)
		if err != nil {
			c.logger.Errorf("Failed to connect to local address %s: %v", localAddress, err)
			tunnelConnection.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", localAddress)
		go utils.ConnectionHandler(localConnection, tunnelConnection, c.logger, c.usageMonitor, int(port), c.config.Sniffing)
	}
}
