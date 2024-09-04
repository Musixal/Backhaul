package utils

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/gorilla/websocket"
)

// ReceivePort reads a 2-byte big-endian unsigned integer directly from the connection
func ReceiveBinaryInt(conn net.Conn) (uint16, error) {
	var port uint16

	// Use binary.Read to read the port directly from the connection
	err := binary.Read(conn, binary.BigEndian, &port)
	if err != nil {
		return 0, fmt.Errorf("failed to read port number from connection: %w", err)
	}

	// Successful
	return port, nil
}

func ReceiveBinaryString(conn net.Conn) (string, error) {
	// Create a buffer to read the first 2 bytes (the length of the message)
	lenBuf := make([]byte, 2)

	// Read exactly 2 bytes for the message length
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return "", fmt.Errorf("failed to read message length: %w", err)
	}

	// Decode the length of the message from the 2-byte buffer
	messageLength := binary.BigEndian.Uint16(lenBuf)

	// Create a buffer of the appropriate size to hold the message
	messageBuf := make([]byte, messageLength)

	// Read the message data into the buffer
	if _, err := io.ReadFull(conn, messageBuf); err != nil {
		return "", fmt.Errorf("failed to read message: %w", err)
	}
	// Convert the message buffer to a string and return it
	return string(messageBuf), nil
}

// SendPort sends the port number as a 2-byte big-endian unsigned integer.
func SendBinaryInt(conn net.Conn, port uint16) error {
	// Create a 2-byte slice to hold the port number
	buf := make([]byte, 2)

	// Encode the port number as a big-endian 2-byte unsigned integer
	binary.BigEndian.PutUint16(buf, port)

	// Send the 2-byte buffer over the connection
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("failed to send port number %d: %w", port, err)
	}

	// Successful
	return nil
}

func SendBinaryString(conn net.Conn, message string) error {
	// Create a buffer with the appropriate size for the message
	buf := make([]byte, 2+len(message))

	// Encode the length of the message as a big-endian 2-byte unsigned integer
	binary.BigEndian.PutUint16(buf[:2], uint16(len(message)))

	// Copy the message into the buffer after the length
	copy(buf[2:], message)

	// Send the buffer over the connection
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	// Successful
	return nil
}

// ReceiveWebSocketInt reads a 2-byte big-endian unsigned integer from the WebSocket connection.
func ReceiveWebSocketInt(conn *websocket.Conn) (uint16, error) {
	var port uint16

	_, message, err := conn.ReadMessage()
	if err != nil {
		return 0, fmt.Errorf("failed to read message: %w", err)
	}

	if len(message) < 2 {
		return 0, fmt.Errorf("message too short to contain a valid port number")
	}

	port = binary.BigEndian.Uint16(message[:2])
	return port, nil
}

// SendWebSocketInt sends the port number as a 2-byte big-endian unsigned integer over a WebSocket connection.
func SendWebSocketInt(conn *websocket.Conn, port uint16) error {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, port)

	err := conn.WriteMessage(websocket.BinaryMessage, buf)
	if err != nil {
		return fmt.Errorf("failed to send port number %d: %w", port, err)
	}

	return nil
}
