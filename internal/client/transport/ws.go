package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsTransport struct {
	config         *WsConfig
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	controlChannel *websocket.Conn
	timeout        time.Duration
	restartMutex   sync.Mutex
	heartbeatSig   string
	chanSignal     string
	usageMonitor   *web.Usage
}
type WsConfig struct {
	RemoteAddr    string
	Nodelay       bool
	KeepAlive     time.Duration
	RetryInterval time.Duration
	Token         string
	Forwarder     map[int]string
	Sniffer       bool
	WebPort       int
	SnifferLog    string
	Mode          config.TransportType
	TunnelStatus  string
}

func NewWSClient(parentCtx context.Context, config *WsConfig, logger *logrus.Logger) *WsTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &WsTransport{
		config:         config,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		controlChannel: nil,             // will be set when a control connection is established
		timeout:        5 * time.Second, // Default timeout
		heartbeatSig:   "0",             // Default heartbeat signal
		chanSignal:     "1",             // Default channel signal
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return client
}

func (c *WsTransport) Restart() {
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
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""

	go c.ChannelDialer()

}

func (c *WsTransport) ChannelDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (Websocket)"

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			c.logger.Info("attempting to establish a new websocket control channel connection")

			tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "/channel")
			if err != nil {
				c.logger.Errorf("failed to dial websocket control channel: %v", err)
				time.Sleep(c.config.RetryInterval)
				continue
			}
			c.controlChannel = tunnelWSConn
			c.logger.Info("websocket control channel established successfully")

			c.config.TunnelStatus = "Connected (Websocket)"

			go c.channelListener()

			return
		}
	}
}

func (c *WsTransport) channelListener() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			_, msg, err := c.controlChannel.ReadMessage()
			if err != nil {
				c.logger.Errorf("error receiving channel signal: %v. Restarting client...", err)
				go c.Restart()
				return
			}

			message := string(msg)
			if message == c.chanSignal {
				go c.tunnelDialer()
			} else if message == c.heartbeatSig {
				c.logger.Debug("heartbeat received successfully")
			} else {
				c.logger.Errorf("unexpected response from control channel: %s. Restarting client...", message)
				go c.Restart()
				return
			}
		}
	}
}

func (c *WsTransport) tunnelDialer() {
	select {
	case <-c.ctx.Done():
		return
	default:
		if c.controlChannel == nil {
			c.logger.Warn("websocket control channel is nil, cannot dial tunnel. Restarting client...")
			go c.Restart()
			return
		}
		c.logger.Debugf("initiating new websocket tunnel connection to address %s", c.config.RemoteAddr)

		tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "")
		if err != nil {
			c.logger.Errorf("failed to dial webSocket tunnel server: %v", err)
			return
		}
		go c.handleWSSession(tunnelWSConn)
	}
}

func (c *WsTransport) handleWSSession(wsSession *websocket.Conn) {
loop:
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			_, portBytes, err := wsSession.ReadMessage()

			if err != nil {
				c.logger.Debugf("Unable to get port from websocket connection %s: %v", wsSession.RemoteAddr().String(), err)
				wsSession.Close()
				return
			}

			port := binary.BigEndian.Uint16(portBytes)
			if port == 10 {
				c.logger.Trace("Ping recieved from the server")
				continue loop
			}
			go c.localDialer(wsSession, port)
			break loop
		}
	}
}

func (c *WsTransport) localDialer(tunnelConnection *websocket.Conn, port uint16) {
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
			c.logger.Errorf("connecting to local address %s is not possible", localAddress)
			tunnelConnection.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", localAddress)
		go utils.WSToTCPConnHandler(tunnelConnection, localConnection, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
	}
}

func (c *WsTransport) wsDialer(addr string, path string) (*websocket.Conn, error) {
	// Create a TLS configuration that allows insecure connections
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // Skip server certificate verification
	}

	// Setup headers with authorization
	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %v", c.config.Token))

	var wsURL string
	dialer := websocket.Dialer{}
	if c.config.Mode == config.WS {
		wsURL = fmt.Sprintf("ws://%s%s", addr, path)
		dialer = websocket.Dialer{
			HandshakeTimeout: c.timeout, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := net.Dial("tcp4", addr)
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
				conn, err := net.Dial("tcp4", addr)
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
		c.logger.Errorf("Failed to dial websocket server %s: %v", wsURL, err)
		return nil, err
	}

	return tunnelWSConn, nil
}

func (c *WsTransport) tcpDialer(address string, tcpnodelay bool) (*net.TCPConn, error) {
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
