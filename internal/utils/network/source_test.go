package network

import (
	"net"
	"testing"
)

func TestSourceSelectorSingleIP(t *testing.T) {
	selector, err := NewSourceSelector("203.0.113.10", nil, "")
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}

	for i := 0; i < 3; i++ {
		got, err := selector.Pick()
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if got != "203.0.113.10" {
			t.Fatalf("Pick() = %q, want %q", got, "203.0.113.10")
		}
	}
}

func TestSourceSelectorRoundRobin(t *testing.T) {
	selector, err := NewSourceSelector("", []string{"203.0.113.10", "203.0.113.11"}, SourceRoundRobin.String())
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}

	want := []string{"203.0.113.10", "203.0.113.11", "203.0.113.10", "203.0.113.11"}
	for i, expected := range want {
		got, err := selector.Pick()
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if got != expected {
			t.Fatalf("Pick() #%d = %q, want %q", i, got, expected)
		}
	}
}

func TestSourceSelectorRandomStaysInPool(t *testing.T) {
	selector, err := NewSourceSelector("", []string{"203.0.113.10", "203.0.113.11"}, SourceRandom.String())
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}

	allowed := map[string]bool{"203.0.113.10": true, "203.0.113.11": true}
	for i := 0; i < 20; i++ {
		got, err := selector.Pick()
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if !allowed[got] {
			t.Fatalf("Pick() = %q, not in pool", got)
		}
	}
}

func TestSourceSelectorIPv4CIDR(t *testing.T) {
	selector, err := NewSourceSelector("203.0.113.0/24", nil, SourceRandom.String())
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}

	_, network, _ := net.ParseCIDR("203.0.113.0/24")
	for i := 0; i < 100; i++ {
		got, err := selector.Pick()
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		ip := net.ParseIP(got)
		if ip == nil {
			t.Fatalf("Pick() returned invalid IP %q", got)
		}
		if !network.Contains(ip) {
			t.Fatalf("Pick() = %q, outside %s", got, network)
		}
		if got == "203.0.113.0" || got == "203.0.113.255" {
			t.Fatalf("Pick() returned reserved address %q", got)
		}
	}
}

func TestSourceSelectorIPv6CIDR(t *testing.T) {
	selector, err := NewSourceSelector("2001:db8:1:2::/64", nil, SourceRandom.String())
	if err != nil {
		t.Fatalf("NewSourceSelector() error = %v", err)
	}

	_, network, _ := net.ParseCIDR("2001:db8:1:2::/64")
	for i := 0; i < 100; i++ {
		got, err := selector.Pick()
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		ip := net.ParseIP(got)
		if ip == nil {
			t.Fatalf("Pick() returned invalid IP %q", got)
		}
		if !network.Contains(ip) {
			t.Fatalf("Pick() = %q, outside %s", got, network)
		}
	}
}

func TestSourceSelectorRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name        string
		sendThrough string
		pool        []string
		strategy    string
	}{
		{name: "no addresses"},
		{name: "bad IP", sendThrough: "not-an-ip"},
		{name: "bad CIDR", sendThrough: "203.0.113.0/99"},
		{name: "bad strategy", sendThrough: "203.0.113.10", strategy: "fastest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSourceSelector(tc.sendThrough, tc.pool, tc.strategy); err == nil {
				t.Fatal("NewSourceSelector() expected error, got nil")
			}
		})
	}
}

func (s SourceStrategy) String() string {
	return string(s)
}
