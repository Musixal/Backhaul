package utils

import (
	"strconv"
	"strings"
)

func ParsePortRange(s string) []int {
	s = strings.Trim(s, "[]")
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return nil
		}
		start, err1 := strconv.Atoi(parts[0])
		end, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || start > end {
			return nil
		}
		var ports []int
		for i := start; i <= end; i++ {
			ports = append(ports, i)
		}
		return ports
	}
	port, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return []int{port}
}

func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
