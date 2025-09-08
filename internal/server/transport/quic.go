package transport

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/utils/network"
	"github.com/musix/backhaul/internal/web"
	"github.com/quic-go/quic-go"

	"github.com/sirupsen/logrus"
)

type QuicTransport struct {
	config         *QuicConfig
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	streamChannel  chan *quic.Stream
	localChannel   chan LocalTCPConn
	reqNewConnChan chan struct{}
	controlChannel *quic.Stream
	quicListener   *quic.Listener
	restartMutex   sync.Mutex
	usageMonitor   *web.Usage
	rtt            int64 
}

type QuicConfig struct {
	BindAddr      string
	Token         string
	SnifferLog    string
	TunnelStatus  string
	Ports         []string
	Sniffer       bool
	Heartbeat     time.Duration 
	ChannelSize   int
	WebPort       int
	TLSCertFile   string
	TLSKeyFile    string
}

func NewQUICServer(parentCtx context.Context, config *QuicConfig, logger *logrus.Logger) *QuicTransport {
	
	ctx, cancel := context.WithCancel(parentCtx)

	
	server := &QuicTransport{
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		streamChannel:  make(chan *quic.Stream, config.ChannelSize),
		localChannel:   make(chan LocalTCPConn, config.ChannelSize),
		reqNewConnChan: make(chan struct{}, config.ChannelSize),
		controlChannel: nil, 
		quicListener:   nil,
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		rtt:            0,
	}

	return server
}

func (s *QuicTransport) Start() {
	s.config.TunnelStatus = "Disconnected (QUIC)"

	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}

	go s.streamListener()

	s.channelHandshake()

	if s.controlChannel != nil {
		s.config.TunnelStatus = "Connected (QUIC)"

		go s.parsePortMappings()
		go s.channelHandler()

		s.logger.Info("QUIC server ready - data streams will be handled directly")
	}
}

func (s *QuicTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")

	
	level := s.logger.Level
	s.logger.SetLevel(logrus.FatalLevel)

	if s.cancel != nil {
		s.cancel()
	}

	
	if s.quicListener != nil {
		s.quicListener.Close()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	
	s.streamChannel = make(chan *quic.Stream, s.config.ChannelSize)
	s.localChannel = make(chan LocalTCPConn, s.config.ChannelSize)
	s.reqNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""
	s.controlChannel = nil
	s.quicListener = nil

	
	s.logger.SetLevel(level)

	go s.Start()
}

func (s *QuicTransport) channelHandshake() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case stream := <-s.streamChannel:
			
			if err := stream.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				s.logger.Errorf("failed to set read deadline: %v", err)
				stream.Close()
				continue
			}

			msg, transport, err := receiveBinaryTransportString(stream)
			if transport != utils.SG_Chan {
				s.logger.Errorf("invalid signal received for channel, Discarding connection")
				stream.Close()
				continue
			} else if err != nil {
				if _, ok := err.(net.Error); ok {
					s.logger.Warn("timeout while waiting for control channel signal")
				} else {
					s.logger.Errorf("failed to receive control channel signal: %v", err)
				}
				stream.Close() 
				continue
			}

			
			stream.SetReadDeadline(time.Time{})

			if msg != s.config.Token {
				s.logger.Warnf("invalid security token received: %s", msg)
				stream.Close()
				continue
			}

			err = sendBinaryTransportString(stream, s.config.Token, utils.SG_Chan)
			if err != nil {
				s.logger.Errorf("failed to send security token: %v", err)
				stream.Close()
				continue
			}

			s.controlChannel = stream

			s.logger.Info("QUIC control channel successfully established.")
			return
		}
	}
}

