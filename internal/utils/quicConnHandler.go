package utils

import (
	"errors"
	"io"
	"net"

	"github.com/musix/backhaul/internal/web"
	"github.com/quic-go/quic-go"
	"github.com/sirupsen/logrus"
)

func QConnectionHandler(from net.Conn, to quic.Stream, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		q1transferData(from, to, from, to, logger, usage, remotePort, sniffer)
	}()

	q1transferData(to, from, from, to, logger, usage, remotePort, sniffer)

	<-done
}

// Using direct Read and Write for transferring data
func q1transferData(from io.ReadWriter, to io.ReadWriter, tcp net.Conn, quic quic.Stream, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	buf := make([]byte, 16*1024) // 16K
	for {
		// Read data from the source connection
		r, err := from.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logger.Trace("reader stream closed or EOF received")
			} else {
				logger.Trace("unable to read from the connection: ", err)
			}
			tcp.Close()
			quic.Close()
			return
		}

		totalWritten := 0
		for totalWritten < r {
			// Write data to the destination connection
			w, err := to.Write(buf[totalWritten:r])
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					logger.Trace("writer stream closed or EOF received")
				} else {
					logger.Trace("unable to write to the connection: ", err)
				}
				tcp.Close()
				quic.Close()
				return
			}
			totalWritten += w
		}

		logger.Tracef("read data: %d bytes, written data: %d bytes", r, totalWritten)
		if sniffer {
			usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
		}
	}

}
