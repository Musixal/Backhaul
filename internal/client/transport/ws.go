package transport

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"

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
}
type WsConfig struct {
	RemoteAddr    string
	Nodelay       bool
	KeepAlive     time.Duration
	RetryInterval time.Duration
	Token         string
	Forwarder     map[int]string
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

	go c.ChannelDialer()

}

func (c *WsTransport) ChannelDialer() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			c.logger.Info("trying to establish a new ws control channel connection")

			tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "/channel")
			if err != nil {
				c.logger.Error(err)
				time.Sleep(c.config.RetryInterval)
				continue
			}
			c.controlChannel = tunnelWSConn
			c.logger.Info("ws control channel established successfully")
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
				c.logger.Error("error receiving channel signal, restarting client")
				go c.Restart()
				return
			}

			message := string(msg)
			if message == c.chanSignal {
				go c.wsTunDialer()
			} else if message == c.heartbeatSig {
				c.logger.Debug("heartbeat received successfully")
			} else {
				c.logger.Error("weird response from channel, exiting from control channel, restarting client")
				go c.Restart()
				return
			}
		}
	}
}

func (c *WsTransport) wsTunDialer() {
	select {
	case <-c.ctx.Done():
		return
	default:
		if c.controlChannel == nil {
			c.logger.Warn("no ws control channel found...")
			go c.Restart()
			return
		}
		c.logger.Debug("initiating new connection to address ", c.config.RemoteAddr)

		tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "")
		if err != nil {
			c.logger.Error("failed to dial tunnel server: ", err)
			return
		}
		go c.handleWSSession(tunnelWSConn)
	}
}

func (c *WsTransport) handleWSSession(wsSession *websocket.Conn) {
	select {
	case <-c.ctx.Done():
		return
	default:
		_, portBytes, err := wsSession.ReadMessage()

		if err != nil {
			c.logger.Debugf("unable to get the port from the %s connection", wsSession.RemoteAddr().String())
			wsSession.Close()
			return
		}

		port := binary.BigEndian.Uint16(portBytes)
		go c.wsLocDialer(wsSession, port)
	}
}

func (c *WsTransport) wsLocDialer(tunnelConnection *websocket.Conn, port uint16) {
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
			c.logger.Errorf("connecting to the local address %s is not possible", localAddress)
			tunnelConnection.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", localAddress)
		go utils.WSToTCPConnHandler(tunnelConnection, localConnection, c.logger)
	}
}

func (c *WsTransport) wsDialer(addr string, path string) (*websocket.Conn, error) {

	wsURL := fmt.Sprintf("ws://%s%s", addr, path)

	// Setup headers with authorization
	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %v", c.config.Token))

	// Custom dialer with TCP keepalive
	dialer := websocket.Dialer{
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

	// Dial to the WebSocket server
	tunnelWSConn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		c.logger.Error("failed to dial tunnel server: ", err)
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
