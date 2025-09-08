package transport

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/utils/network"
	"github.com/musix/backhaul/internal/web"
	"github.com/quic-go/quic-go"

	"github.com/sirupsen/logrus"
)

type QuicTransport struct {
	config          *QuicConfig
	parentctx       context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *logrus.Logger
	controlChannel  *quic.Stream
	quicConn        *quic.Conn
	usageMonitor    *web.Usage
	restartMutex    sync.Mutex
	poolConnections int32
	loadConnections int32
	controlFlow     chan struct{}
}

type QuicConfig struct {
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

func NewQUICClient(parentCtx context.Context, config *QuicConfig, logger *logrus.Logger) *QuicTransport {
	
	ctx, cancel := context.WithCancel(parentCtx)

	
	client := &QuicTransport{
		config:          config,
		parentctx:       parentCtx,
		ctx:             ctx,
		cancel:          cancel,
		logger:          logger,
		controlChannel:  nil, 
		quicConn:        nil,
		usageMonitor:    web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		poolConnections: 0,
		loadConnections: 0,
		controlFlow:     make(chan struct{}, 100),
	}

	return client
}

func (c *QuicTransport) Start() {
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}

	c.config.TunnelStatus = "Disconnected (QUIC)"

	go c.channelDialer()
}

func (c *QuicTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Info("restarting client...")

	
	level := c.logger.Level
	c.logger.SetLevel(logrus.FatalLevel)

	if c.cancel != nil {
		c.cancel()
	}

	
	if c.quicConn != nil {
		c.quicConn.CloseWithError(0, "restart")
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	c.ctx = ctx
	c.cancel = cancel

	
	c.controlChannel = nil
	c.quicConn = nil
	c.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", c.config.WebPort), ctx, c.config.SnifferLog, c.config.Sniffer, &c.config.TunnelStatus, c.logger)
	c.config.TunnelStatus = ""
	c.poolConnections = 0
	c.loadConnections = 0
	c.controlFlow = make(chan struct{}, 100)

	
	c.logger.SetLevel(level)

	go c.Start()
}

func (c *QuicTransport) channelDialer() {
	c.logger.Info("attempting to establish a new QUIC control channel connection...")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			
			quicConn, err := network.QUICDialer(c.ctx, c.config.RemoteAddr, c.config.DialTimeOut, 3)
			if err != nil {
				c.logger.Errorf("QUIC dialer: %v", err)
				time.Sleep(c.config.RetryInterval)
				continue
			}

			
			stream, err := quicConn.OpenStreamSync(c.ctx)
			if err != nil {
				c.logger.Errorf("failed to open control stream: %v", err)
				quicConn.CloseWithError(0, "stream open failed")
				time.Sleep(c.config.RetryInterval)
				continue
			}

			
			err = sendBinaryTransportString(stream, c.config.Token, utils.SG_Chan)
			if err != nil {
				c.logger.Errorf("failed to send security token: %v", err)
				stream.Close()
				quicConn.CloseWithError(0, "token send failed")
				continue
			}

			
			stream.SetReadDeadline(time.Now().Add(2 * time.Second))

			
			message, _, err := receiveBinaryTransportString(stream)
			if err != nil {
				c.logger.Errorf("failed to receive control channel response: %v", err)
				stream.Close()
				quicConn.CloseWithError(0, "token response failed")
				time.Sleep(c.config.RetryInterval)
				continue
			}

			
			stream.SetReadDeadline(time.Time{})

			if message == c.config.Token {
				c.quicConn = quicConn
				c.controlChannel = stream
				c.logger.Info("QUIC control channel established successfully")

				c.config.TunnelStatus = "Connected (QUIC)"
				go c.poolMaintainer()
				go c.channelHandler()

				return

			} else {
				c.logger.Errorf("invalid token received. Expected: %s, Received: %s. Retrying...", c.config.Token, message)
				stream.Close()
				quicConn.CloseWithError(0, "invalid token")
				time.Sleep(c.config.RetryInterval)
				continue
			}
		}
	}
}

func (c *QuicTransport) poolMaintainer() {
	
	for i := 0; i < c.config.ConnPoolSize; i++ {
		go c.streamDialer()
	}

	
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

	newPoolSize := c.config.ConnPoolSize 
	var poolConnectionsSum int32 = 0

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-tickerPool.C:
			
			atomic.AddInt32(&poolConnectionsSum, atomic.LoadInt32(&c.poolConnections))

		case <-tickerLoad.C:
			
			loadConnections := (int(atomic.LoadInt32(&c.loadConnections)) + 9) / 10 
			atomic.StoreInt32(&c.loadConnections, 0)                                

			
			poolConnectionsAvg := (int(atomic.LoadInt32(&poolConnectionsSum)) + 9) / 10 
			atomic.StoreInt32(&poolConnectionsSum, 0)                                   

			
			if (loadConnections + a) > poolConnectionsAvg*b {
				c.logger.Debugf("increasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize+1, poolConnectionsAvg, loadConnections)
				newPoolSize++

				
				go c.streamDialer()
			} else if float64(loadConnections+x) < float64(poolConnectionsAvg)*y && newPoolSize > c.config.ConnPoolSize {
				c.logger.Debugf("decreasing pool size: %d -> %d, avg pool conn: %d, avg load conn: %d", newPoolSize, newPoolSize-1, poolConnectionsAvg, loadConnections)
				newPoolSize--

				
				c.controlFlow <- struct{}{}
			}
		}
	}
}

