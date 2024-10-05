package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsTransport struct {
	config            *WsConfig
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	controlChannel    *websocket.Conn
	restartMutex      sync.Mutex
	usageMonitor      *web.Usage
	activeConnections int32
}
type WsConfig struct {
	RemoteAddr     string
	Token          string
	SnifferLog     string
	TunnelStatus   string
	Nodelay        bool
	Sniffer        bool
	KeepAlive      time.Duration
	RetryInterval  time.Duration
	DialTimeOut    time.Duration
	ConnectionPool int
	WebPort        int
	Mode           config.TransportType
}

func NewWSClient(parentCtx context.Context, config *WsConfig, logger *logrus.Logger) *WsTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &WsTransport{
		config:            config,
		parentctx:         parentCtx,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		controlChannel:    nil, // will be set when a control connection is established
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		activeConnections: 0,
	}

	return client
}

func (c *WsTransport) Start() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = fmt.Sprintf("Disconnected (%s)", c.config.Mode)

	go c.channelDialer()

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

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.activeConnections = 0

	go c.Start()

}

func (c *WsTransport) channelDialer() {
	c.logger.Info("attempting to establish a new websocket control channel connection")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "/channel")
			if err != nil {
				c.logger.Errorf("failed to dial websocket control channel: %v", err)
				time.Sleep(c.config.RetryInterval)
				continue
			}
			c.controlChannel = tunnelWSConn
			c.logger.Info("websocket control channel established successfully")

			c.config.TunnelStatus = fmt.Sprintf("Connected (%s)", c.config.Mode)

			go c.channelHandler()
			go c.poolMaintainer()

			return
		}
	}
}

func (c *WsTransport) poolMaintainer() {
	ticker := time.NewTicker(time.Millisecond * 350)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-ticker.C:
			activeConnections := int(c.activeConnections)
			c.logger.Tracef("active connections: %d", c.activeConnections)
			if activeConnections < c.config.ConnectionPool/2 {
				neededConn := c.config.ConnectionPool - activeConnections
				for i := 0; i < neededConn; i++ {
					go c.tunnelDialer()
				}

			}

		}

	}

}

func (c *WsTransport) channelHandler() {
	msgChan := make(chan byte, 100)
	errChan := make(chan error, 100)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			_, msg, err := c.controlChannel.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			msgChan <- msg[0]
		}
	}()
	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			_ = c.controlChannel.WriteMessage(websocket.BinaryMessage, []byte{utils.SG_Closed})
			return
		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				c.logger.Debug("channel signal received, initiating tunnel dialer")
				go c.tunnelDialer()
			case utils.SG_Closed:
				c.logger.Info("control channel has been closed by the server")
				go c.Restart()
				return
			case utils.SG_HB:
				c.logger.Debug("heartbeat signal received successfully")
			default:
				c.logger.Errorf("unexpected response from channel: %v. Restarting client...", msg)
				go c.Restart()
				return
			}
		case err := <-errChan:
			// Handle errors from the control channel
			c.logger.Error("error receiving channel signal, restarting client: ", err)
			go c.Restart()
			return
		}
	}
}

func (c *WsTransport) tunnelDialer() {
	// Increment active connections counter
	atomic.AddInt32(&c.activeConnections, 1)

	c.logger.Debugf("initiating new websocket tunnel connection to address %s", c.config.RemoteAddr)

	// Dial to the tunnel server
	tunnelConn, err := c.wsDialer(c.config.RemoteAddr, "")
	if err != nil {
		c.logger.Errorf("failed to dial webSocket tunnel server: %v", err)

		// Decrement active connections on failure
		atomic.AddInt32(&c.activeConnections, -1)
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			_, remoteAddrBytes, err := tunnelConn.ReadMessage()
			if err != nil {
				c.logger.Debugf("unable to get port from websocket connection %s: %v", tunnelConn.RemoteAddr().String(), err)
				tunnelConn.Close()

				// Decrement active connections on failure
				atomic.AddInt32(&c.activeConnections, -1)
				return
			}

			if bytes.Equal(remoteAddrBytes, []byte{utils.SG_Ping}) {
				c.logger.Trace("ping received from the server")
				continue
			}

			// Decrement active connections
			atomic.AddInt32(&c.activeConnections, -1)

			remoteAddr := string(remoteAddrBytes)

			// Extract the port from the received address
			port, resolvedAddr, err := ResolveRemoteAddr(remoteAddr)
			if err != nil {
				c.logger.Infof("failed to resolve remote port: %v", err)
				tunnelConn.Close() // Close the connection on error
				return
			}

			c.localDialer(tunnelConn, resolvedAddr, port)
			return
		}
	}
}

func (c *WsTransport) localDialer(tunnelCon *websocket.Conn, remoteAddr string, port int) {
	localConn, err := TcpDialer(remoteAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
	if err != nil {
		c.logger.Errorf("connecting to local address %s is not possible", remoteAddr)
		tunnelCon.Close()
		return
	}
	c.logger.Debugf("connected to local address %s successfully", remoteAddr)

	utils.WSConnectionHandler(tunnelCon, localConn, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
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
			HandshakeTimeout: c.config.DialTimeOut, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := TcpDialer(addr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
				if err != nil {
					return nil, err
				}
				//tcpConn := conn.(*net.TCPConn)
				conn.SetKeepAlive(true)                     // Enable TCP keepalive
				conn.SetKeepAlivePeriod(c.config.KeepAlive) // Set keepalive period
				return conn, nil
			},
		}
	} else {
		wsURL = fmt.Sprintf("wss://%s%s", addr, path)
		dialer = websocket.Dialer{
			TLSClientConfig:  tlsConfig,            // Pass the insecure TLS config here
			HandshakeTimeout: c.config.DialTimeOut, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := TcpDialer(addr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
				if err != nil {
					return nil, err
				}
				conn.SetKeepAlive(true)                     // Enable TCP keepalive
				conn.SetKeepAlivePeriod(c.config.KeepAlive) // Set keepalive period
				return conn, nil
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
