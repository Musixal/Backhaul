package metrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/musix/backhaul/config"
	"github.com/musix/backhaul/internal/stats"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// PrometheusCollector implements Collector
type PrometheusCollector struct {
	ctx       context.Context
	transport config.TransportType
	labels    prometheus.Labels

	reg *prometheus.Registry
}

func init() {
	RegisterCollector("prometheus", NewPrometheusCollector)
}

func NewPrometheusCollector(ctx context.Context, log *logrus.Logger, cfg config.Config) Collector {
	var transport config.TransportType
	var labels prometheus.Labels
	if cfg.IsServerConfig() {
		transport = cfg.Server.Transport
		labels = prometheus.Labels{
			"transport": fmt.Sprintf("%s", transport),
			"side":      "server",
		}
	} else {
		transport = cfg.Client.Transport
		labels = prometheus.Labels{
			"transport": fmt.Sprintf("%s", transport),
			"side":      "client",
		}
	}

	instance := &PrometheusCollector{
		ctx:       ctx,
		transport: transport,
		labels:    labels,
		reg:       prometheus.NewRegistry(),
	}

	// default collectors
	instance.reg.MustRegister(collectors.NewGoCollector())
	instance.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// backhaul tunnel status
	instance.customCollectors()

	return instance
}

func (p *PrometheusCollector) Bind(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
}

func (p *PrometheusCollector) customCollectors() {
	statusGauge := promauto.With(p.reg).NewGauge(
		prometheus.GaugeOpts{
			Namespace:   "backhaul",
			Subsystem:   "tunnel",
			Name:        "status",
			Help:        "is Backhaul tunnel up",
			ConstLabels: p.labels,
		},
	)

	usageGauge := promauto.With(p.reg).NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   "backhaul",
			Subsystem:   "tunnel",
			Name:        "usage",
			Help:        "backhaul usage per port",
			ConstLabels: p.labels,
		},
		[]string{"port"},
	)

	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				var status float64
				if stats.IsUp() {
					status = 1
				}
				statusGauge.Set(status)

				for i, u := range stats.GetPortUsages() {
					usageGauge.With(prometheus.Labels{"port": fmt.Sprintf("%d", i)}).Set(float64(u))
				}
			case <-p.ctx.Done():
				return
			}
		}
	}()
}
