package proxy

import (
	"encoding/json"
	"kiro-go/metrics"
	"net/http"
	"strconv"
	"strings"
)

// apiGetToolStats handles GET /admin/api/tool-stats?kind=web_search&hours=24
func (h *Handler) apiGetToolStats(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	hoursStr := strings.TrimSpace(r.URL.Query().Get("hours"))
	hours := 24
	if hoursStr != "" {
		if v, err := strconv.Atoi(hoursStr); err == nil && v > 0 {
			hours = v
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if kind != "" {
		// Single kind
		stats := metrics.ToolStatsFor(metrics.ToolKind(kind))
		history := metrics.ToolHistory(metrics.ToolKind(kind), hours)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stats": stats,
			"hours": history,
		})
		return
	}
	// All kinds
	stats := metrics.ToolStats()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"stats": stats,
	})
}
