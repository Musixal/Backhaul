package network

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
)

// EndpointSelector rotates across multiple tunnel server endpoints.
//
// Each entry must be a host:port pair, for example:
//
//	"203.0.113.10:8443"
//	"[2001:db8::10]:8443"
//
// The selector is safe for concurrent use.
type EndpointSelector struct {
	addrs    []string
	strategy SourceStrategy
	next     atomic.Uint64
}

// NewEndpointSelector builds an endpoint selector. Strategy is either
// "round_robin" (default) or "random".
func NewEndpointSelector(addrs []string, strategy string) (*EndpointSelector, error) {
	cleaned := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("invalid remote address %q: %w", addr, err)
		}
		cleaned = append(cleaned, addr)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("endpoint selector requires at least one address")
	}

	switch SourceStrategy(strategy) {
	case "", SourceRoundRobin:
		strategy = string(SourceRoundRobin)
	case SourceRandom:
	default:
		return nil, fmt.Errorf("invalid remote_addr_strategy %q: use %q or %q", strategy, SourceRoundRobin, SourceRandom)
	}

	return &EndpointSelector{
		addrs:    cleaned,
		strategy: SourceStrategy(strategy),
	}, nil
}

// Pick returns the next endpoint.
func (s *EndpointSelector) Pick() string {
	if s == nil || len(s.addrs) == 0 {
		return ""
	}

	var idx int
	switch s.strategy {
	case SourceRandom:
		idx = rand.IntN(len(s.addrs))
	default:
		idx = int((s.next.Add(1) - 1) % uint64(len(s.addrs)))
	}
	return s.addrs[idx]
}

// Len returns the number of configured endpoints.
func (s *EndpointSelector) Len() int {
	if s == nil {
		return 0
	}
	return len(s.addrs)
}

// Strategy returns the configured strategy.
func (s *EndpointSelector) Strategy() SourceStrategy {
	if s == nil {
		return ""
	}
	return s.strategy
}

var defaultEndpointSelector atomic.Pointer[EndpointSelector]

// SetDefaultEndpointSelector installs the process-wide endpoint selector.
func SetDefaultEndpointSelector(selector *EndpointSelector) {
	defaultEndpointSelector.Store(selector)
}

// DefaultEndpointSelector returns the process-wide endpoint selector.
func DefaultEndpointSelector() *EndpointSelector {
	return defaultEndpointSelector.Load()
}

// PickRemoteAddr returns a rotated endpoint when a pool is configured, or
// fallback otherwise. Local service dials must call TcpDialer directly and
// should not use this helper.
func PickRemoteAddr(fallback string) string {
	selector := DefaultEndpointSelector()
	if selector == nil {
		return fallback
	}
	if picked := selector.Pick(); picked != "" {
		return picked
	}
	return fallback
}
