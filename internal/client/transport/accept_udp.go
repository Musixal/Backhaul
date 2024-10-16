package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"time"

	"github.com/musix/backhaul/internal/web"
	"github.com/sirupsen/logrus"
)

const BufferSize = 16 * 1024

func UDPDialer(tcp net.Conn, remoteAddr string, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	remoteUDPAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		logger.Fatalf("failed to resolve remote address: %v", err)
	}

	// Dial the remote UDP server
	remoteConn, err := net.DialUDP("udp", nil, remoteUDPAddr)
	if err != nil {
		logger.Fatalf("failed to dial remote UDP address: %v", err)
	}

	defer remoteConn.Close()

	done := make(chan struct{})

	go func() {
		go tcpToUDP(tcp, remoteConn, logger, usage, remotePort, sniffer)
		done <- struct{}{}
	}()

	udpToTCP(tcp, remoteConn, logger, usage, remotePort, sniffer)

	<-done
}

func tcpToUDP(tcp net.Conn, udp *net.UDPConn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	buf := make([]byte, BufferSize)
	lenBuf := make([]byte, 2) // 2-byte header for packet size

	for {
		// First, read the 2-byte packet size header from TCP
		_, err := io.ReadFull(tcp, lenBuf)
		if err != nil {
			if err == io.EOF {
				logger.Debug("TCP connection closed by client.")
			} else {
				logger.Errorf("failed to read packet size from TCP: %v", err)
			}
			return
		}

		// Convert the header to an integer
		packetSize := binary.BigEndian.Uint16(lenBuf)

		// Make sure the packet size doesn't exceed the buffer
		if int(packetSize) > len(buf) {
			logger.Errorf("packet size %d exceeds buffer size %d", packetSize, len(buf))
			return
		}

		// Now read the actual packet data from TCP
		_, err = io.ReadFull(tcp, buf[:packetSize])
		if err != nil {
			logger.Errorf("failed to read packet from TCP: %v", err)
			return
		}

		totalWritten := 0
		for totalWritten < int(packetSize) {
			w, err := udp.Write(buf[totalWritten:packetSize])
			if err != nil {
				logger.Errorf("failed to write to UDP connection: %v", err)
				return
			}
			totalWritten += w
		}

		logger.Tracef("read %d bytes from TCP, wrote %d bytes to UDP", packetSize, totalWritten)

		if sniffer {
			usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
		}
	}
}

func udpToTCP(tcp net.Conn, udp *net.UDPConn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	buf := make([]byte, BufferSize-6) // reserved for 5 bytes header

	// Pre-allocate headers
	header := make([]byte, 2)                                    // 2-byte header for packet size
	timestampHeader := make([]byte, 4)                           // 4-byte header for timestamp
	packetBuffer := bytes.NewBuffer(make([]byte, 0, BufferSize)) // Pre-allocated buffer for packets

	for {
		r, err := udp.Read(buf)
		if err != nil {
			logger.Errorf("failed to read from UDP connection: %v", err)
			return
		}

		// Check if the packet size exceeds the maximum limit
		if r > 65535 {
			logger.Errorf("packet too large, size: %d bytes", r)
			continue
		}

		// Store the current timestamp (in milliseconds)
		timestamp := time.Now().UnixMilli()

		// Get the last milliseconds within a 10-minute window
		// The number 599,999 is less than 2^20, which means needs only 20 bits (just under 3 bytes) to represent it.
		// Better than 8 bytes overhead for storing Unix Time in ms.
		lastMillis := timestamp % (10 * 60 * 1000) // 10 minutes in milliseconds

		// Using 4 bytes avoids manual slicing and is easier to work with in Go,
		// which natively handles 32-bit (4-byte) integers.
		binary.BigEndian.PutUint32(timestampHeader, uint32(lastMillis))

		// Store the packet size as a 2-byte header
		binary.BigEndian.PutUint16(header, uint16(r))

		// Clear and prepare the packet buffer
		packetBuffer.Reset()
		packetBuffer.Write(timestampHeader) // Write timestamp
		packetBuffer.Write(header)          // Write size header
		packetBuffer.Write(buf[:r])         // Write actual data

		// Write the packet (with timestamp and header) to the TCP connection
		totalWritten := 0
		packet := packetBuffer.Bytes()

		for totalWritten < len(packet) {
			w, err := tcp.Write(packet[totalWritten:])
			if err != nil {
				logger.Errorf("failed to write to TCP connection: %v", err)
				return
			}
			totalWritten += w
		}

		logger.Tracef("read %d bytes from UDP, wrote %d bytes to TCP", r, totalWritten)

		if sniffer {
			usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
		}
	}
}
