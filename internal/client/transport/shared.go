package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/musix/backhaul/internal/config"
	"golang.org/x/exp/rand"
)

func ResolveRemoteAddr(remoteAddr string) (int, string, error) {
	// Split the address into host and port
	parts := strings.Split(remoteAddr, ":")
	var port int
	var err error

	// Handle cases where only the port is sent or host:port format
	if len(parts) < 2 {
		port, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, "", fmt.Errorf("invalid port format: %v", err)
		}
		// Default to localhost if only the port is provided
		return port, fmt.Sprintf("127.0.0.1:%d", port), nil
	}

	// If both host and port are provided
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", fmt.Errorf("invalid port format: %v", err)
	}

	// Return the full resolved address
	return port, remoteAddr, nil
}

func TcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, retry int, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
	var tcpConn *net.TCPConn
	var err error

	retries := retry           // Number of retries
	backoff := 1 * time.Second // Initial backoff duration

	for i := 0; i < retries; i++ {
		// Attempt to establish a TCP connection
		tcpConn, err = attemptTcpDialer(ctx, address, timeout, keepAlive, nodelay, SO_RCVBUF, SO_SNDBUF)
		if err == nil {
			// Connection successful
			return tcpConn, nil
		}

		// If this is the last retry, return the error
		if i == retries-1 {
			break
		}

		// Log retry attempt and wait before retrying
		time.Sleep(backoff)
		backoff *= 2 // Exponential backoff (double the wait time after each failure)
	}

	return nil, err
}

func attemptTcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
	//Resolve the address to a TCP address
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution: %v", err)
	}

	// Options
	dialer := &net.Dialer{
		Control: func(network, address string, s syscall.RawConn) error {
			err := ReusePortControl(network, address, s)
			if err != nil {
				return err
			}

			if SO_RCVBUF > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, SO_RCVBUF); err != nil {
						err = fmt.Errorf("failed to set SO_RCVBUF: %v", err)
					}
				})
			}
			if err != nil {
				return err
			}

			if SO_SNDBUF > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, SO_SNDBUF); err != nil {
						err = fmt.Errorf("failed to set SO_SNDBUF: %v", err)
					}
				})
			}

			return err

		},
		Timeout:   timeout,   // Set the connection timeout
		KeepAlive: keepAlive, // Set the keep-alive duration
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

	if !nodelay {
		err = tcpConn.SetNoDelay(false)
		if err != nil {
			tcpConn.Close()
			return nil, fmt.Errorf("failed to set TCP_NODELAY")
		}
	}

	return tcpConn, nil
}

