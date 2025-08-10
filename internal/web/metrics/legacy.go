package metrics

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/musix/backhaul/config"
	"github.com/musix/backhaul/internal/stats"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/sirupsen/logrus"
)

type SystemStats struct {
	TunnelStatus    string `json:"tunnelStatus"`
	CPUUsage        string `json:"cpuUsage"`
	RAMUsage        string `json:"ramUsage"`
	DiskUsage       string `json:"diskUsage"`
	SwapUsage       string `json:"swapUsage"`
	NetworkTraffic  string `json:"networkTraffic"`
	UploadSpeed     string `json:"uploadSpeed"`
	DownloadSpeed   string `json:"downloadSpeed"`
	BackhaulTraffic string `json:"backhaulTraffic"`
	Sniffer         string `json:"sniffer"`
	AllConnections  string `json:"allConnections"`
}

func init() {
	RegisterCollector("default", func(ctx context.Context, log *logrus.Logger, cfg config.Config) Collector {
		var transport config.TransportType
		var sniffer bool
		if cfg.IsServerConfig() {
			transport = cfg.Server.Transport
			sniffer = cfg.Server.Sniffer
		} else {
			transport = cfg.Client.Transport
			sniffer = cfg.Client.Sniffer
		}

		return &LegacyCollector{
			ctx:              ctx,
			logger:           log,
			transport:        transport,
			portUsageEnabled: sniffer,
		}
	})
}

// LegacyCollector implements Handler
type LegacyCollector struct {
	ctx              context.Context
	logger           *logrus.Logger
	transport        config.TransportType
	status           *string
	portUsageEnabled bool
}

func (c *LegacyCollector) Bind(srv *http.ServeMux) {
	srv.HandleFunc("/", c.handleIndex) // handle index
	srv.HandleFunc("/stats", c.statsHandler)
	if c.portUsageEnabled {
		srv.HandleFunc("/data", c.handleData) // New route for JSON data
	}
}

//go:embed index.html
var indexHTML embed.FS

func (c *LegacyCollector) handleIndex(w http.ResponseWriter, r *http.Request) {
	usageData := stats.GetPortUsages()
	readableData := c.usageDataWithReadableUsage(usageData)

	tmpl, err := template.ParseFS(indexHTML, "index.html")
	if err != nil {
		c.logger.Errorf("error parsing template: %v", err)
		return
	}

	err = tmpl.Execute(w, readableData)
	if err != nil {
		c.logger.Errorf("error executing template: %v", err)
	}
}

func (c *LegacyCollector) handleData(w http.ResponseWriter, r *http.Request) {
	usageData := stats.GetPortUsages()
	readableData := c.usageDataWithReadableUsage(usageData)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(readableData); err != nil {
		c.logger.Errorf("error encoding JSON response: %v", err)
	}
}

func (c *LegacyCollector) statsHandler(w http.ResponseWriter, r *http.Request) {
	systemStats, err := c.getSystemStats()
	if err != nil {
		c.logger.Error("Error fetching system stats:", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(systemStats); err != nil {
		c.logger.Error("Error encoding JSON:", err)
	}
}

// converts the byte usage to a human-readable format
func (c *LegacyCollector) usageDataWithReadableUsage(usageData stats.PortUsages) []struct {
	Port          int
	ReadableUsage string
} {
	var result []struct {
		Port          int
		ReadableUsage string
	}

	for port, portUsage := range usageData {
		result = append(result, struct {
			Port          int
			ReadableUsage string
		}{
			Port:          port,
			ReadableUsage: c.convertBytesToReadable(portUsage),
		})
	}

	return result
}

// ConvertBytesToReadable converts bytes into a human-readable format (KB, MB, GB)
func (c *LegacyCollector) convertBytesToReadable(bytes uint64) string {
	const (
		KB = 1 << (10 * 1) // 1024 bytes
		MB = 1 << (10 * 2) // 1024 KB
		GB = 1 << (10 * 3) // 1024 MB
		TB = 1 << (10 * 4) // 1024 TB
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes) // Bytes
	}
}

func (c *LegacyCollector) getSystemStats() (*SystemStats, error) {

	// Get initial network stats
	initialStats, err := c.getNetworkStats()
	if err != nil {
		return nil, err
	}

	// Wait for 1 second
	time.Sleep(1 * time.Second)

	// Get updated network stats
	finalStats, err := c.getNetworkStats()
	if err != nil {
		return nil, err
	}

	// Get CPU usage
	cpuPercent, err := cpu.Percent(0, false)
	if err != nil {
		return nil, err
	}

	// Get RAM usage
	memStats, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	// Get Disk usage
	diskStats, err := disk.Usage("/")
	if err != nil {
		return nil, err
	}

	// Get Swap usage
	swapStats, err := mem.SwapMemory()
	if err != nil {
		return nil, err
	}

	// Get Network traffic
	netStats, err := net.IOCounters(false)
	if err != nil {
		return nil, err
	}

	// Get all active network connections (TCP, UDP, etc.)
	connections, err := net.Connections("all")
	if err != nil {
		return nil, err
	}

	// Calculate upload and download speeds
	uploadSpeed := float64(finalStats.BytesSent - initialStats.BytesSent)
	downloadSpeed := float64(finalStats.BytesRecv - initialStats.BytesRecv)

	var tunnelStatus string
	if stats.IsUp() {
		tunnelStatus = fmt.Sprintf("Connected (%s)", c.transport)
	} else {

		tunnelStatus = fmt.Sprintf("Disconnected (%s)", c.transport)
	}

	systemStats := &SystemStats{
		TunnelStatus:    tunnelStatus,
		CPUUsage:        c.formatFloat(cpuPercent[0]),
		RAMUsage:        c.convertBytesToReadable(memStats.Used),
		DiskUsage:       c.convertBytesToReadable(diskStats.Used),
		SwapUsage:       c.convertBytesToReadable(swapStats.Used),
		NetworkTraffic:  c.convertBytesToReadable(netStats[0].BytesSent + netStats[0].BytesRecv),
		DownloadSpeed:   c.formatSpeed(downloadSpeed),
		UploadSpeed:     c.formatSpeed(uploadSpeed),
		BackhaulTraffic: c.convertBytesToReadable(stats.GetTotalUsage()),
		Sniffer:         map[bool]string{true: "Running", false: "Not running"}[c.portUsageEnabled],
		AllConnections:  fmt.Sprintf("%d", len(connections)),
	}

	return systemStats, nil
}

func (c *LegacyCollector) formatSpeed(bytesPerSec float64) string {
	if bytesPerSec >= 1e9 {
		return fmt.Sprintf("%.2f GB/s", bytesPerSec/1e9)
	} else if bytesPerSec >= 1e6 {
		return fmt.Sprintf("%.2f MB/s", bytesPerSec/1e6)
	} else if bytesPerSec >= 1e3 {
		return fmt.Sprintf("%.2f KB/s", bytesPerSec/1e3)
	}
	return fmt.Sprintf("%.2f B/s", bytesPerSec)
}

func (c *LegacyCollector) formatFloat(value float64) string {
	return fmt.Sprintf("%.2f%%", value)
}

func (c *LegacyCollector) getNetworkStats() (*net.IOCountersStat, error) {
	ioCounters, err := net.IOCounters(false)
	if err != nil {
		return nil, err
	}
	if len(ioCounters) == 0 {
		return nil, fmt.Errorf("no network IO counters found")
	}
	return &ioCounters[0], nil
}
