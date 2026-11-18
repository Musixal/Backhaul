package network

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestTcpDialerUsesConfiguredSourceIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	selector, err := NewSourceSelector("127.0.0.2", nil, "")
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}
	SetDefaultSourceSelector(selector)
	defer SetDefaultSourceSelector(nil)

	conn, err := TcpDialer(context.Background(), listener.Addr().String(), "", time.Second, time.Second, true, 1, 0, 0, 0)
	if err != nil {
		t.Fatalf("TcpDialer() error = %v", err)
	}
	defer conn.Close()

	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	defer accepted.Close()

	host, _, err := net.SplitHostPort(accepted.RemoteAddr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	if host != "127.0.0.2" {
		t.Fatalf("source IP = %q, want %q", host, "127.0.0.2")
	}
}

func TestTcpDialerRotatesSourceIPs(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	selector, err := NewSourceSelector("", []string{"127.0.0.2", "127.0.0.3"}, SourceRoundRobin.String())
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}
	SetDefaultSourceSelector(selector)
	defer SetDefaultSourceSelector(nil)

	for _, want := range []string{"127.0.0.2", "127.0.0.3"} {
		conn, err := TcpDialer(context.Background(), listener.Addr().String(), "", time.Second, time.Second, true, 1, 0, 0, 0)
		if err != nil {
			t.Fatalf("TcpDialer() error = %v", err)
		}

		accepted, err := listener.Accept()
		if err != nil {
			conn.Close()
			t.Fatalf("Accept() error = %v", err)
		}

		host, _, err := net.SplitHostPort(accepted.RemoteAddr().String())
		accepted.Close()
		conn.Close()
		if err != nil {
			t.Fatalf("SplitHostPort() error = %v", err)
		}
		if host != want {
			t.Fatalf("source IP = %q, want %q", host, want)
		}
	}
}
