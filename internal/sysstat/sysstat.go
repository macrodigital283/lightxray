// Package sysstat reads coarse host metrics from /proc + statfs. It's the
// single source of truth for both the admin server_status API and the
// dashboard's stats bar. Every signal is Linux-only and degrades to zero on
// other platforms (local macOS dev) — all callers tolerate zeros.
package sysstat

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Snapshot is a point-in-time host metrics sample.
type Snapshot struct {
	CPUPercent  float64 // 0-100, aggregate across all cores
	NumCPUs     int
	RAMUsedGB   float64
	RAMTotalGB  float64
	DiskUsedGB  float64
	DiskTotalGB float64
	LoadAvg1    float64
	LoadAvg5    float64
	LoadAvg15   float64
	NetDownMbps float64 // current receive rate (megabits/sec)
	NetUpMbps   float64 // current transmit rate (megabits/sec)
	NetRecvGB   float64 // cumulative received since boot
	NetSentGB   float64 // cumulative sent since boot
}

// sampleWindow is how long Sample() pauses between the two reads it needs to
// turn CPU jiffies + net byte counters into rates. Short enough to keep a
// page load / health probe snappy, long enough to be non-noisy.
const sampleWindow = 150 * time.Millisecond

// Sample collects every metric. It blocks for sampleWindow because CPU% and
// network rate are each derived from two reads taken that far apart. Don't
// call it in a tight loop.
func Sample(diskPath string) Snapshot {
	var s Snapshot
	s.NumCPUs = runtime.NumCPU()
	s.LoadAvg1, s.LoadAvg5, s.LoadAvg15 = LoadAvg()
	s.RAMUsedGB, s.RAMTotalGB = Mem()
	s.DiskUsedGB, s.DiskTotalGB = Disk(diskPath)

	rxTotal, txTotal, _ := readNetDev()
	s.NetRecvGB = float64(rxTotal) / (1024 * 1024 * 1024)
	s.NetSentGB = float64(txTotal) / (1024 * 1024 * 1024)

	// CPU + net rate share one sample window: read both counters, sleep
	// once, read both again.
	idle1, total1, cpuOK := readProcStat()
	rx1, tx1, netOK := readNetDev()
	t0 := time.Now()
	time.Sleep(sampleWindow)
	elapsed := time.Since(t0).Seconds()

	if cpuOK {
		if idle2, total2, ok := readProcStat(); ok {
			if dt := total2 - total1; dt > 0 {
				s.CPUPercent = clamp((1-float64(idle2-idle1)/float64(dt))*100, 0, 100)
			}
		}
	}
	if netOK && elapsed > 0 {
		if rx2, tx2, ok := readNetDev(); ok {
			s.NetDownMbps = nonneg(float64(rx2-rx1) * 8 / 1e6 / elapsed)
			s.NetUpMbps = nonneg(float64(tx2-tx1) * 8 / 1e6 / elapsed)
		}
	}
	return s
}

// LoadAvg parses /proc/loadavg. Zeros if unavailable.
func LoadAvg() (l1, l5, l15 float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	return
}

// Mem returns (usedGB, totalGB) from /proc/meminfo (MemTotal - MemAvailable).
func Mem() (usedGB, totalGB float64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var totalKB, availKB int64
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKB = parseKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availKB = parseKB(line)
		}
	}
	totalGB = float64(totalKB) / (1024 * 1024)
	usedGB = float64(totalKB-availKB) / (1024 * 1024)
	if usedGB < 0 {
		usedGB = 0
	}
	return
}

// Disk reports (usedGB, totalGB) for the filesystem holding path.
func Disk(path string) (usedGB, totalGB float64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	totalGB = float64(total) / (1024 * 1024 * 1024)
	usedGB = float64(total-free) / (1024 * 1024 * 1024)
	return
}

func parseKB(line string) int64 {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseInt(f[1], 10, 64)
	return n
}

// readProcStat returns aggregate (idle, total) jiffies off the first "cpu"
// line of /proc/stat. idle = idle + iowait.
func readProcStat() (idle, total int64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line := string(b)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
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

// readNetDev sums rx + tx bytes across all non-loopback interfaces from
// /proc/net/dev. Per line: "iface: rxbytes rxpkts ... txbytes txpkts ...".
func readNetDev() (rx, tx int64, ok bool) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:i])
		if iface == "" || iface == "lo" {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseInt(f[0], 10, 64) // receive bytes
		t, _ := strconv.ParseInt(f[8], 10, 64) // transmit bytes
		rx += r
		tx += t
	}
	return rx, tx, true
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func nonneg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
