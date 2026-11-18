package web

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	usage := &Usage{}
	recorder := httptest.NewRecorder()
	usage.healthHandler(recorder, httptest.NewRequest("GET", "/healthz", nil))

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if strings.TrimSpace(recorder.Body.String()) != "ok" {
		t.Fatalf("body = %q, want ok", recorder.Body.String())
	}
}

func TestMetricsHandler(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "backhaul.json")
	if err := os.WriteFile(logPath, []byte(`[{"Port":9007,"Usage":42}]`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	status := "Connected (TCP)"
	usage := &Usage{
		snifferLog:   logPath,
		tunnelStatus: &status,
	}
	usage.dataStore.Store(9008, PortUsage{Port: 9008, Usage: 8})

	recorder := httptest.NewRecorder()
	usage.metricsHandler(recorder, httptest.NewRequest("GET", "/metrics", nil))

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	for _, want := range []string{
		"backhaul_up 1",
		`backhaul_tunnel_status{status="Connected (TCP)"} 1`,
		`backhaul_port_traffic_bytes{port="9007"} 42`,
		`backhaul_port_traffic_bytes{port="9008"} 8`,
		"backhaul_traffic_bytes_total 50",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}
