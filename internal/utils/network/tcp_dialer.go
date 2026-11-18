package network

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

func TcpDialer(ctx context.Context, remoteAddress string, localSrc string, timeout time.Duration, keepAlive time.Duration, nodelay bool, retry int, SO_RCVBUF int, SO_SNDBUF, mss int) (*net.TCPConn, error) {
	var tcpConn *net.TCPConn
	var err error

	retries := retry           // Number of retries
	backoff := 1 * time.Second // Initial backoff duration

	for i := 0; i < retries; i++ {
		// Pick a fresh source address for every attempt. This is what makes
		// rotation and CIDR randomization effective across reconnects.
		src := localSrc
		if src == "" {
			src, err = pickDefaultSource()
			if err != nil {
				return nil, err
			}
		}

		// Attempt to establish a TCP connection
		tcpConn, err = attemptTcpDialer(ctx, remoteAddress, src, timeout, keepAlive, nodelay, SO_RCVBUF, SO_SNDBUF, mss)
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

// func attemptTcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
func attemptTcpDialer(
	ctx context.Context,
	remoteAddress string,
	localSrc string,
	timeout time.Duration,
	keepAlive time.Duration,
	nodelay bool,
	SO_RCVBUF int,
	SO_SNDBUF int,
	mss int,
) (*net.TCPConn, error) {

	//Resolve the address to a TCP address
	tcpAddr, err := net.ResolveTCPAddr("tcp", remoteAddress)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution: %v", err)
	}

	var localTCPAddr *net.TCPAddr
	if localSrc != "" {
		// A bare source IP is the common case; add an ephemeral port so it
		// can be resolved as a TCP address. IPv6 literals are handled too.
		if net.ParseIP(localSrc) != nil {
			localSrc = net.JoinHostPort(localSrc, "0")
		}
		localTCPAddr, err = net.ResolveTCPAddr("tcp", localSrc)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve local address: %v", err)
		}
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

			// Set MSS (Maximum Segment Size)
			if mss > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, mss); err != nil {
						err = fmt.Errorf("failed to set MSS: %v", err)
					}
				})
			}

			return err

		},
		Timeout:         timeout, // Set the connection timeout
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Interval: keepAlive, Count: 9, Idle: 0},
		LocalAddr:       localTCPAddr,
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
			return nil, fmt.Errorf("failed to set TCP_NODELAY to %v: %w", nodelay, err)
		}
	}

	return tcpConn, nil
}
