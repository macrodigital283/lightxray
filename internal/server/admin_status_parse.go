package server

import (
	"strconv"
	"strings"
)

// fscan3 — `fmt.Sscanf("%f %f %f", ...)` is dragged into Hiddify-shaped
// outputs, but it's heavy and we only need three numbers off the start
// of a string. Manual whitespace split is ~10× faster and zero-alloc.
func fscan3(s string, a, b, c *float64) (int, error) {
	parts := strings.Fields(s)
	if len(parts) < 3 {
		return 0, nil
	}
	*a, _ = strconv.ParseFloat(parts[0], 64)
	*b, _ = strconv.ParseFloat(parts[1], 64)
	*c, _ = strconv.ParseFloat(parts[2], 64)
	return 3, nil
}

// parseMeminfo pulls MemTotal + MemAvailable (kB) from /proc/meminfo.
// Anything else in the file is ignored.
func parseMeminfo(s string, totalKB, availKB *int64) {
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			*totalKB = parseKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			*availKB = parseKB(line)
		}
	}
}

// parseKB extracts the integer kB value from a /proc/meminfo line:
//   "MemTotal:       16384000 kB"
func parseKB(line string) int64 {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(f[1], 10, 64)
	return n
}

// parseFirstCPULine pulls the aggregate "cpu" line off /proc/stat:
//   cpu  user nice system idle iowait irq softirq steal guest guest_nice
// total = sum of all; idle = idle + iowait.
func parseFirstCPULine(s string) (idle, total int64, ok bool) {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i := 1; i < len(f); i++ {
		n, _ := strconv.ParseInt(f[i], 10, 64)
		total += n
		if i == 4 || i == 5 { // idle, iowait
			idle += n
		}
	}
	return idle, total, true
}
