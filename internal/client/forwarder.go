package client

import (
	"strconv"
	"strings"
)

// for both tcp and tcpmux
func (c *Client) forwarderReader(config []string) map[int]string {
	forwarder := make(map[int]string)
	for _, portMapping := range config {
		parts := strings.Split(portMapping, "=")
		if len(parts) != 2 {
			c.logger.Fatalf("invalid port mapping: %s", portMapping)
			continue
		}

		localPortStr := strings.TrimSpace(parts[0])

		localPort, err := strconv.Atoi(localPortStr)
		if err != nil {
			c.logger.Fatalf("invalid local port in mapping: %s", localPortStr)
			continue
		}
		remoteAddress := strings.TrimSpace(parts[1])

		forwarder[localPort] = remoteAddress
	}
	return forwarder
}
