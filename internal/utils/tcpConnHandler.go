package utils

import (
	"io"
	"net"

	"github.com/musix/backhaul/internal/web"
	"github.com/sirupsen/logrus"
)

func TCPConnectionHandler(from net.Conn, to net.Conn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		transferData(from, to, logger, usage, remotePort, sniffer)
	}()

	transferData(to, from, logger, usage, remotePort, sniffer)

	<-done
}

// Using io.Copy for efficient data transfer
func transferData(from net.Conn, to net.Conn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	bytesCopied, err := io.Copy(to, from)
	if err != nil {
		logger.Trace("error during data transfer: ", err)
	}

	logger.Tracef("data transferred: %d bytes", bytesCopied)

	if sniffer {
		usage.AddOrUpdatePort(remotePort, uint64(bytesCopied))
	}

	from.Close()
	to.Close()
}