func (s *QuicTransport) channelHandler() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	
	messageChan := make(chan byte, 1)

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				message, err := receiveBinaryByte(s.controlChannel)
				if err != nil {
					if s.cancel != nil {
						s.logger.Error("failed to read from channel connection. ", err)
						go s.Restart()
					}
					return
				}
				messageChan <- message
			}
		}
	}()

	
	rtt := time.Now()
	err := sendBinaryByte(s.controlChannel, utils.SG_RTT)
	if err != nil {
		s.logger.Error("failed to send RTT signal, attempting to restart server...")
		go s.Restart()
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			_ = sendBinaryByte(s.controlChannel, utils.SG_Closed)
			return

		case <-s.reqNewConnChan:
			err := sendBinaryByte(s.controlChannel, utils.SG_Chan)
			if err != nil {
				s.logger.Error("failed to send request new connection signal. ", err)
				go s.Restart()
				return
			}

		case <-ticker.C:
			err := sendBinaryByte(s.controlChannel, utils.SG_HB)
			if err != nil {
				s.logger.Error("failed to send heartbeat signal")
				go s.Restart()
				return
			}
			s.logger.Trace("heartbeat signal sent successfully")

		case message, ok := <-messageChan:
			if !ok {
				s.logger.Error("channel closed, likely due to an error in QUIC read")
				return
			}

			if message == utils.SG_Closed {
				s.logger.Warn("control channel has been closed by the client")
				go s.Restart()
				return

			} else if message == utils.SG_RTT {
				measureRTT := time.Since(rtt)
				s.rtt = measureRTT.Milliseconds()
				s.logger.Infof("Round Trip Time (RTT): %d ms", s.rtt)
			}
		}
	}
}

func (s *QuicTransport) streamListener() {
	listener, err := network.QUICListen(s.config.BindAddr, s.config.TLSCertFile, s.config.TLSKeyFile)
	if err != nil {
		s.logger.Fatalf("failed to start QUIC listener on %s: %v", s.config.BindAddr, err)
		return
	}

	defer listener.Close()
	s.quicListener = listener

	s.logger.Infof("QUIC server started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptQUICConn(listener)

	<-s.ctx.Done()
}

func (s *QuicTransport) acceptQUICConn(listener *quic.Listener) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debugf("waiting for incoming QUIC connection on %s", listener.Addr().String())
			conn, err := listener.Accept(s.ctx)
			if err != nil {
				s.logger.Debugf("failed to accept QUIC connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			
			go s.handleQUICConnection(conn)
		}
	}
}

func (s *QuicTransport) handleQUICConnection(conn *quic.Conn) {
	defer conn.CloseWithError(0, "connection handler finished")

	
	isFirstStream := true

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			
			stream, err := conn.AcceptStream(s.ctx)
			if err != nil {
				s.logger.Debugf("failed to accept stream from %s: %v", conn.RemoteAddr().String(), err)
				return
			}

			s.logger.Debugf(" STREAM RECEIVED: isFirstStream=%v, controlChannel=%v", isFirstStream, s.controlChannel != nil)

			if isFirstStream {
				
				isFirstStream = false
				select {
				case s.streamChannel <- stream:
					s.logger.Debugf("accepted control channel stream from %s", conn.RemoteAddr().String())
				default: 
					s.logger.Warnf("stream channel is full, discarding control stream from %s", conn.RemoteAddr().String())
					stream.Close()
				}
			} else {
				
				s.logger.Debugf("RECEIVED DATA STREAM from %s", conn.RemoteAddr().String())
				go s.handleDataStream(stream)
			}
		}
	}
}

func (s *QuicTransport) handleDataStream(stream *quic.Stream) {
	s.logger.Debugf("handleDataStream called!")
	

	
	select {
	case localConn := <-s.localChannel:
		
		if time.Now().UnixMilli()-localConn.timeCreated > 3000 {
			s.logger.Debugf("local connection timed out: %d ms", time.Now().UnixMilli()-localConn.timeCreated)
			localConn.conn.Close()
			stream.Close()
			return
		}

		s.logger.Debugf(" matching data stream with local connection for %s", localConn.remoteAddr)

		
		err := sendBinaryTransportString(stream, localConn.remoteAddr, utils.SG_TCP)
		if err != nil {
			s.logger.Errorf("failed to send remote address to client: %v", err)
			localConn.conn.Close()
			stream.Close()
			return
		}

		s.logger.Debugf(" sent remote address %s to client, starting data forwarding", localConn.remoteAddr)

		
		go s.handleQUICStream(localConn.conn, stream, localConn.conn.LocalAddr().(*net.TCPAddr).Port)

	default:
		
		s.logger.Debug(" no waiting local connection for data stream")
		stream.Close()
		return
	}
}

