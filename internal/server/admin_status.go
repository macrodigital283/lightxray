package server

import (
	"log/slog"
	"net/http"

	"github.com/macrodigital283/lightxray/internal/sysstat"
)

// statusResponse matches the slice of HiddifyServerStatus the pool actually
// reads. Fields we don't have real data for are populated with safe zeros
// — pool tolerates zeros (it just renders them in the dashboard).
type statusResponse struct {
	Stats struct {
		System struct {
			CPUPercent           float64 `json:"cpu_percent"`
			RAMUsed              float64 `json:"ram_used"`
			RAMTotal             float64 `json:"ram_total"`
			DiskUsed             float64 `json:"disk_used"`
			DiskTotal            float64 `json:"disk_total"`
			HiddifyUsed          float64 `json:"hiddify_used"`
			LoadAvg1Min          float64 `json:"load_avg_1min"`
			LoadAvg5Min          float64 `json:"load_avg_5min"`
			LoadAvg15Min         float64 `json:"load_avg_15min"`
			NumCPUs              int     `json:"num_cpus"`
			TotalConnections     int64   `json:"total_connections"`
			TotalUniqueIPs       int64   `json:"total_unique_ips"`
			BytesRecv            int64   `json:"bytes_recv"`
			BytesSent            int64   `json:"bytes_sent"`
			NetSentCumulativeGB  float64 `json:"net_sent_cumulative_GB"`
			NetTotalCumulativeGB float64 `json:"net_total_cumulative_GB"`
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
// We populate the fields the dashboard reads (cpu/ram/disk/load/net + a
// user-count derived figure under total_connections) and leave per-process
// breakdowns empty. The pool treats absent top5 lists as fine.
func (d Deps) adminServerStatus(w http.ResponseWriter, r *http.Request) {
	var resp statusResponse
	sys := &resp.Stats.System

	snap := sysstat.Sample("/")
	sys.NumCPUs = snap.NumCPUs
	sys.LoadAvg1Min = snap.LoadAvg1
	sys.LoadAvg5Min = snap.LoadAvg5
	sys.LoadAvg15Min = snap.LoadAvg15
	sys.RAMUsed = snap.RAMUsedGB
	sys.RAMTotal = snap.RAMTotalGB
	sys.DiskUsed = snap.DiskUsedGB
	sys.DiskTotal = snap.DiskTotalGB
	sys.CPUPercent = snap.CPUPercent
	sys.NetSentCumulativeGB = snap.NetSentGB
	sys.NetTotalCumulativeGB = snap.NetRecvGB + snap.NetSentGB

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
