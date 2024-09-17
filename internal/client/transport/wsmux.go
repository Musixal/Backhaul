package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
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
}

type WsMuxConfig struct {
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
	Sniffer          bool
	WebPort          int
	SnifferLog       string
	TunnelStatus     string
	Mode             config.TransportType
}

func NewWSMuxClient(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &WsMuxTransport{
		config:       config,
		parentctx:    parentCtx,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		smuxSession:  make([]*smux.Session, config.MuxSession),
		timeout:      10 * time.Second, // Default timeout
		usageMonitor: web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return client
}

func (c *WsMuxTransport) Restart() {
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

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.smuxSession = make([]*smux.Session, c.config.MuxSession)
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""

	go c.MuxDialer()

}

func (c *WsMuxTransport) MuxDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (WSMux)"

	for id := 0; id < c.config.MuxSession; id++ {
	innerloop:
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				c.logger.Debugf("initiating new mux session to address %s (session ID: %d)", c.config.RemoteAddr, id)
				// Dial to the tunnel server
				tunnelTCPConn, err := c.wsDialer(c.config.RemoteAddr, "/channel")
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
				session, err := smux.Server(tunnelTCPConn.UnderlyingConn(), &config)
				if err != nil {
					c.logger.Errorf("failed to create mux session: %v", err)
					continue
				}

				c.smuxSession[id] = session
				c.logger.Infof("mux session established successfully (session ID: %d)", id)
				go c.handleMUXStreams(id)
				break innerloop
			}
		}
	}
	c.config.TunnelStatus = "Connected (WSMux)"
}

func (c *WsMuxTransport) handleMUXStreams(id int) {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			stream, err := c.smuxSession[id].AcceptStream()
			if err != nil {
				c.logger.Errorf("failed to accept mux stream for session ID %d: %v", id, err)
				c.logger.Info("attempting to restart client...")
				go c.Restart()
				return

			}
			go c.handleTCPSession(stream)
		}
	}
}

func (c *WsMuxTransport) handleTCPSession(tcpsession net.Conn) {
	select {
	case <-c.ctx.Done():
		return
	default:
		port, err := utils.ReceiveBinaryInt(tcpsession)

		if err != nil {
			c.logger.Tracef("unable to get the port from the %s connection: %v", tcpsession.RemoteAddr().String(), err)
			tcpsession.Close()
			return
		}
		go c.localDialer(tcpsession, port)

	}
}

func (c *WsMuxTransport) localDialer(tunnelConnection net.Conn, port uint16) {
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
			c.logger.Errorf("failed to connect to local address %s: %v", localAddress, err)
			tunnelConnection.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", localAddress)
		go utils.ConnectionHandler(localConnection, tunnelConnection, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
	}
}

func (c *WsMuxTransport) tcpDialer(address string, tcpnodelay bool) (*net.TCPConn, error) {
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

	if !tcpnodelay {
		err = tcpConn.SetNoDelay(false)
		if err != nil {
			tcpConn.Close()
			return nil, err
		}
	}

	return tcpConn, nil
}

func (c *WsMuxTransport) wsDialer(addr string, path string) (*websocket.Conn, error) {
	// Create a TLS configuration that allows insecure connections
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // Skip server certificate verification
	}

	// Setup headers with authorization
	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %v", c.config.Token))

	var wsURL string
	dialer := websocket.Dialer{}
	if c.config.Mode == config.WSMUX {
		wsURL = fmt.Sprintf("ws://%s%s", addr, path)
		dialer = websocket.Dialer{
			HandshakeTimeout: c.timeout, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout("tcp", addr, c.timeout)
				if err != nil {
					return nil, err
				}
				tcpConn := conn.(*net.TCPConn)
				tcpConn.SetKeepAlive(true)                     // Enable TCP keepalive
				tcpConn.SetKeepAlivePeriod(c.config.KeepAlive) // Set keepalive period
				return tcpConn, nil
			},
		}
	} else {
		wsURL = fmt.Sprintf("wss://%s%s", addr, path)
		dialer = websocket.Dialer{
			TLSClientConfig:  tlsConfig, // Pass the insecure TLS config here
			HandshakeTimeout: c.timeout, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout("tcp", addr, c.timeout)
				if err != nil {
					return nil, err
				}
				tcpConn := conn.(*net.TCPConn)
				tcpConn.SetKeepAlive(true)                     // Enable TCP keepalive
				tcpConn.SetKeepAlivePeriod(c.config.KeepAlive) // Set keepalive period
				return tcpConn, nil
			},
		}
	}

	// Dial to the WebSocket server
	tunnelWSConn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return nil, err
	}

	return tunnelWSConn, nil
}
