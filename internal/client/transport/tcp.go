package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config         *TcpConfig
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	controlChannel net.Conn
	timeout        time.Duration
	restartMutex   sync.Mutex
	heartbeatSig   string
	chanSignal     string
	usageMonitor   *web.Usage
}
type TcpConfig struct {
	RemoteAddr    string
	Nodelay       bool
	KeepAlive     time.Duration
	RetryInterval time.Duration
	Token         string
	Forwarder     map[int]string
	Sniffer       bool
	WebPort       int
	SnifferLog    string
	TunnelStatus  string
}

func NewTCPClient(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpTransport{
		config:         config,
		ctx:            ctx,
		parentctx:      parentCtx,
		cancel:         cancel,
		logger:         logger,
		controlChannel: nil,              // will be set when a control connection is established
		timeout:        30 * time.Second, // Default timeout for tcpDialer
		heartbeatSig:   "0",              // Default heartbeat signal
		chanSignal:     "1",              // Default channel signal
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return client
}

func (c *TcpTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")
	if c.cancel != nil {
		c.cancel()
	}

	go c.closeControlChannel("restarting client")

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""

	go c.ChannelDialer()

}

func (c *TcpTransport) ChannelDialer() {
	// for  webui
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (TCP)"
	c.logger.Info("attempting to establish a new control channel connection...")

connectLoop:
	for {
		select {
		case <-c.ctx.Done():
			go c.closeControlChannel("context cancellation")
			return
		default:
			tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr, c.config.Nodelay)
			if err != nil {
				c.logger.Errorf("channel dialer: error dialing remote address %s: %v", c.config.RemoteAddr, err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			err = utils.SendBinaryString(tunnelTCPConn, c.config.Token)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				tunnelTCPConn.Close()
				continue connectLoop
			}

			// Set a read deadline for the token response
			if err := tunnelTCPConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				c.logger.Errorf("failed to set read deadline: %v", err)
				tunnelTCPConn.Close()
				continue connectLoop
			}
			// Receive response
			message, err := utils.ReceiveBinaryString(tunnelTCPConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Errorf("failed to receive control channel response: %v", err)
				}
				tunnelTCPConn.Close() // Close connection on error or timeout
				time.Sleep(c.config.RetryInterval)
				continue connectLoop
			}
			// Resetting the deadline (removes any existing deadline)
			tunnelTCPConn.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.controlChannel = tunnelTCPConn
				c.logger.Info("control channel established successfully")

				c.config.TunnelStatus = "Connected (TCP)"
				go c.channelListener()
				break connectLoop // break the loop
			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				tunnelTCPConn.Close() // Close connection if the token is invalid
				time.Sleep(c.config.RetryInterval)
				continue connectLoop
			}
		}
	}

	<-c.ctx.Done()
	go c.closeControlChannel("context cancellation")
}

func (c *TcpTransport) closeControlChannel(reason string) {
	if c.controlChannel != nil {
		_ = utils.SendBinaryString(c.controlChannel, "closed")
		c.controlChannel.Close()
		c.logger.Debugf("control channel closed due to %s", reason)
	}
}

// listen to the channel signals
func (c *TcpTransport) channelListener() {
	for c.controlChannel != nil {
		select {
		case <-c.ctx.Done():
			return
		default:
			msg, err := utils.ReceiveBinaryString(c.controlChannel)
			if err != nil {
				c.logger.Error("error receiving channel signal, restarting client")
				go c.Restart()
				return
			}
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
		}
	}
	c.logger.Error("control channel connection closed unexpectedly, restarting client")
	go c.Restart()

}

// Dialing to the tunnel server, chained functions, without retry
func (c *TcpTransport) tunnelDialer() {
	select {
	case <-c.ctx.Done():
		return
	default:
		if c.controlChannel == nil {
			c.logger.Warn("No control channel found, cannot initiate tunnel dialer")
			return
		}
		c.logger.Debugf("initiating new connection to tunnel server at %s", c.config.RemoteAddr)

		// Dial to the tunnel server
		tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr, c.config.Nodelay)
		if err != nil {
			c.logger.Error("failed to dial tunnel server: ", err)
			return
		}
		go c.handleTCPSession(tunnelTCPConn)
	}
}

// func (c *TcpTransport) handleTCPSession(tcpsession net.Conn) {
// 	select {
// 	case <-c.ctx.Done():
// 		return
// 	default:
// 		port, err := utils.ReceiveBinaryInt(tcpsession)
// 		if err != nil {
// 			c.logger.Errorf("failed to receive port from tunnel connection %s: %v", tcpsession.RemoteAddr().String(), err)
// 			tcpsession.Close()
// 			return
// 		}
// 		go c.localDialer(tcpsession, port)

// 	}
// }

func (c *TcpTransport) handleTCPSession(tcpsession net.Conn) {
	// Channel to receive the port or error
	resultChan := make(chan struct {
		port uint16
		err  error
	})

	// Start goroutine to receive the binary int
	go func() {
		port, err := utils.ReceiveBinaryInt(tcpsession)
		resultChan <- struct {
			port uint16
			err  error
		}{port, err}
		close(resultChan)
	}()

	select {
	case <-c.ctx.Done():
		// Context canceled, close the session
		tcpsession.Close()
		c.logger.Trace("session closed due to context cancellation")
		return
	case result := <-resultChan:
		// Check if there was an error during the receive operation
		if result.err != nil {
			c.logger.Errorf("failed to receive port from tunnel connection %s: %v", tcpsession.RemoteAddr().String(), result.err)
			tcpsession.Close()
			return
		}

		// Port received successfully, handle the connection
		go c.localDialer(tcpsession, result.port)
	}
}

func (c *TcpTransport) localDialer(tunnelConnection net.Conn, port uint16) {
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

func (c *TcpTransport) tcpDialer(address string, tcpnodelay bool) (*net.TCPConn, error) {
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
