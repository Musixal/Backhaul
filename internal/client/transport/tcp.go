package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config         *TcpConfig
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	controlChannel net.Conn
	timeout        time.Duration
	restartMutex   sync.Mutex
	heartbeatSig   string
	chanSignal     string
}
type TcpConfig struct {
	RemoteAddr    string
	Nodelay       bool
	KeepAlive     time.Duration
	RetryInterval time.Duration
	Token         string
	Forwarder     map[int]string
}

func NewTCPClient(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &TcpTransport{
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

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel

	// Re-initialize variables
	c.controlChannel = nil

	go c.ChannelDialer()

}

func (c *TcpTransport) ChannelDialer() {
	for c.controlChannel == nil {
		select {
		case <-c.ctx.Done():
			return
		default:
			c.logger.Info("trying to establish a new control channel connection")
			tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr, c.config.Nodelay)
			if err != nil {
				c.logger.Error(err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			err = utils.SendBinaryString(tunnelTCPConn, c.config.Token)
			if err != nil {
				c.logger.Error("unable to send security token")
				tunnelTCPConn.Close()
				continue
			}

			// Set a read deadline for the token response
			tunnelTCPConn.SetReadDeadline(time.Now().Add(2 * time.Second))

			// Receive response
			message, err := utils.ReceiveBinaryString(tunnelTCPConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Error("unable to establish new control channel")
				}
				tunnelTCPConn.Close() // Close connection on error or timeout
				time.Sleep(c.config.RetryInterval)
				continue
			}

			if message == c.config.Token {
				c.controlChannel = tunnelTCPConn
				c.logger.Info("control channel established successfully")
				// Resetting the deadline (removes any existing deadline)
				tunnelTCPConn.SetReadDeadline(time.Time{})
				go c.channelListener()
				return
			} else {
				c.logger.Error("invalid token received, retrying...")
				tunnelTCPConn.Close() // Close connection if the token is invalid
				time.Sleep(c.config.RetryInterval)
				continue
			}
		}
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
			if msg == c.chanSignal {
				go c.tunnelDialer()
			} else if msg == c.heartbeatSig {
				c.logger.Debug("heartbeat received successfully")
			} else {
				c.logger.Error("weird response from channel, exiting from control channel, restarting client")
				go c.Restart()
				return
			}
		}
	}
	c.logger.Error("an error occured in control channel connection, restarting client")
	go c.Restart()
}

// Dialing to the tunnel server, chained functions, without retry
func (c *TcpTransport) tunnelDialer() {
	select {
	case <-c.ctx.Done():
		return
	default:
		if c.controlChannel == nil {
			//exit
			c.logger.Warn("no control channel found...")
			return
		}
		c.logger.Debug("initiating new connection to address ", c.config.RemoteAddr)

		// Dial to the tunnel server
		tunnelTCPConn, err := c.tcpDialer(c.config.RemoteAddr, c.config.Nodelay)
		if err != nil {
			c.logger.Error("failed to dial tunnel server: ", err)
			return
		}
		go c.handleTCPSession(tunnelTCPConn)
	}
}

func (c *TcpTransport) handleTCPSession(tcpsession net.Conn) {
	select {
	case <-c.ctx.Done():
		return
	default:
		port, err := utils.ReceiveBinaryInt(tcpsession)

		if err != nil {
			c.logger.Tracef("unable to get the port from the %s connection", tcpsession.RemoteAddr().String())
			tcpsession.Close()
			return
		}
		go c.localDialer(tcpsession, port)

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
			c.logger.Errorf("connecting to the local address %s is not possible", localAddress)
			tunnelConnection.Close()
			return
		}
		c.logger.Debugf("connected to local address %s successfully", localAddress)
		go utils.ConnectionHandler(localConnection, tunnelConnection, c.logger)
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
