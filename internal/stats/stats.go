package stats

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/musix/backhaul/config"
	"github.com/sirupsen/logrus"
)

type TunnelSide string

var (
	ServerSide TunnelSide = "server"
	ClientSide TunnelSide = "client"
)

type statsStorage struct {
	ctx context.Context

	side      TunnelSide
	transport config.TransportType

	location string
	logger   *logrus.Logger

	stats *stats
	up    bool
}

type stats struct {
	mu         sync.Mutex
	PortUsages PortUsages `json:"port_usages"`
	TotalUsage uint64     `json:"total_usage"`
}

type PortUsages map[int]uint64

var instance *statsStorage

func InitClientStats(ctx context.Context, logger *logrus.Logger, cfg config.ClientConfig) {
	instance = &statsStorage{
		ctx:       ctx,
		side:      ClientSide,
		transport: cfg.Transport,
		location:  cfg.SnifferLog,
		logger:    logger,
		stats:     nil,
	}

	if cfg.Sniffer {
		instance.init()
	}
}

func InitServerStats(ctx context.Context, logger *logrus.Logger, cfg config.ServerConfig) {
	instance = &statsStorage{
		ctx:       ctx,
		side:      ServerSide,
		transport: cfg.Transport,
		location:  cfg.SnifferLog,
		logger:    logger,
		stats:     nil,
	}

	if cfg.Sniffer {
		instance.init()
	}
}

func (s *statsStorage) init() {
	s.load()

	go func() {
		ticker := time.NewTicker(15 * time.Second) // every 15 seconds
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				go s.save()
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

func (s *statsStorage) save() {
	if s.stats == nil {
		return
	}

	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	data, err := json.MarshalIndent(s.stats, "", "  ")
	if err != nil {
		s.logger.Errorf("Failed to marshal stats: %v", err)
		return
	}

	if err := os.WriteFile(s.location, data, 0644); err != nil {
		s.logger.Errorf("Failed to save stats to file: %v", err)
	}
}

func (s *statsStorage) load() {
	data, err := os.ReadFile(s.location)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Errorf("Failed to read stats file: %v", err)
		}

		s.stats = &stats{
			PortUsages: make(PortUsages),
			TotalUsage: 0,
		}
		return
	}

	var loadedStats stats
	if err := json.Unmarshal(data, &loadedStats); err != nil {
		s.logger.Errorf("Failed to unmarshal stats: %v", err)
		s.stats = &stats{
			PortUsages: make(PortUsages),
			TotalUsage: 0,
		}
		return
	}

	s.stats = &loadedStats
	if s.stats.PortUsages == nil {
		s.stats.PortUsages = make(PortUsages)
	}
}

func RecordPortUsage(port int, usage uint64) {
	if instance == nil {
		panic("attempt to record usage before initiating stat storage")
	}

	if instance.stats == nil {
		return
	}

	instance.stats.mu.Lock()
	defer instance.stats.mu.Unlock()

	existing, exists := instance.stats.PortUsages[port]
	if exists {
		usage = usage + existing
	}

	instance.stats.PortUsages[port] = usage
	instance.stats.TotalUsage += usage
}

func GetPortUsages() PortUsages {
	if instance == nil {
		return map[int]uint64{}
	}

	if instance.stats == nil {
		return map[int]uint64{}
	}

	return instance.stats.PortUsages
}

func GetTotalUsage() uint64 {
	if instance == nil {
		return 0
	}

	if instance.stats == nil {
		return 0
	}

	return instance.stats.TotalUsage
}

func SetDown() {
	if instance == nil {
		return
	}

	instance.up = false
}

func SetUp() {
	if instance == nil {
		return
	}

	instance.up = true
}

func IsUp() bool {
	if instance == nil {
		return false
	}

	return instance.up
}
