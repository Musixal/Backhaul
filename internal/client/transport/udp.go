package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"
	"github.com/sirupsen/logrus"
)

type UdpTransport struct {
	config          *UdpConfig
	parentctx       context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *logrus.Logger
	controlChannel  net.Conn
	usageMonitor    *web.Usage
	restartMutex    sync.Mutex
	poolConnections int32
	loadConnections int32
	controlFlow     chan struct{}
}
type UdpConfig struct {
	RemoteAddr     string
	Token          string
	SnifferLog     string
	TunnelStatus   string
	RetryInterval  time.Duration
	DialTimeOut    time.Duration
	ConnPoolSize   int
	WebPort        int
	Sniffer        bool
	AggressivePool bool
}

func NewUDPClient(parentCtx context.Context, config *UdpConfig, logger *logrus.Logger) *UdpTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	client := &UdpTransport{
		config:          config,
		parentctx:       parentCtx,
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger,
		controlChannel:  nil, // will be set when a control connection is established
		usageMonitor:    web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		poolConnections: 0,
		loadConnections: 0,
		controlFlow:     make(chan struct{}, 100),
	}

	return client
}

func (c *UdpTransport) Start() {
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (UDP)"

	go c.channelDialer()
}

func (c *UdpTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")

	// for removing timeout logs
	level := c.logger.Level
	c.logger.SetLevel(logrus.FatalLevel)

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
	c.controlFlow = make(chan struct{}, 100)

	// set the log level again
	c.logger.SetLevel(level)

	go c.Start()

}

func (c *UdpTransport) channelDialer() {
	c.logger.Info("attempting to establish a new control channel connection...")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			tunnelTCPConn, err := TcpDialer(c.ctx, c.config.RemoteAddr, c.config.DialTimeOut, 30, true, 3, 0, 0)
			if err != nil {
				c.logger.Errorf("channel dialer: %v", err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			// Sending security token
			err = utils.SendBinaryTransportString(tunnelTCPConn, c.config.Token, utils.SG_Chan)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				tunnelTCPConn.Close()
				continue
			}

			// Set a read deadline for the token response
			if err := tunnelTCPConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				c.logger.Errorf("failed to set read deadline: %v", err)
				tunnelTCPConn.Close()
				continue
			}

			// Receive response
			message, _, err := utils.ReceiveBinaryTransportString(tunnelTCPConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					c.logger.Warn("timeout while waiting for control channel response")
				} else {
					c.logger.Errorf("failed to receive control channel response: %v", err)
				}
				tunnelTCPConn.Close() // Close connection on error or timeout
				time.Sleep(c.config.RetryInterval)
				continue
			}
			// Resetting the deadline (removes any existing deadline)
			tunnelTCPConn.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.controlChannel = tunnelTCPConn
				c.logger.Info("control channel established successfully")

				c.config.TunnelStatus = "Connected (UDP)"

				go c.poolMaintainer()
				go c.channelHandler()

				return

			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				tunnelTCPConn.Close() // Close connection if the token is invalid
				time.Sleep(c.config.RetryInterval)
				continue
			}
		}
	}
}

func (c *UdpTransport) poolMaintainer() {
	for i := 0; i < c.config.ConnPoolSize; i++ { //initial pool filling
		go c.tunnelDialer()
	}

	// factors
	a := 4
	b := 5
	x := 3
	y := 4.0

	if c.config.AggressivePool {
		c.logger.Info("aggressive pool management enabled")
		a = 1
		b = 2
		x = 0
		y = 0.75
	}

	tickerPool := time.NewTicker(time.Second * 1)
	defer tickerPool.Stop()

	tickerLoad := time.NewTicker(time.Second * 10)
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
			// Calculate the loadConnections over the last 10 seconds
			loadConnections := (int(atomic.LoadInt32(&c.loadConnections)) + 9) / 10 // +9 for ceil-like logic
			atomic.StoreInt32(&c.loadConnections, 0)                                // Reset

			// Calculate the average pool connections over the last 10 seconds
			poolConnectionsAvg := (int(atomic.LoadInt32(&poolConnectionsSum)) + 9) / 10 // +9 for ceil-like logic
			atomic.StoreInt32(&poolConnectionsSum, 0)                                   // Reset

			// Dynamically adjust the pool size based on current connections
			if (loadConnections + a) > poolConnectionsAvg*b {
				c.logger.Debugf("increasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize+1, poolConnectionsAvg, loadConnections)
				newPoolSize++

				// Add a new connection to the pool
				go c.tunnelDialer()
			} else if float64(loadConnections+x) < float64(poolConnectionsAvg)*y && newPoolSize > c.config.ConnPoolSize {
				c.logger.Debugf("decreasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize-1, poolConnectionsAvg, loadConnections)
				newPoolSize--

				// send a signal to controlFlow
				c.controlFlow <- struct{}{}
			}
		}
	}

}

