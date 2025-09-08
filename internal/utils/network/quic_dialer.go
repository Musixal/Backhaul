package network

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/quic-go/quic-go"
)

func QUICDialer(ctx context.Context, remoteAddress string, timeout time.Duration, retry int) (*quic.Conn, error) {
	var conn *quic.Conn
	var err error

	retries := retry
	backoff := 1 * time.Second

	for i := 0; i < retries; i++ {
		conn, err = attemptQUICDialer(ctx, remoteAddress, timeout)
		if err == nil {
			return conn, nil
		}

		if i == retries-1 {
			break
		}

		time.Sleep(backoff)
		backoff *= 2
	}

	return nil, err
}

func attemptQUICDialer(ctx context.Context, remoteAddress string, timeout time.Duration) (*quic.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"backhaul-quic"},
	}

	quicConfig := &quic.Config{
		MaxIdleTimeout:        30 * time.Second,
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 1000,
		KeepAlivePeriod:       15 * time.Second,
	}

	conn, err := quic.DialAddr(dialCtx, remoteAddress, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("QUIC dial failed: %v", err)
	}

	return conn, nil
}
