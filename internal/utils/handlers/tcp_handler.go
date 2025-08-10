package handlers

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/musix/backhaul/internal/stats"
	"github.com/sirupsen/logrus"
)

func TCPConnectionHandler(ctx context.Context, from net.Conn, to net.Conn, logger *logrus.Logger, remotePort int) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		transferData(from, to, logger, remotePort)
	}()

	transferData(to, from, logger, remotePort)

	select {
	case <-ctx.Done():
		from.Close()
		to.Close()
		return
	case <-done:
	}
}

// Using direct Read and Write for transferring data
func transferData(from net.Conn, to net.Conn, logger *logrus.Logger, remotePort int) {
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
			from.Close()
			to.Close()
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
				from.Close()
				to.Close()
				return

			}
			totalWritten += w
		}

		logger.Tracef("read data: %d bytes, written data: %d bytes", r, totalWritten)
		stats.RecordPortUsage(remotePort, uint64(totalWritten))
	}
}
