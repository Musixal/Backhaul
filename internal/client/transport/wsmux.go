package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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
	dialLimitChan     chan struct{}
	usageMonitor      *web.Usage
	restartMutex      sync.Mutex
	activeConnections int32
	lastRequest       time.Time
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
	DialLimit        int
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
			KeepAliveInterval: 20 * time.Second,
			KeepAliveTimeout:  40 * time.Second,
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
		activeConnections: 0,
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		lastRequest:       time.Now(),
		dialLimitChan:     make(chan struct{}, config.DialLimit),
	}

	return client
}

func (c *WsMuxTransport) Start() {
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = fmt.Sprintf("Disconnected (%s)", c.config.Mode)

	go c.channelDialer()
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
	c.lastRequest = time.Now()
	c.dialLimitChan = make(chan struct{}, c.config.DialLimit)

	go c.Start()

}

func (c *WsMuxTransport) channelDialer() {
	c.logger.Infof("attempting to establish a new %s control channel connection", c.config.Mode)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:

			tunnelWSConn, err := WebSocketDialer(c.config.RemoteAddr, "/channel", c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay, c.config.Token, c.config.Mode)
			if err != nil {
				c.logger.Errorf("failed to dial %s control channel: %v", c.config.Mode, err)
				time.Sleep(c.config.RetryInterval)
				continue
			}
			c.controlChannel = tunnelWSConn
			c.logger.Info("control channel established successfully")

			c.config.TunnelStatus = fmt.Sprintf("Connected (%s)", c.config.Mode)

			go c.channelHandler()
			go c.poolMaintainer()

			return
		}
	}
}

func (c *WsMuxTransport) poolMaintainer() {
	ticker := time.NewTicker(time.Millisecond * 500)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-ticker.C:
			if time.Since(c.lastRequest).Milliseconds() < 500 {
				continue
			}
			activeConnections := int(c.activeConnections)
			c.logger.Tracef("active connections: %d", c.activeConnections)
			if activeConnections < c.config.ConnectionPool {
				neededConn := c.config.ConnectionPool - activeConnections
				for i := 0; i < neededConn; i++ {
					go c.tunnelDialer()
					c.dialLimitChan <- struct{}{}
				}

			}

		}

	}

}

func (c *WsMuxTransport) channelHandler() {
	msgChan := make(chan byte, 1000)
	errChan := make(chan error, 1000)

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

	for {
		select {
		case <-c.ctx.Done():
			_ = c.controlChannel.WriteMessage(websocket.BinaryMessage, []byte{utils.SG_Closed})
			return

		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				c.logger.Debug("channel signal received, initiating tunnel dialer")
				c.lastRequest = time.Now()
				go c.tunnelDialer()
				c.dialLimitChan <- struct{}{}
			case utils.SG_HB:
				c.logger.Debug("heartbeat received successfully")
			case utils.SG_Closed:
				c.logger.Info("control channel has been closed by the server")
				go c.Restart()
				return
			default:
				c.logger.Errorf("unexpected response from control channel: %v. Restarting client...", msg)
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
	// Increment active connections counter
	atomic.AddInt32(&c.activeConnections, 1)

	c.logger.Debugf("initiating new %s tunnel connection to address %s", c.config.Mode, c.config.RemoteAddr)

	// Dial to the tunnel server
	tunnelWSConn, err := WebSocketDialer(c.config.RemoteAddr, "/tunnel", c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay, c.config.Token, c.config.Mode)
	if err != nil {
		<-c.dialLimitChan

		c.logger.Errorf("failed to dial %s tunnel server: %v", c.config.Mode, err)

		// Decrement active connections on failure
		atomic.AddInt32(&c.activeConnections, -1)
		return
	}

	<-c.dialLimitChan

	c.handleSession(tunnelWSConn)
}

func (c *WsMuxTransport) handleSession(tunnelConn *websocket.Conn) {
	defer func() {
		atomic.AddInt32(&c.activeConnections, -1)
	}()

	// SMUX server
	session, err := smux.Server(tunnelConn.NetConn(), c.smuxConfig)
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
				c.logger.Debug("session is closed: ", err)
				session.Close()
				return
			}

			remoteAddr, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				c.logger.Errorf("unable to get port from stream connection %s: %v", tunnelConn.RemoteAddr().String(), err)
				if err := session.Close(); err != nil {
					c.logger.Errorf("failed to close mux stream: %v", err)
				}
				return
			}
			go c.localDialer(stream, remoteAddr)
		}
	}
}

func (c *WsMuxTransport) localDialer(stream *smux.Stream, remoteAddr string) {
	// Extract the port from the received address
	port, resolvedAddr, err := ResolveRemoteAddr(remoteAddr)
	if err != nil {
		c.logger.Infof("failed to resolve remote port: %v", err)
		stream.Close()
		return
	}

	localConnection, err := TcpDialer(resolvedAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay)
	if err != nil {
		c.logger.Errorf("connecting to local address %s is not possible", remoteAddr)
		stream.Close()
		return
	}

	c.logger.Debugf("connected to local address %s successfully", remoteAddr)

	utils.TCPConnectionHandler(stream, localConnection, c.logger, c.usageMonitor, int(port), c.config.Sniffer)
}