func (s *QuicTransport) parsePortMappings() {
	for _, portMapping := range s.config.Ports {
		parts := strings.Split(portMapping, "=")

		var localAddr, remoteAddr string

		
		if len(parts) == 1 {
			localPortOrRange := strings.TrimSpace(parts[0])
			remoteAddr = localPortOrRange 

			
			if strings.Contains(localPortOrRange, "-") {
				rangeParts := strings.Split(localPortOrRange, "-")
				if len(rangeParts) != 2 {
					s.logger.Fatalf("invalid port range format: %s", localPortOrRange)
				}

				
				startPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				if err != nil || startPort < 1 || startPort > 65535 {
					s.logger.Fatalf("invalid start port in range: %s", rangeParts[0])
				}

				endPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err != nil || endPort < 1 || endPort > 65535 || endPort < startPort {
					s.logger.Fatalf("invalid end port in range: %s", rangeParts[1])
				}

				
				for port := startPort; port <= endPort; port++ {
					localAddr = fmt.Sprintf(":%d", port)
					go s.localListener(localAddr, strconv.Itoa(port)) 
					time.Sleep(1 * time.Millisecond)                   
				}
				continue
			} else {
				
				port, err := strconv.Atoi(localPortOrRange)
				if err != nil || port < 1 || port > 65535 {
					s.logger.Fatalf("invalid port format: %s", localPortOrRange)
				}
				localAddr = fmt.Sprintf(":%d", port)
			}
		} else if len(parts) == 2 {
			
			localPortOrRange := strings.TrimSpace(parts[0])
			remoteAddr = strings.TrimSpace(parts[1])

			
			if strings.Contains(localPortOrRange, "-") {
				rangeParts := strings.Split(localPortOrRange, "-")
				if len(rangeParts) != 2 {
					s.logger.Fatalf("invalid port range format: %s", localPortOrRange)
				}

				
				startPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				if err != nil || startPort < 1 || startPort > 65535 {
					s.logger.Fatalf("invalid start port in range: %s", rangeParts[0])
				}

				endPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err != nil || endPort < 1 || endPort > 65535 || endPort < startPort {
					s.logger.Fatalf("invalid end port in range: %s", rangeParts[1])
				}

				
				for port := startPort; port <= endPort; port++ {
					localAddr = fmt.Sprintf(":%d", port)
					go s.localListener(localAddr, remoteAddr)
					time.Sleep(1 * time.Millisecond) 
				}
				continue
			} else {
				
				port, err := strconv.Atoi(localPortOrRange)
				if err == nil && port > 1 && port < 65535 { 
					localAddr = fmt.Sprintf(":%d", port)
				} else {
					localAddr = localPortOrRange 
				}
			}
		} else {
			s.logger.Fatalf("invalid port mapping format: %s", portMapping)
		}
		
		go s.localListener(localAddr, remoteAddr)
	}
}

func (s *QuicTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", localAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("TCP listener started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptLocalConn(listener, remoteAddr)

	<-s.ctx.Done()
}

func (s *QuicTransport) acceptLocalConn(listener net.Listener, remoteAddr string) {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			s.logger.Debugf("waiting for accept incoming connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("disarded non-TCP connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			select {
			case s.localChannel <- LocalTCPConn{conn: conn, remoteAddr: remoteAddr, timeCreated: time.Now().UnixMilli()}:

				select {
				case s.reqNewConnChan <- struct{}{}:
					
				default:
					
					s.logger.Warn("channel is full, cannot request a new connection")
				}

				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default: 
				s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
				conn.Close()
			}
		}
	}
}

func (s *QuicTransport) handleQUICStream(localConn net.Conn, stream *quic.Stream, port int) {
	defer stream.Close()
	defer localConn.Close()

	done := make(chan struct{}, 2)

	
	go func() {
		defer func() { done <- struct{}{} }()
		defer stream.Close()

		buf := make([]byte, 32*1024)
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				localConn.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := localConn.Read(buf)
				if err != nil {
					if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
						s.logger.Debugf("failed to read from local connection: %v", err)
					}
					return
				}
				localConn.SetReadDeadline(time.Time{})

				if s.config.Sniffer {
					s.usageMonitor.AddOrUpdatePort(port, uint64(n))
				}

				_, err = stream.Write(buf[:n])
				if err != nil {
					s.logger.Debugf("failed to write to QUIC stream: %v", err)
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
			case <-s.ctx.Done():
				return
			default:
				stream.SetReadDeadline(time.Now().Add(30 * time.Second))
				n, err := stream.Read(buf)
				if err != nil {
					s.logger.Debugf("failed to read from QUIC stream: %v", err)
					return
				}
				stream.SetReadDeadline(time.Time{})

				if s.config.Sniffer {
					s.usageMonitor.AddOrUpdatePort(port, uint64(n))
				}

				_, err = localConn.Write(buf[:n])
				if err != nil {
					s.logger.Debugf("failed to write to local connection: %v", err)
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
