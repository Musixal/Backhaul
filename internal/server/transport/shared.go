package transport

import (
	"net"
	"sync"

	"github.com/gorilla/websocket"
)

type TunnelChannel struct {
	conn *websocket.Conn
	ping chan struct{}
	mu   *sync.Mutex
}

type LocalTCPConn struct {
	conn       net.Conn
	remoteAddr string
}
