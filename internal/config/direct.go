package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const MaxDirectConnections = 32

var byteSizePattern = regexp.MustCompile(`(?i)^\s*(\d+(?:\.\d+)?)\s*(kb|kib|mb|mib|gb|gib)\s*$`)

// DirectMinChunkBytes parses Direct.MinChunkSize using binary units. Both MB
// and MiB mean 2^20 bytes, matching the rest of tork's size handling.
func (c Config) DirectMinChunkBytes() (int64, error) {
	if c.Direct.MaxConnections < 0 || c.Direct.MaxConnections > MaxDirectConnections {
		return 0, fmt.Errorf("direct.max_connections must be between 0 and %d", MaxDirectConnections)
	}
	m := byteSizePattern.FindStringSubmatch(c.Direct.MinChunkSize)
	if m == nil {
		return 0, errorsForDirectChunkSize()
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0, errorsForDirectChunkSize()
	}
	var multiplier float64
	switch strings.ToLower(m[2]) {
	case "kb", "kib":
		multiplier = 1 << 10
	case "mb", "mib":
		multiplier = 1 << 20
	case "gb", "gib":
		multiplier = 1 << 30
	}
	if n > math.MaxInt64/multiplier {
		return 0, errorsForDirectChunkSize()
	}
	return int64(n * multiplier), nil
}

func errorsForDirectChunkSize() error {
	return fmt.Errorf("direct.min_chunk_size must be a positive size such as 10MB or 64MiB")
}

// Debrid connection limits (roadmap D10, J7.5).
const (
	DefaultDebridConnections      = 4
	MaxDebridConnections          = 16
	DefaultDebridTotalConnections = 8
)

// DebridConnectionChoices are the values Settings offers for
// debrid.connections.
var DebridConnectionChoices = []int{1, 2, 4, 8, 16}

// ValidateDebridConnections checks debrid.connections and
// debrid.max_total_connections, the same way DirectMinChunkBytes checks
// direct.max_connections.
func (c Config) ValidateDebridConnections() error {
	if n := c.Debrid.Connections; n < 1 || n > MaxDebridConnections {
		return fmt.Errorf("debrid.connections must be between 1 and %d (got %d)", MaxDebridConnections, n)
	}
	if n := c.Debrid.MaxTotalConnections; n < 1 || n > MaxDirectConnections {
		return fmt.Errorf("debrid.max_total_connections must be between 1 and %d (got %d)", MaxDirectConnections, n)
	}
	return nil
}

// WriteDebridConnections saves debrid.connections with the comment-preserving
// writer. The caller updates its in-memory config after success.
func (c *Config) WriteDebridConnections(n int) error {
	if n < 1 || n > MaxDebridConnections {
		return fmt.Errorf("connections must be between 1 and %d", MaxDebridConnections)
	}
	return c.writeScalar([]string{"debrid", "connections"}, "!!int", strconv.Itoa(n))
}