func ReusePortControl(network, address string, s syscall.RawConn) error {
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

func WebSocketDialer(ctx context.Context, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, retry int, SO_RCVBUF int, SO_SNDBUF int) (*websocket.Conn, error) {
	var tunnelWSConn *websocket.Conn
	var err error

	retries := retry           // Number of retries
	backoff := 1 * time.Second // Initial backoff duration

	for i := 0; i < retries; i++ {
		// Attempt to dial the WebSocket
		tunnelWSConn, err = attemptDialWebSocket(ctx, addr, edgeIP, path, timeout, keepalive, nodelay, token, mode, SO_RCVBUF, SO_SNDBUF)
		if err == nil {
			// If successful, return the connection
			return tunnelWSConn, nil
		}

		// If this is the last retry, return the error
		if i == retries-1 {
			break
		}

		// Log the retry attempt and wait before retrying
		time.Sleep(backoff)
		backoff *= 2 // Exponential backoff (double the wait time after each failure)
	}

	return nil, err
}

func attemptDialWebSocket(ctx context.Context, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, SO_RCVBUF int, SO_SNDBUF int) (*websocket.Conn, error) {
	// Generate a random X-user-id
	rand.Seed(uint64(time.Now().UnixNano()))
	randomUserID := rand.Int31() // Generate a random int64 number

	// List of 30 diverse User-Agent strings from various browsers and platforms
	userAgents := []string{
		// Chrome
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 12; Pixel 5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 9; SM-G960F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.5359.128 Mobile Safari/537.36",
		// Firefox
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:114.0) Gecko/20100101 Firefox/114.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:102.0) Gecko/20100101 Firefox/102.0",
		"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:115.0) Gecko/20100101 Firefox/115.0",
		"Mozilla/5.0 (Linux; Android 10; Pixel 4 XL) Gecko/20100101 Firefox/96.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 14_6 like Mac OS X) Gecko/20100101 Firefox/90.0",
		// Safari
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.0 Safari/605.1.15",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 15_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.1 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (iPad; CPU OS 14_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.0 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.1.2 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		// Edge
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36 Edg/91.0.864.64",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/95.0.4638.69 Safari/537.36 Edg/95.0.1020.40",
		"Mozilla/5.0 (Linux; Android 11; SM-G998U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.210 Mobile Safari/537.36 EdgA/46.3.4.5155",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/111.0.0.0 Safari/537.36 Edg/111.0.1661.44",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36 Edg/115.0.1901.183",
		// Opera
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/112.0.0.0 Safari/537.36 OPR/97.0.4719.63",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36 OPR/98.0.4759.15",
		"Mozilla/5.0 (Linux; Android 10; SM-N975F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Mobile Safari/537.36 OPR/65.2.3381.61420",
		"Mozilla/5.0 (Linux; Android 11; SM-G998U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.5735.196 Mobile Safari/537.36 OPR/71.2.3767.68577",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Safari/537.36 OPR/99.0.4759.21",
		// Older Browsers
		"Mozilla/4.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0)",
		"Mozilla/4.0 (compatible; MSIE 6.0; Windows NT 5.1; SV1)",
		"Mozilla/5.0 (Windows NT 6.1; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/89.0.4389.82 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36",
	}

	// Pick a random User-Agent
	randomUserAgent := userAgents[rand.Intn(len(userAgents))]

	// Setup headers with authorization and X-user-id
	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %v", token))
	headers.Add("X-User-Id", fmt.Sprintf("%d", randomUserID))
	headers.Add("User-Agent", randomUserAgent)

	var wsURL string
	dialer := websocket.Dialer{}

	// Handle edgeIP assignment
	if edgeIP != "" {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid address format, failed to parse: %w", err)
		}

		edgeIP = fmt.Sprintf("%s:%s", edgeIP, port)
	} else {
		edgeIP = addr
	}

	// path generation
	if path != "/channel" {
		path = fmt.Sprintf("%s/%s", path, strconv.Itoa(int(randomUserID)))
	}

	if mode == config.WS || mode == config.WSMUX {
		wsURL = fmt.Sprintf("ws://%s%s", addr, path)

		dialer = websocket.Dialer{
			EnableCompression: true,
			HandshakeTimeout:  45 * time.Second, // default handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := TcpDialer(ctx, edgeIP, timeout, keepalive, nodelay, 1, SO_RCVBUF, SO_SNDBUF)
				if err != nil {
					return nil, err
				}
				return conn, nil
			},
		}
	} else if mode == config.WSS || mode == config.WSSMUX {
		wsURL = fmt.Sprintf("wss://%s%s", addr, path)

		// Create a TLS configuration that allows insecure connections
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true, // Skip server certificate verification
		}

		dialer = websocket.Dialer{
			EnableCompression: true,
			TLSClientConfig:   tlsConfig,        // Pass the insecure TLS config here
			HandshakeTimeout:  45 * time.Second, // default handshake timeout
			NetDial: func(_, addr string) (net.Conn, error) {
				conn, err := TcpDialer(ctx, edgeIP, timeout, keepalive, nodelay, 1, SO_RCVBUF, SO_SNDBUF)
				if err != nil {
					return nil, err
				}
				return conn, nil
			},
		}
	}

	// Dial to the WebSocket server
	tunnelWSConn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return nil, err
	}
	return tunnelWSConn, nil
}
