package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/musix/backhaul/config"
	"github.com/musix/backhaul/internal/stats"
	"github.com/sirupsen/logrus"
)

type CollectorFactory func(ctx context.Context, log *logrus.Logger, cfg config.Config) Collector
type Collector interface {
	Bind(mux *http.ServeMux)
}

type Handler struct {
	ctx context.Context
	srv *http.Server
	log *logrus.Logger

	cfg config.Config

	collectors map[string]Collector
}

var factories = make(map[string]CollectorFactory)

func NewMetricsHandler(ctx context.Context, log *logrus.Logger, cfg config.Config) *Handler {
	var includedCollectors []string
	if cfg.IsServerConfig() {
		stats.InitServerStats(ctx, log, cfg.Server)
		includedCollectors = cfg.Server.MetricCollectors
	} else {
		stats.InitClientStats(ctx, log, cfg.Client)
		includedCollectors = cfg.Client.MetricCollectors
	}

	collectors := map[string]Collector{}

	for _, name := range includedCollectors {
		factory, ok := factories[name]
		if !ok {
			log.Errorf("unknown metrics handler: %s", name)
			continue
		}

		collectors[name] = factory(ctx, log, cfg)
	}

	return &Handler{
		ctx:        ctx,
		log:        log,
		cfg:        cfg,
		collectors: collectors,
	}
}

func (m *Handler) Monitor() {
	var port int
	if m.cfg.IsServerConfig() {
		port = m.cfg.Server.WebPort
	} else {
		port = m.cfg.Client.WebPort
	}

	if port <= 0 {
		return
	}

	srv := http.NewServeMux()
	m.bindCollectors(srv)

	bindAddr := fmt.Sprintf(":%d", port)
	m.srv = &http.Server{
		Addr:    bindAddr,
		Handler: srv,
	}

	go func() {
		<-m.ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := m.srv.Shutdown(shutdownCtx); err != nil {
			m.log.Errorf("sniffer server shutdown error: %v", err)
		}
	}()

	// Start the server
	m.log.Info("collector service listening on port: ", bindAddr)
	if err := m.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		m.log.Errorf("collector server error: %v", err)
	}
}

func (m *Handler) bindCollectors(srv *http.ServeMux) {
	for _, collectorImpl := range m.collectors {
		collectorImpl.Bind(srv)
	}
}

func RegisterCollector(name string, factory CollectorFactory) {
	factories[name] = factory
}
