package transport

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config            *TcpConfig
	parentctx         context.Context
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *logrus.Logger
	controlChannel    net.Conn
	restartMutex      sync.Mutex
	heartbeatSig      string
	chanSignal        string
	usageMonitor      *web.Usage
	activeConnections int
	activeMu          sync.Mutex
}
type TcpConfig struct {
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
	TunnelStatus   string
}

func NewTCPClient(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpTransport{
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
	c.activeConnections = 0
	c.activeMu = sync.Mutex{}

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
			return
		default:
			tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr)
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
				go c.poolChecker()
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
		c.logger.Infof("control channel closed due to %s", reason)
	}
}

func (c *TcpTransport) channelListener() {
	msgChan := make(chan string, 100)
	errChan := make(chan error, 100)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			msg, err := utils.ReceiveBinaryString(c.controlChannel)
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
			c.logger.Debug("context done, stopping channel listener")
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

// Dialing to the tunnel server, chained functions, without retry
func (c *TcpTransport) tunnelDialer() {
	c.activeMu.Lock()
	c.activeConnections++
	c.activeMu.Unlock()

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
		tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr)
		if err != nil {
			c.logger.Error("failed to dial tunnel server: ", err)
			c.activeMu.Lock()
			c.activeConnections--
			c.activeMu.Unlock()
			return
		}
		go c.handleTCPSession(tunnelTCPConn)
	}
}

func (c *TcpTransport) handleTCPSession(tcpsession net.Conn) {
	defer func() {
		c.activeMu.Lock()
		c.activeConnections--
		c.activeMu.Unlock()
	}()

	select {
	case <-c.ctx.Done():
		return
	default:
		remoteAddr, err := utils.ReceiveBinaryString(tcpsession)
		if err != nil {
			c.logger.Debugf("failed to receive port from tunnel connection %s: %v", tcpsession.RemoteAddr().String(), err)
			tcpsession.Close()
			return
		}
		go c.localDialer(tcpsession, remoteAddr)

	}
}

func (c *TcpTransport) localDialer(tunnelConnection net.Conn, remoteAddr string) {
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
				tunnelConnection.Close()
				return
			}
			remoteAddr = fmt.Sprintf("127.0.0.1:%d", port)
		} else {
			port, err = strconv.Atoi(parts[1])
			if err != nil {
				c.logger.Info("failed to find the remote port, ", err)
				tunnelConnection.Close()
				return
			}
		}

		localConnection, err := c.tcpDialer(remoteAddr)
		if err != nil {
			c.logger.Errorf("failed to connect to local address %s: %v", remoteAddr, err)
			tunnelConnection.Close()
			return
		}

		c.logger.Debugf("connected to local address %s successfully", remoteAddr)
		go utils.TCPConnectionHandler(localConnection, tunnelConnection, c.logger, c.usageMonitor, port, c.config.Sniffer)
	}
}

func (c *TcpTransport) tcpDialer(address string) (*net.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.config.DialTimeOut)
	defer cancel()

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
	conn, err := dialer.DialContext(ctx, "tcp", tcpAddr.String())
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

func (c *TcpTransport) poolChecker() {
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

func (c *TcpTransport) reusePortControl(network, address string, s syscall.RawConn) error {
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
