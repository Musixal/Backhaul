package utils

import (
	"errors"
	"io"
	"net"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// WebSocketToTCPConnectionHandler handles data transfer between a WebSocket and a TCP connection
func WSToTCPConnHandler(wsConn *websocket.Conn, tcpConn net.Conn, logger *logrus.Logger) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		transferWebSocketToTCP(wsConn, tcpConn, logger)
	}()

	transferTCPToWebSocket(tcpConn, wsConn, logger)

	<-done

	wsConn.Close()
	tcpConn.Close()
}

// transferWebSocketToTCP transfers data from a WebSocket connection to a TCP connection
func transferWebSocketToTCP(wsConn *websocket.Conn, tcpConn net.Conn, logger *logrus.Logger) {
	for {
		// Read message from the WebSocket connection
		messageType, message, err := wsConn.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, io.EOF) {
				logger.Trace("WebSocket reader stream closed or EOF received")
			} else {
				logger.Trace("unable to read from the WebSocket connection: ", err)
			}
			wsConn.Close()
			tcpConn.Close()
			return
		}

		// Only handle text or binary messages (ignore control messages like pings)
		if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
			// Write the message to the TCP connection
			_, err = tcpConn.Write(message)
			if err != nil {
				logger.Trace("unable to write to the TCP connection: ", err)
				wsConn.Close()
				tcpConn.Close()
				return
			}
			logger.Tracef("transferred data from WebSocket to TCP: %d bytes", len(message))
		}
	}
}

// transferTCPToWebSocket transfers data from a TCP connection to a WebSocket connection
func transferTCPToWebSocket(tcpConn net.Conn, wsConn *websocket.Conn, logger *logrus.Logger) {
	buf := make([]byte, 16*1024) // 16K buffer size
	for {
		// Read data from the TCP connection
		n, err := tcpConn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logger.Trace("TCP reader stream closed or EOF received")
			} else {
				logger.Trace("unable to read from the TCP connection: ", err)
			}
			tcpConn.Close()
			wsConn.Close()
			return
		}

		// Write the data to the WebSocket connection as a binary message
		err = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
		if err != nil {
			if errors.Is(err, websocket.ErrCloseSent) || errors.Is(err, io.EOF) {
				logger.Trace("WebSocket writer stream closed or EOF received")
			} else {
				logger.Trace("unable to write to the WebSocket connection: ", err)
			}
			tcpConn.Close()
			wsConn.Close()
			return
		}

		logger.Tracef("transferred data from TCP to WebSocket: %d bytes", n)
	}
}
