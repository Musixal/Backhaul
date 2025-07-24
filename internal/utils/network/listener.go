//go:build !windows
// +build !windows

package network

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// listenWithBuffers creates a TCP listener with specified SO_RCVBUF and SO_SNDBUF sizes.
// It returns a net.Listener and an error if any.
func ListenWithBuffers(network, address string, rcvBufSize, sndBufSize, mss int, keepAlivePeriod time.Duration, dis_nodelay bool) (net.Listener, error) {
	// Options
	listenerCfg := &net.ListenConfig{
		Control: func(network, address string, s syscall.RawConn) error {
			// Set socket options for SO_REUSEADDR and SO_REUSEPORT
			err := ReusePortControl(network, address, s)
			if err != nil {
				return err
			}

			// Set SO_RCVBUF
			if rcvBufSize > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize); err != nil {
						err = fmt.Errorf("failed to set SO_RCVBUF: %v", err)
					}
				})
			}
			if err != nil {
				return err
			}

			// Set SO_SNDBUF
			if sndBufSize > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBufSize); err != nil {
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

			// Set TCP_NODELAY based on tcpNoDelay flag (enabled or disabled)
			if !dis_nodelay {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1); err != nil {
						err = fmt.Errorf("failed to set TCP_NODELAY: %v", err)
					}
				})
			} else {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 0); err != nil {
						err = fmt.Errorf("failed to unset TCP_NODELAY: %v", err)
					}
				})
			}

			return err

		},
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Interval: keepAlivePeriod, Count: 9, Idle: 0},
	}

	// Dial the TCP connection with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	listener, err := listenerCfg.Listen(ctx, network, address)
	if err != nil {
		return nil, err
	}

	return listener, err
}
