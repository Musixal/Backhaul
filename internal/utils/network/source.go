package network

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
)

// SourceStrategy controls how a source address is selected from a pool.
type SourceStrategy string

const (
	// SourceRoundRobin rotates through the pool in order.
	SourceRoundRobin SourceStrategy = "round_robin"
	// SourceRandom picks a random entry from the pool for every connection.
	SourceRandom SourceStrategy = "random"
)

// sourceEntry is either a single IP or a CIDR block.
type sourceEntry struct {
	ip      net.IP
	network *net.IPNet
}

// SourceSelector selects local source addresses for outbound connections.
//
// It supports:
//   - a single IP:        "203.0.113.10"
//   - a CIDR block:       "2001:db8::/64" (a random address is used per dial)
//   - a pool of entries:  ["203.0.113.10", "203.0.113.11"]
//
// The selector is safe for concurrent use.
type SourceSelector struct {
	entries  []sourceEntry
	strategy SourceStrategy
	next     atomic.Uint64
}

// NewSourceSelector builds a selector from a single send-through value, a
// source-IP pool, and a strategy. At least one address is required.
func NewSourceSelector(sendThrough string, pool []string, strategy string) (*SourceSelector, error) {
	raw := make([]string, 0, len(pool)+1)
	if strings.TrimSpace(sendThrough) != "" {
		raw = append(raw, sendThrough)
	}
	for _, item := range pool {
		if strings.TrimSpace(item) != "" {
			raw = append(raw, item)
		}
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("source selector requires at least one address")
	}

	entries := make([]sourceEntry, 0, len(raw))
	for _, item := range raw {
		entry, err := parseSourceEntry(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	switch SourceStrategy(strategy) {
	case "", SourceRoundRobin:
		strategy = string(SourceRoundRobin)
	case SourceRandom:
	default:
		return nil, fmt.Errorf("invalid source_ip_strategy %q: use %q or %q", strategy, SourceRoundRobin, SourceRandom)
	}

	return &SourceSelector{
		entries:  entries,
		strategy: SourceStrategy(strategy),
	}, nil
}

func parseSourceEntry(value string) (sourceEntry, error) {
	if value == "" {
		return sourceEntry{}, fmt.Errorf("empty source address")
	}

	if strings.Contains(value, "/") {
		ip, network, err := net.ParseCIDR(value)
		if err != nil {
			return sourceEntry{}, fmt.Errorf("invalid source CIDR %q: %w", value, err)
		}
		return sourceEntry{ip: ip, network: network}, nil
	}

	ip := net.ParseIP(value)
	if ip == nil {
		return sourceEntry{}, fmt.Errorf("invalid source IP %q", value)
	}
	return sourceEntry{ip: ip}, nil
}

// Pick returns the next source address. An empty string means "let the OS
// choose", which only happens for a nil selector.
func (s *SourceSelector) Pick() (string, error) {
	if s == nil || len(s.entries) == 0 {
		return "", nil
	}

	var idx int
	switch s.strategy {
	case SourceRandom:
		idx = rand.IntN(len(s.entries))
	default:
		idx = int((s.next.Add(1) - 1) % uint64(len(s.entries)))
	}

	entry := s.entries[idx]
	if entry.network == nil {
		return entry.ip.String(), nil
	}
	return randomIPInNetwork(entry.network).String(), nil
}

// Strategy returns the configured strategy.
func (s *SourceSelector) Strategy() SourceStrategy {
	if s == nil {
		return ""
	}
	return s.strategy
}

// Len returns the number of configured pool entries.
func (s *SourceSelector) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

func randomIPInNetwork(network *net.IPNet) net.IP {
	ones, bits := network.Mask.Size()
	if bits == 0 {
		return network.IP
	}

	if ip4 := network.IP.To4(); ip4 != nil {
		ip := make(net.IP, net.IPv4len)
		copy(ip, ip4)

		hostBits := bits - ones
		if hostBits <= 0 {
			return ip
		}

		// For prefixes with at least two host bits, avoid the network and
		// broadcast addresses. /31 and /32 are treated as usable ranges.
		var value uint32
		if hostBits >= 2 {
			value = rand.Uint32N(1<<uint(hostBits)-2) + 1
		} else {
			value = rand.Uint32N(1 << uint(hostBits))
		}

		base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		base |= value
		ip[0] = byte(base >> 24)
		ip[1] = byte(base >> 16)
		ip[2] = byte(base >> 8)
		ip[3] = byte(base)
		return ip
	}

	ip := make(net.IP, net.IPv6len)
	copy(ip, network.IP)
	hostBits := bits - ones
	for i := 0; i < hostBits; i++ {
		byteIndex := len(ip) - 1 - i/8
		bitIndex := uint(i % 8)
		if rand.Uint32N(2) == 1 {
			ip[byteIndex] |= 1 << bitIndex
		} else {
			ip[byteIndex] &^= 1 << bitIndex
		}
	}
	return ip
}

var defaultSourceSelector atomic.Pointer[SourceSelector]

// SetDefaultSourceSelector installs the process-wide selector used by dialers.
func SetDefaultSourceSelector(selector *SourceSelector) {
	defaultSourceSelector.Store(selector)
}

// DefaultSourceSelector returns the process-wide selector, if configured.
func DefaultSourceSelector() *SourceSelector {
	return defaultSourceSelector.Load()
}

func pickDefaultSource() (string, error) {
	selector := DefaultSourceSelector()
	if selector == nil {
		return "", nil
	}
	return selector.Pick()
}
