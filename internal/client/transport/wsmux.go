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
	"github.com/xtaci/smux"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsMuxTransport struct {
	config            *WsMuxConfig
	smuxConfig        *smux.Config
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	controlChannel    *websocket.Conn
	usageMonitor      *web.Usage
	activeMu          sync.Mutex
	restartMutex      sync.Mutex
	heartbeatSig      string
	chanSignal        string
	activeConnections int
}
type WsMuxConfig struct {
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
	Mode             config.TransportType
}

func NewWSMuxClient(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           config.MuxVersion,
			KeepAliveInterval: 10 * time.Second,
			KeepAliveTimeout:  30 * time.Second,
			MaxFrameSize:      config.MaxFrameSize,
			MaxReceiveBuffer:  config.MaxReceiveBuffer,
			MaxStreamBuffer:   config.MaxStreamBuffer,
		},
		config:            config,
		parentctx:         parentCtx,
		ctx:               ctx,
		cancel:            cancel,
		logger:            logger,
		controlChannel:    nil, // will be set when a control connection is established
		heartbeatSig:      "0", // Default heartbeat signal
		chanSignal:        "1", // Default channel signal
		activeConnections: 0,
		activeMu:          sync.Mutex{},
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
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
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.activeConnections = 0
	c.activeMu = sync.Mutex{}

	go c.ChannelDialer()

}

func (c *WsMuxTransport) closeControlChannel(reason string) {
	if c.controlChannel != nil {
		_ = c.controlChannel.WriteMessage(websocket.TextMessage, []byte("closed"))
		c.controlChannel.Close()
		c.logger.Infof("control channel closed due to %s", reason)
	}
}

func (c *WsMuxTransport) ChannelDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (WsMux)"
	c.logger.Info("attempting to establish a new wsmux control channel connection")

connectLoop:
	for {
		select {
		case <-c.ctx.Done():
			return
		default:

			tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "/channel")
			if err != nil {
				c.logger.Errorf("failed to dial wsmux control channel: %v", err)
				time.Sleep(c.config.RetryInterval)
				continue
			}
			c.controlChannel = tunnelWSConn
			c.logger.Info("wsmux control channel established successfully")

			c.config.TunnelStatus = "Connected (WsMux)"

			go c.channelListener()
			go c.poolChecker()

			break connectLoop
		}
	}

	<-c.ctx.Done()
	c.closeControlChannel("context cancellation")
}

func (c *WsMuxTransport) channelListener() {
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
				c.logger.Debug("heartbeat received successfully")
			default:
				c.logger.Errorf("unexpected response from control channel: %s. Restarting client...", msg)
				go c.Restart()
				return

			}
		case err := <-errChan:
			c.logger.Errorf("error receiving channel signal: %v. Restarting client...", err)
			go c.Restart()
			return

		}
	}
}

func (c *WsMuxTransport) tunnelDialer() {
	c.activeMu.Lock()
	c.activeConnections++
	c.activeMu.Unlock()

	if c.controlChannel == nil {
		c.logger.Warn("wsmux control channel is nil, cannot dial tunnel. Restarting client...")
		go c.Restart()
		return
	}
	c.logger.Debugf("initiating new wsmux tunnel connection to address %s", c.config.RemoteAddr)

	tunnelWSConn, err := c.wsDialer(c.config.RemoteAddr, "/tunnel")
	if err != nil {
		c.logger.Errorf("failed to dial wsmux tunnel server: %v", err)
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
		return
	}
	c.handleTunnelConn(tunnelWSConn)
}

func (c *WsMuxTransport) handleTunnelConn(tunnelConn *websocket.Conn) {
	defer func() {
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
	}()

	// SMUX server
	session, err := smux.Server(tunnelConn.UnderlyingConn(), c.smuxConfig)
	if err != nil {
		c.logger.Errorf("failed to create mux session: %v", err)
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			stream, err := session.AcceptStream()
			if err != nil {
				c.logger.Trace("session is closed: ", err)
				return
			}

			remoteAddr, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				c.logger.Errorf("unable to get port from websocket connection %s: %v", tunnelConn.RemoteAddr().String(), err)
				if err := session.Close(); err != nil {
					c.logger.Errorf("failed to close mux stream due to recievebinarystring: %v", err)
				}
				tunnelConn.Close()
				return
			}
			go c.localDialer(stream, remoteAddr)
		}
	}
}

func (c *WsMuxTransport) localDialer(stream *smux.Stream, remoteAddr string) {
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
	utils.TCPConnectionHandler(stream, localConnection, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
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

func (c *WsMuxTransport) tcpDialer(address string) (*net.TCPConn, error) {
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

func (c *WsMuxTransport) poolChecker() {
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
					time.Sleep(time.Millisecond * 10)
				}

			}

		}

	}

}

func (c *WsMuxTransport) reusePortControl(network, address string, s syscall.RawConn) error {
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
