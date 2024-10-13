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
	config          *WsMuxConfig
	smuxConfig      *smux.Config
	parentctx       context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *logrus.Logger
	controlChannel  *websocket.Conn
	usageMonitor    *web.Usage
	restartMutex    sync.Mutex
	poolConnections int32
	loadConnections int32
	controlFlow     chan struct{}
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
	ConnPoolSize     int
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
		config:          config,
		parentctx:       parentCtx,
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger,
		controlChannel:  nil, // will be set when a control connection is established
		usageMonitor:    web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		poolConnections: 0,
		loadConnections: 0,
		controlFlow:     make(chan struct{}),
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

	// close control channel connection
	if c.controlChannel != nil {
		c.controlChannel.Close()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.poolConnections = 0
	c.loadConnections = 0
	c.controlFlow = make(chan struct{})

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

			go c.poolMaintainer()
			go c.channelHandler()

			return
		}
	}
}

func (c *WsMuxTransport) poolMaintainer() {
	for i := 0; i < c.config.ConnPoolSize; i++ { //initial pool filling
		go c.tunnelDialer()
	}

	tickerPool := time.NewTicker(time.Second * 1)
	defer tickerPool.Stop()

	tickerLoad := time.NewTicker(time.Second * 30)
	defer tickerLoad.Stop()

	newPoolSize := c.config.ConnPoolSize // intial value
	var poolConnectionsSum int32 = 0

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-tickerPool.C:
			// Accumulate pool connections over time (every second)
			atomic.AddInt32(&poolConnectionsSum, atomic.LoadInt32(&c.poolConnections))

		case <-tickerLoad.C:
			// Calculate the loadConnections over the last 30 seconds
			loadConnections := (int(atomic.LoadInt32(&c.loadConnections)) + 29) / 30 // Every 30 seconds, +29 for ceil-like logic
			atomic.StoreInt32(&c.loadConnections, 0)                                 // Reset

			// Calculate the average pool connections over the last 30 seconds
			poolConnectionsAvg := (int(atomic.LoadInt32(&poolConnectionsSum)) + 29) / 30 // Average connections in 1 second, +29 for ceil-like logic
			atomic.StoreInt32(&poolConnectionsSum, 0)                                    // Reset

			// Dynamically adjust the pool size based on current connections
			if (loadConnections+4)/5 > poolConnectionsAvg { // caclulate in 200ms
				c.logger.Infof("increasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize+1, poolConnectionsAvg, loadConnections)
				newPoolSize++

				// Add a new connection to the pool
				go c.tunnelDialer()
			} else if (loadConnections+3)/4 < poolConnectionsAvg && newPoolSize > c.config.ConnPoolSize { // tolerance for decreasing pool is 20%
				c.logger.Infof("decreasing pool size: %d -> %d", newPoolSize, newPoolSize-1)
				newPoolSize--

				// send a signal to controlFlow
				c.controlFlow <- struct{}{}
			}
		}
	}

}

func (c *WsMuxTransport) channelHandler() {
	msgChan := make(chan byte, 1000)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return

			default:
				_, msg, err := c.controlChannel.ReadMessage()
				if err != nil {
					c.logger.Error("failed to read from channel connection. ", err)
					go c.Restart()
					return
				}
				msgChan <- msg[0]
			}
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
				atomic.AddInt32(&c.loadConnections, 1)
				select {
				case <-c.controlFlow: // Do nothing

				default:
					c.logger.Debug("channel signal received, initiating tunnel dialer")
					go c.tunnelDialer()
				}

			case utils.SG_HB:
				c.logger.Debug("heartbeat received successfully")

			case utils.SG_Closed:
				c.logger.Info("control channel has been closed by the server")
				go c.Restart()
				return

			default:
				c.logger.Errorf("unexpected response from control channel: %v", msg)
				go c.Restart()
				return
			}

		}
	}
}

func (c *WsMuxTransport) tunnelDialer() {
	c.logger.Debugf("initiating new %s tunnel connection to address %s", c.config.Mode, c.config.RemoteAddr)

	// Dial to the tunnel server
	tunnelWSConn, err := WebSocketDialer(c.config.RemoteAddr, "/tunnel", c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay, c.config.Token, c.config.Mode)
	if err != nil {
		c.logger.Errorf("failed to dial %s tunnel server: %v", c.config.Mode, err)

		return
	}

	// Increment active connections counter
	atomic.AddInt32(&c.poolConnections, 1)

	c.handleSession(tunnelWSConn)
}

func (c *WsMuxTransport) handleSession(tunnelConn *websocket.Conn) {
	defer func() {
		atomic.AddInt32(&c.poolConnections, -1)
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
