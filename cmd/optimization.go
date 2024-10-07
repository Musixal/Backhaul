package cmd

import (
	"os/exec"
	"runtime"
	"syscall"
)

// applyTCPTuning applies temporary TCP optimizations for Linux to handle massive connections
func ApplyTCPTuning() {
	if runtime.GOOS == "linux" {
		logger.Info("Applying TCP optimizations for Linux...")

		// Commands for optimizing TCP parameters
		commands := [][]string{
			{"sysctl", "-w", "net.ipv4.ip_local_port_range=1024 65535"}, // Increase ephemeral ports
			{"sysctl", "-w", "net.ipv4.tcp_tw_reuse=1"},                 // Reuse TIME_WAIT sockets
			{"sysctl", "-w", "net.ipv4.tcp_fin_timeout=15"},             // Reduce TCP FIN timeout
			{"sysctl", "-w", "net.core.somaxconn=4096"},                 // Increase max queue length of incoming connections
			{"sysctl", "-w", "net.ipv4.tcp_max_syn_backlog=8192"},       // Increase SYN request backlog
			{"sysctl", "-w", "net.ipv4.tcp_window_scaling=1"},           // Enable TCP window scaling
			{"sysctl", "-w", "net.ipv4.tcp_fastopen=3"},                 // Enable TCP Fast Open
		}

		// Execute the sysctl commands
		for _, cmd := range commands {
			err := exec.Command(cmd[0], cmd[1:]...).Run()
			if err != nil {
				logger.Errorf("Failed to apply TCP tuning: %v", err)
			} else {
				logger.Debugf("Successfully applied: %s", cmd)
			}
		}

		// Set file descriptor limit programmatically
		var rLimit syscall.Rlimit
		err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
		if err != nil {
			logger.Errorf("Error getting Rlimit: %v", err)
		} else {
			logger.Debugf("Current file descriptor limit: %d", rLimit.Cur)

			// Set the maximum and current file descriptor limits to 1048576
			rLimit.Max = 1048576
			rLimit.Cur = 1048576
			err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
			if err != nil {
				logger.Errorf("Error setting Rlimit: %v", err)
			} else {
				logger.Debugf("Successfully set file descriptor limit to: %d", rLimit.Cur)
			}
		}
	} else {
		logger.Info("Non-Linux system detected, skipping TCP optimizations.")
	}
}
