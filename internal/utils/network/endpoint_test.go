package network

import "testing"

func TestEndpointSelectorRoundRobin(t *testing.T) {
	selector, err := NewEndpointSelector([]string{"203.0.113.10:8443", "[2001:db8::10]:8443"}, "")
	if err != nil {
		t.Fatalf("NewEndpointSelector() error = %v", err)
	}

	want := []string{"203.0.113.10:8443", "[2001:db8::10]:8443", "203.0.113.10:8443"}
	for i, expected := range want {
		if got := selector.Pick(); got != expected {
			t.Fatalf("Pick() #%d = %q, want %q", i, got, expected)
		}
	}
}

func TestEndpointSelectorRandom(t *testing.T) {
	selector, err := NewEndpointSelector([]string{"203.0.113.10:8443", "203.0.113.11:8443"}, string(SourceRandom))
	if err != nil {
		t.Fatalf("NewEndpointSelector() error = %v", err)
	}

	allowed := map[string]bool{
		"203.0.113.10:8443": true,
		"203.0.113.11:8443": true,
	}
	for i := 0; i < 20; i++ {
		if got := selector.Pick(); !allowed[got] {
			t.Fatalf("Pick() = %q, not in pool", got)
		}
	}
}

func TestEndpointSelectorRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name     string
		addrs    []string
		strategy string
	}{
		{name: "empty"},
		{name: "missing port", addrs: []string{"203.0.113.10"}},
		{name: "bad strategy", addrs: []string{"203.0.113.10:8443"}, strategy: "fastest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEndpointSelector(tc.addrs, tc.strategy); err == nil {
				t.Fatal("NewEndpointSelector() expected error, got nil")
			}
		})
	}
}

func TestPickRemoteAddr(t *testing.T) {
	SetDefaultEndpointSelector(nil)
	if got := PickRemoteAddr("fallback:8443"); got != "fallback:8443" {
		t.Fatalf("PickRemoteAddr() = %q, want fallback", got)
	}

	selector, err := NewEndpointSelector([]string{"203.0.113.10:8443", "203.0.113.11:8443"}, "")
	if err != nil {
		t.Fatalf("NewEndpointSelector() error = %v", err)
	}
	SetDefaultEndpointSelector(selector)
	defer SetDefaultEndpointSelector(nil)

	if got := PickRemoteAddr("fallback:8443"); got != "203.0.113.10:8443" {
		t.Fatalf("PickRemoteAddr() = %q, want first pool address", got)
	}
	if got := PickRemoteAddr("fallback:8443"); got != "203.0.113.11:8443" {
		t.Fatalf("PickRemoteAddr() = %q, want second pool address", got)
	}
}
