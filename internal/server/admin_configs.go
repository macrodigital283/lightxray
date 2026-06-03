package server

import "net/http"

// allConfigsResponse mimics the SLICE of Hiddify's /all-configs/ that the
// pool actually reads: chconfigs["0"].proxy_path_client. Everything else
// the pool ignores, so we return an empty structure for those keys.
type allConfigsResponse struct {
	ChConfigs map[string]chConfig `json:"chconfigs"`
}

type chConfig struct {
	ProxyPathClient string `json:"proxy_path_client"`
	ProxyPathAdmin  string `json:"proxy_path_admin"`
}

// adminAllConfigs — GET /api/v2/admin/all-configs/
// Pool calls this once per panel to discover the customer-facing proxy
// path. We surface our configured client/admin paths.
func (d Deps) adminAllConfigs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, allConfigsResponse{
		ChConfigs: map[string]chConfig{
			"0": {
				ProxyPathClient: d.cfg.ClientProxyPath,
				ProxyPathAdmin:  d.cfg.AdminProxyPath,
			},
		},
	})
}
