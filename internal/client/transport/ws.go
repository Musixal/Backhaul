package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	heartbeatSig      string
	chanSignal        string
	usageMonitor      *web.Usage
	activeConnections int
	activeMu          sync.Mutex
}
type WsConfig struct {
	RemoteAddr     string
	Nodelay        bool
	KeepAlive      time.Duration
	RetryInterval  time.Duration
	DialTimeOut    time.Duration
	ConnectionPool int
	Token          string
	Sniffer        bool
	WebPort        int
	SnifferLog     string
	Mode           config.TransportType
	TunnelStatus   string
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
		heartbeatSig:      "0", // Default heartbeat signal
		chanSignal:        "1", // Default channel signal
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		activeConnections: 0,
		activeMu:          sync.Mutex{},
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

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.activeConnections = 0
	c.activeMu = sync.Mutex{}

	go c.ChannelDialer()

}

func (c *WsTransport) closeControlChannel(reason string) {
	if c.controlChannel != nil {
		_ = c.controlChannel.WriteMessage(websocket.TextMessage, []byte("closed"))
		c.controlChannel.Close()
		c.logger.Infof("control channel closed due to %s", reason)
	}
}

func (c *WsTransport) ChannelDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (Websocket)"
	c.logger.Info("attempting to establish a new websocket control channel connection")

connectLoop:
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

			c.config.TunnelStatus = "Connected (Websocket)"

			go c.channelListener()
			go c.poolChecker()

			break connectLoop
		}
	}

	<-c.ctx.Done()

	c.closeControlChannel("context cancellation")
}

func (c *WsTransport) channelListener() {
	msgChan := make(chan string, 100)
	errChan := make(chan error, 100)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			_, msg, err := c.controlChannel.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			msgChan <- string(msg)
		}
	}()
	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-msgChan:
			switch msg {
			case c.chanSignal:
				c.logger.Debug("channel signal received, initiating tunnel dialer")
				go c.tunnelDialer()
			case c.heartbeatSig:
				c.logger.Debug("heartbeat signal received successfully")
			default:
				c.logger.Errorf("unexpected response from channel: %s. Restarting client...", msg)
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
	c.activeMu.Lock()
	c.activeConnections++
	c.activeMu.Unlock()

	c.logger.Debugf("initiating new websocket tunnel connection to address %s", c.config.RemoteAddr)

	tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "")
	if err != nil {
		c.logger.Errorf("failed to dial webSocket tunnel server: %v", err)
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
		return
	}
	c.handleWSSession(tunnelWSConn)
}

func (c *WsTransport) handleWSSession(tunnelCon *websocket.Conn) {
	defer func() {
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
	}()

loop: // loop for reading ping or addr
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			_, remoteAddrBytes, err := tunnelCon.ReadMessage()
			if err != nil {
				c.logger.Debugf("unable to get port from websocket connection %s: %v", tunnelCon.RemoteAddr().String(), err)
				tunnelCon.Close()
				return
			}

			remoteAddr := string(remoteAddrBytes)
			if remoteAddr == "PING" {
				c.logger.Trace("ping recieved from the server")
				continue loop
			}
			go c.localDialer(tunnelCon, remoteAddr)
			return
		}
	}
}

func (c *WsTransport) localDialer(tunnelCon *websocket.Conn, remoteAddr string) {
	select {
	case <-c.ctx.Done():
		return
	default:
		// Extract the port
		parts := strings.Split(remoteAddr, ":")
		var port int
		var err error
		if len(parts) < 2 {
			port, err = strconv.Atoi(parts[0])
			if err != nil {
				c.logger.Info("failed to find the remote port, ", err)
				tunnelCon.Close()
				return
			}
			remoteAddr = fmt.Sprintf("127.0.0.1:%d", port)
		} else {
			port, err = strconv.Atoi(parts[1])
			if err != nil {
				c.logger.Info("failed to find the remote port, ", err)
				tunnelCon.Close()
				return
			}
		}
		localConn, err := c.tcpDialer(remoteAddr)
		if err != nil {
			c.logger.Errorf("connecting to local address %s is not possible", remoteAddr)
			tunnelCon.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", remoteAddr)

		utils.WSConnectionHandler(tunnelCon, localConn, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
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
			HandshakeTimeout: c.config.DialTimeOut, // Set handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := c.tcpDialer(addr)
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
				conn, err := c.tcpDialer(addr)
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

func (c *WsTransport) tcpDialer(address string) (*net.TCPConn, error) {
	// Resolve the address to a TCP address
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}

	// options
	dialer := &net.Dialer{
		Control:   c.reusePortControl,
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

func (c *WsTransport) poolChecker() {
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

func (c *WsTransport) reusePortControl(network, address string, s syscall.RawConn) error {
	var controlErr error

	// Set socket options
	err := s.Control(func(fd uintptr) {
		// Set SO_REUSEADDR
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			controlErr = fmt.Errorf("failed to set SO_REUSEADDR: %v", err)
			return
		}

		// Conditionally set SO_REUSEPORT only on Linux
		if runtime.GOOS == "linux" {
			if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf /* SO_REUSEPORT */, 1); err != nil {
				controlErr = fmt.Errorf("failed to set SO_REUSEPORT: %v", err)
				return
			}
		}
	})

	if err != nil {
		return err
	}

	return controlErr
}
