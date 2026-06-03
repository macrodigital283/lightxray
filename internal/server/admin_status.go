package server

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"syscall"
	"time"
)

// statusResponse matches the slice of HiddifyServerStatus the pool actually
// reads. Fields we don't have real data for are populated with safe zeros
// — pool tolerates zeros (it just renders them in the dashboard).
type statusResponse struct {
	Stats struct {
		System struct {
			CPUPercent             float64 `json:"cpu_percent"`
			RAMUsed                float64 `json:"ram_used"`
			RAMTotal               float64 `json:"ram_total"`
			DiskUsed               float64 `json:"disk_used"`
			DiskTotal              float64 `json:"disk_total"`
			HiddifyUsed            float64 `json:"hiddify_used"`
			LoadAvg1Min            float64 `json:"load_avg_1min"`
			LoadAvg5Min            float64 `json:"load_avg_5min"`
			LoadAvg15Min           float64 `json:"load_avg_15min"`
			NumCPUs                int     `json:"num_cpus"`
			TotalConnections       int64   `json:"total_connections"`
			TotalUniqueIPs         int64   `json:"total_unique_ips"`
			BytesRecv              int64   `json:"bytes_recv"`
			BytesSent              int64   `json:"bytes_sent"`
			NetSentCumulativeGB    float64 `json:"net_sent_cumulative_GB"`
			NetTotalCumulativeGB   float64 `json:"net_total_cumulative_GB"`
		} `json:"system"`
		Top5 struct {
			CPU    []any `json:"cpu"`
			Memory []any `json:"memory"`
		} `json:"top5"`
	} `json:"stats"`
}

// adminServerStatus — GET /api/v2/admin/server_status/
//
// Pool's health scheduler hits this every cycle. Must return 200 quickly.
// We populate the fields the dashboard reads (cpu/ram/disk/load + a
// user-count derived figure under total_connections) and leave per-process
// breakdowns empty. The pool treats absent top5 lists as fine.
func (d Deps) adminServerStatus(w http.ResponseWriter, r *http.Request) {
	var resp statusResponse
	sys := &resp.Stats.System

	sys.NumCPUs = runtime.NumCPU()
	sys.LoadAvg1Min, sys.LoadAvg5Min, sys.LoadAvg15Min = readLoadAvg()
	sys.RAMUsed, sys.RAMTotal = readMem()
	sys.DiskUsed, sys.DiskTotal = readDiskUsage("/")
	sys.CPUPercent = readCPUPercent()

	// Surface enabled-user-count as "connections" so the dashboard's
	// load-balance heuristic has something to chew on. Cheap to compute.
	ctx, cancel := reqCtx(r)
	defer cancel()
	if n, err := d.store.CountEnabledUsers(ctx); err == nil {
		sys.TotalConnections = n
	} else {
		slog.Warn("server_status count", "err", err)
	}

	resp.Stats.Top5.CPU = []any{}
	resp.Stats.Top5.Memory = []any{}

	writeJSON(w, http.StatusOK, resp)
}

// readLoadAvg parses /proc/loadavg on Linux. Returns zeros if unavailable
// (e.g. macOS local dev) — pool tolerates zeros.
func readLoadAvg() (l1, l5, l15 float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	_, _ = fscan3(string(b), &l1, &l5, &l15)
	return
}

// readMem returns (usedGB, totalGB). Reads /proc/meminfo on Linux.
func readMem() (usedGB, totalGB float64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var totalKB, availKB int64
	parseMeminfo(string(b), &totalKB, &availKB)
	totalGB = float64(totalKB) / (1024 * 1024)
	usedGB = float64(totalKB-availKB) / (1024 * 1024)
	if usedGB < 0 {
		usedGB = 0
	}
	return
}

// readDiskUsage uses statfs to report (usedGB, totalGB) for the given path.
func readDiskUsage(path string) (usedGB, totalGB float64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	used := total - free
	totalGB = float64(total) / (1024 * 1024 * 1024)
	usedGB = float64(used) / (1024 * 1024 * 1024)
	return
}

// readCPUPercent is a coarse one-shot sample. We compute it inline from
// /proc/stat (two reads 100ms apart). Cheap enough for the health-probe
// cadence (every few minutes).
func readCPUPercent() float64 {
	idle1, total1, ok := readProcStat()
	if !ok {
		return 0
	}
	time.Sleep(100 * time.Millisecond)
	idle2, total2, ok := readProcStat()
	if !ok {
		return 0
	}
	dt := total2 - total1
	di := idle2 - idle1
	if dt <= 0 {
		return 0
	}
	pct := (1 - float64(di)/float64(dt)) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func readProcStat() (idle, total int64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseFirstCPULine(string(b))
}
