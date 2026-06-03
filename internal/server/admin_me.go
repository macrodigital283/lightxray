package server

import "net/http"

// meResponse matches the pool's HiddifyMe interface exactly.
type meResponse struct {
	UUID            string `json:"uuid"`
	Name            string `json:"name"`
	Mode            string `json:"mode"`
	ParentAdminUUID string `json:"parent_admin_uuid"`
	CanAddAdmin     bool   `json:"can_add_admin"`
}

// adminMe — GET /api/v2/admin/me/
// Cheapest possible auth probe: returns the admin's profile.
func (d Deps) adminMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, meResponse{
		UUID:            d.cfg.AdminUUID,
		Name:            d.cfg.AdminName,
		Mode:            "super_admin",
		ParentAdminUUID: d.cfg.AdminUUID, // self — single-admin model
		CanAddAdmin:     false,
	})
}
