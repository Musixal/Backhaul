package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

type TcpMuxTransport struct {
	config            *TcpMuxConfig
	smuxConfig        *smux.Config
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	controlChannel    net.Conn
	usageMonitor      *web.Usage
	activeMu          sync.Mutex
	restartMutex      sync.Mutex
	activeConnections int
}

type TcpMuxConfig struct {
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
}

func NewMuxClient(parentCtx context.Context, config *TcpMuxConfig, logger *logrus.Logger) *TcpMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpMuxTransport{
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
		activeMu:          sync.Mutex{},
		usageMonitor:      web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
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

	// Close tunnel channel connection
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

	go c.ChannelDialer()

}

func (c *TcpMuxTransport) ChannelDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (TCPMux)"
	c.logger.Info("attempting to establish a new tcpmux control channel connection...")

loop:
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			tunnelConn, err := c.tcpDialer(c.config.RemoteAddr)
			if err != nil {
				c.logger.Errorf("tcpmux channel dialer: error dialing remote address %s: %v", c.config.RemoteAddr, err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			err = utils.SendBinaryString(tunnelConn, c.config.Token)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				tunnelConn.Close()
				continue loop
			}

			// Set a read deadline for the token response
			if err := tunnelConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				c.logger.Errorf("failed to set read deadline: %v", err)
				tunnelConn.Close()
				continue loop
			}
			// Receive response
			message, err := utils.ReceiveBinaryString(tunnelConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Errorf("failed to receive control channel response: %v", err)
				}
				tunnelConn.Close() // Close connection on error or timeout
				time.Sleep(c.config.RetryInterval)
				continue loop
			}
			// Resetting the deadline (removes any existing deadline)
			tunnelConn.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.controlChannel = tunnelConn
				c.logger.Info("tcpmux control channel established successfully")

				c.config.TunnelStatus = "Connected (TCPMux)"
				go c.channelListener()
				go c.poolChecker()
				break loop // break the loop
			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				tunnelConn.Close() // Close connection if the token is invalid
				time.Sleep(c.config.RetryInterval)
				continue loop
			}
		}
	}

}

func (c *TcpMuxTransport) closeControlChannel(reason string) {
	if c.controlChannel != nil {
		_ = utils.SendBinaryByte(c.controlChannel, utils.SG_Closed)
		c.controlChannel.Close()
		c.logger.Debugf("control channel closed due to %s", reason)
	}
}

func (c *TcpMuxTransport) channelListener() {
	msgChan := make(chan byte, 100)
	errChan := make(chan error, 100)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			msg, err := utils.ReceiveBinaryByte(c.controlChannel)
			if err != nil {
				errChan <- err
				return
			}
			msgChan <- msg
		}
	}()

	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			c.closeControlChannel("context cancellation")
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

func (c *TcpMuxTransport) tunnelDialer() {
	c.activeMu.Lock()
	c.activeConnections++
	c.activeMu.Unlock()

	if c.controlChannel == nil {
		c.logger.Warn("control channel is nil, cannot dial tunnel. Restarting client...")
		go c.Restart()
		return
	}
	c.logger.Debugf("initiating new tunnel connection to address %s", c.config.RemoteAddr)

	tunnelConn, err := c.tcpDialer(c.config.RemoteAddr)
	if err != nil {
		c.logger.Errorf("failed to dial tunnel server: %v", err)
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
		return
	}
	c.handleTunnelConn(tunnelConn)
}

func (c *TcpMuxTransport) handleTunnelConn(tunnelConn net.Conn) {
	defer func() {
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
	}()

	// SMUX server
	session, err := smux.Server(tunnelConn, c.smuxConfig)
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
				session.Close()
				return
			}

			remoteAddr, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				c.logger.Errorf("unable to get port from stream connection %s: %v", tunnelConn.RemoteAddr().String(), err)
				if err := session.Close(); err != nil {
					c.logger.Errorf("failed to close mux stream: %v", err)
				}
				tunnelConn.Close()
				return
			}
			go c.localDialer(stream, remoteAddr)
		}
	}
}

func (c *TcpMuxTransport) localDialer(stream *smux.Stream, remoteAddr string) {
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

func (c *TcpMuxTransport) tcpDialer(address string) (*net.TCPConn, error) {
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

func (c *TcpMuxTransport) poolChecker() {
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