func (c *QuicTransport) channelHandler() {
	msgChan := make(chan byte, 1000)

	
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				msg, err := receiveBinaryByte(c.controlChannel)
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

	
	for {
		select {
		case <-c.ctx.Done():
			_ = sendBinaryByte(c.controlChannel, utils.SG_Closed)
			return

		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				atomic.AddInt32(&c.loadConnections, 1)

				select {
				case <-c.controlFlow:
					
				default:
					
					c.logger.Debug("channel signal received, initiating stream dialer")
					go c.streamDialer()
				}

			case utils.SG_HB:
				c.logger.Debug("heartbeat signal received successfully")

			case utils.SG_Closed:
				c.logger.Warn("control channel has been closed by the server")
				go c.Restart()
				return

			case utils.SG_RTT:
				err := sendBinaryByte(c.controlChannel, utils.SG_RTT)
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


func (c *QuicTransport) streamDialer() {
	c.logger.Debugf("initiating new QUIC stream to tunnel server")

	stream, err := c.quicConn.OpenStreamSync(c.ctx)
	if err != nil {
		c.logger.Error("failed to open QUIC stream: ", err)
		return
	}

	
	err = sendBinaryByte(stream, utils.SG_Chan)
	if err != nil {
		c.logger.Error("failed to send ready signal: ", err)
		stream.Close()
		return
	}

	
	atomic.AddInt32(&c.poolConnections, 1)

	
	remoteAddr, transport, err := receiveBinaryTransportString(stream)

	
	atomic.AddInt32(&c.poolConnections, -1)

	if err != nil {
		c.logger.Debugf("failed to receive port from tunnel stream: %v", err)
		stream.Close()
		return
	}

	
	port, resolvedAddr, err := network.ResolveRemoteAddr(remoteAddr)
	if err != nil {
		c.logger.Infof("failed to resolve remote port: %v", err)
		stream.Close()
		return
	}

	switch transport {
	case utils.SG_TCP:
		
		c.localDialer(stream, resolvedAddr, port)

	case utils.SG_UDP:
		
		c.logger.Debug("UDP over QUIC not yet implemented")
		stream.Close()

	default:
		c.logger.Error("undefined transport. close the connection.")
		stream.Close()
	}
}

func (c *QuicTransport) localDialer(stream *quic.Stream, resolvedAddr string, port int) {
	var sendBuf, recvBuf int

	if strings.Contains(resolvedAddr, "127.0.0.1") {
		
		sendBuf = 32 * 1024
		recvBuf = 32 * 1024
	} else {
		
		sendBuf = 0
		recvBuf = 0
	}

	localConnection, err := network.TcpDialer(c.ctx, resolvedAddr, "", c.config.DialTimeOut, 30*time.Second, true, 1, recvBuf, sendBuf, 0)
	if err != nil {
		c.logger.Errorf("local dialer: %v", err)
		stream.Close()
		return
	}

	c.logger.Debugf("connected to local address %s successfully", resolvedAddr)

	
	c.handleQUICStream(stream, localConnection, port)
}


func (c *QuicTransport) handleQUICStream(stream *quic.Stream, localConn net.Conn, port int) {
	defer stream.Close()
	defer localConn.Close()

	done := make(chan struct{}, 2)

	
	go func() {
		defer func() { done <- struct{}{} }()
		defer stream.Close()

		buf := make([]byte, 32*1024)
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				localConn.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := localConn.Read(buf)
				if err != nil {
					if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
						c.logger.Debugf("failed to read from local connection: %v", err)
					}
					return
				}
				localConn.SetReadDeadline(time.Time{})

				if c.config.Sniffer {
					c.usageMonitor.AddOrUpdatePort(port, uint64(n))
				}

				_, err = stream.Write(buf[:n])
				if err != nil {
					c.logger.Debugf("failed to write to QUIC stream: %v", err)
					return
				}
			}
		}
	}()

	
	go func() {
		defer func() { done <- struct{}{} }()
		defer localConn.Close()

		buf := make([]byte, 32*1024)
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				stream.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := stream.Read(buf)
				if err != nil {
					c.logger.Debugf("failed to read from QUIC stream: %v", err)
					return
				}
				stream.SetReadDeadline(time.Time{})

				if c.config.Sniffer {
					c.usageMonitor.AddOrUpdatePort(port, uint64(n))
				}

				_, err = localConn.Write(buf[:n])
				if err != nil {
					c.logger.Debugf("failed to write to local connection: %v", err)
					return
				}
			}
		}
	}()

		
	<-done
}


func receiveBinaryByte(stream *quic.Stream) (byte, error) {
	buf := make([]byte, 1)
	_, err := io.ReadFull(stream, buf)
	if err != nil {
		return 0, err
	}
	return buf[0], nil
}


func sendBinaryByte(stream *quic.Stream, data byte) error {
	buf := []byte{data}
	_, err := stream.Write(buf)
	return err
}


func receiveBinaryTransportString(stream *quic.Stream) (string, byte, error) {
	
	header := make([]byte, 3)
	_, err := io.ReadFull(stream, header)
	if err != nil {
		return "", 0, err
	}

	
	strLen := binary.BigEndian.Uint16(header[:2])
	transport := header[2]

	
	if strLen > 0 {
		strData := make([]byte, strLen)
		_, err = io.ReadFull(stream, strData)
		if err != nil {
			return "", 0, err
		}
		return string(strData), transport, nil
	}

	return "", transport, nil
}


func sendBinaryTransportString(stream *quic.Stream, data string, transport byte) error {
	strLen := len(data)
	if strLen > 65535 {
		return fmt.Errorf("string too long: %d", strLen)
	}

	
	header := make([]byte, 3)
	binary.BigEndian.PutUint16(header[:2], uint16(strLen))
	header[2] = transport

	
	_, err := stream.Write(header)
	if err != nil {
		return err
	}

	
	if strLen > 0 {
		_, err = stream.Write([]byte(data))
		if err != nil {
			return err
		}
	}

	return nil
}