func (c *UdpTransport) channelHandler() {
	msgChan := make(chan byte, 1000)

	// Goroutine to handle the blocking ReceiveBinaryString
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				msg, err := utils.ReceiveBinaryByte(c.controlChannel)
				if err != nil {
					if c.cancel != nil {
						c.logger.Error("failed to read from control channel. ", err)
						go c.Restart()
					}
					return
				}
				msgChan <- msg
			}
		}
	}()

	// Main loop to listen for context cancellation or received messages
	for {
		select {
		case <-c.ctx.Done():
			_ = utils.SendBinaryByte(c.controlChannel, utils.SG_Closed)
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
				c.logger.Debug("heartbeat signal received successfully")

			case utils.SG_Closed:
				c.logger.Warn("control channel has been closed by the server")
				go c.Restart()
				return

			case utils.SG_RTT:
				err := utils.SendBinaryByte(c.controlChannel, utils.SG_RTT)
				if err != nil {
					c.logger.Error("failed to send RTT signal, restarting client: ", err)
					go c.Restart()
					return
				}

			default:
				c.logger.Errorf("unexpected response from channel: %v.", msg)
				go c.Restart()
				return
			}
		}
	}
}

func (c *UdpTransport) tunnelDialer() {
	c.logger.Debugf("initiating new connection to tunnel server at %s", c.config.RemoteAddr)

	remoteAddr, err := net.ResolveUDPAddr("udp", c.config.RemoteAddr)
	if err != nil {
		c.logger.Error("failed to resolve tunnel address:", err)
		return
	}

	tunConn, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		c.logger.Error("failed to connect to server:", err)
		return
	}

	defer tunConn.Close()

	done := make(chan struct{})

	// Start handleTunnelConn in a goroutine
	go func() {
		c.handleTunnelConn(tunConn)
		close(done) // Signal that handleTunnelConn is done
	}()

	// Wait for either handleTunnelConn to finish or the context to be done
	select {
	case <-done:
	case <-c.ctx.Done():
	}
}

func (c *UdpTransport) handleTunnelConn(tunConn *net.UDPConn) {
	// Send token message to the server
	_, err := tunConn.Write([]byte(c.config.Token))
	if err != nil {
		c.logger.Error("faliled to send token:", err)
		return
	}

	// Increment active connections counter
	atomic.AddInt32(&c.poolConnections, 1)

	// Prepare a buffer to receive the server's response
	buffer := make([]byte, 47) // maximum buffer requried for store in IPv6:Port format

	for {
		n, _, err := tunConn.ReadFromUDP(buffer)
		if err != nil {
			c.logger.Error("failed to receive response from server:", err)

			atomic.AddInt32(&c.poolConnections, -1)

			return
		}

		// Compare the received bytes with the expected SG_Ping message
		if n == 1 && buffer[0] == utils.SG_Ping {
			c.logger.Tracef("ping signal recieved for %s", tunConn.LocalAddr().String())
			continue
		}

		port, remoteAddr, err := ResolveRemoteAddr(string(buffer[:n]))

		// Decrement active connections after successful or failed connection
		atomic.AddInt32(&c.poolConnections, -1)

		if err != nil {
			c.logger.Error("failed to find remote address:", err)
			return
		}

		c.localDialer(remoteAddr, port, tunConn)

		break
	}

}

func (c *UdpTransport) localDialer(remoteAddr string, port int, tunConn *net.UDPConn) {
	remoteResolvedAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		c.logger.Error("failed to resolve remote address:", err)
		return
	}

	// Dial the remote UDP server
	remoteConn, err := net.DialUDP("udp", nil, remoteResolvedAddr)
	if err != nil {
		c.logger.Errorf("failed to dial remote UDP address: %v", err)
	}

	defer remoteConn.Close()

	done := make(chan struct{})
	c.logger.Debugf("start to copy from tunnel %s to local %s", tunConn.LocalAddr(), remoteAddr)
	go func() {
		c.udpCopy(remoteConn, tunConn, port)
		done <- struct{}{}
	}()

	c.udpCopy(tunConn, remoteConn, port)

	<-done

}

func (c *UdpTransport) udpCopy(srcConn, dstConn *net.UDPConn, port int) {
	buf := make([]byte, 16*1024)
	readTimeout := 60 * time.Second

	for {
		// Set the read deadline to 60 seconds from now
		err := srcConn.SetReadDeadline(time.Now().Add(readTimeout))
		if err != nil {
			c.logger.Errorf("failed to set read deadline: %v", err)
			return
		}

		// Read from the UDP source connection
		n, _, err := srcConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				c.logger.Debug("read from UDP timed out")
				return // Exit on timeout
			}
			c.logger.Errorf("failed to read from UDP: %v", err)
			return
		}

		totalWritten := 0
		// Write the read data to the destination UDP connection
		for totalWritten < n {
			w, err := dstConn.Write(buf[totalWritten:n])
			if err != nil {
				c.logger.Errorf("failed to write to UDP %s: %v", dstConn.RemoteAddr().String(), err)
				return
			}
			totalWritten += w
		}

		// Optionally update the port usage stats if sniffing is enabled
		if c.config.Sniffer {
			c.usageMonitor.AddOrUpdatePort(port, uint64(totalWritten))
		}

		c.logger.Debugf("forwarded %d bytes from %s to %s", n, srcConn.LocalAddr().String(), dstConn.RemoteAddr().String())
	}
}
