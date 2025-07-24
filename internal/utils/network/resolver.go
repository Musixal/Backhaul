package network

import (
	"fmt"
	"strconv"
	"strings"
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
