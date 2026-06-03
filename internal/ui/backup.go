package ui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/macrodigital283/lightxray/internal/db"
)

// backupFile is the on-disk JSON shape. Version-stamped so we can change
// the schema later without silently breaking older backups — the restore
// path checks the version and refuses unknowns.
type backupFile struct {
	Version    int             `json:"version"`
	ExportedAt time.Time       `json:"exported_at"`
	PublicHost string          `json:"public_host,omitempty"`
	Users      []backupUser    `json:"users"`
	CDNHosts   []backupCDNHost `json:"cdn_hosts"`
}

type backupUser struct {
	UUID            string     `json:"uuid"`
	Name            string     `json:"name"`
	Comment         string     `json:"comment,omitempty"`
	UsageBytes      int64      `json:"usage_bytes"`
	UsageLimitBytes int64      `json:"usage_limit_bytes"`
	PackageDays     int        `json:"package_days"`
	StartDate       *time.Time `json:"start_date,omitempty"`
	LastResetTime   time.Time  `json:"last_reset_time"`
	LastOnline      *time.Time `json:"last_online,omitempty"`
	Enable          bool       `json:"enable"`
	Mode            string     `json:"mode"`
	Lang            string     `json:"lang"`
	AddedByUUID     string     `json:"added_by_uuid"`
	CreatedAt       time.Time  `json:"created_at"`
}

type backupCDNHost struct {
	Hostname string `json:"hostname"`
	Enabled  bool   `json:"enabled"`
}

// backupPage — GET /ui/backup/
// Static page with a download link + restore upload form. Re-rendered
// after a restore with a result summary so the operator sees what
// happened.
func (u *UI) backupPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "backup_page", nil)
}

// backupExport — GET /ui/backup/export.json
// Streams the JSON file with Content-Disposition: attachment so browsers
// kick off a download.
func (u *UI) backupExport(w http.ResponseWriter, r *http.Request) {
	users, err := u.store.ListUsers(r.Context())
	if err != nil {
		slog.Error("ui backup: list users", "err", err)
		u.renderError(w, http.StatusInternalServerError, "list users failed")
		return
	}
	hosts, err := u.store.ListCDNHosts(r.Context())
	if err != nil {
		slog.Error("ui backup: list cdn", "err", err)
		u.renderError(w, http.StatusInternalServerError, "list cdn hosts failed")
		return
	}

	bf := backupFile{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		PublicHost: u.cfg.PublicHost,
		Users:      make([]backupUser, 0, len(users)),
		CDNHosts:   make([]backupCDNHost, 0, len(hosts)),
	}
	for _, dbu := range users {
		bf.Users = append(bf.Users, backupUser{
			UUID:            dbu.UUID.String(),
			Name:            dbu.Name,
			Comment:         dbu.Comment,
			UsageBytes:      dbu.UsageBytes,
			UsageLimitBytes: dbu.UsageLimitBytes,
			PackageDays:     dbu.PackageDays,
			StartDate:       dbu.StartDate,
			LastResetTime:   dbu.LastResetTime,
			LastOnline:      dbu.LastOnline,
			Enable:          dbu.Enable,
			Mode:            dbu.Mode,
			Lang:            dbu.Lang,
			AddedByUUID:     dbu.AddedByUUID.String(),
			CreatedAt:       dbu.CreatedAt,
		})
	}
	for _, h := range hosts {
		bf.CDNHosts = append(bf.CDNHosts, backupCDNHost{
			Hostname: h.Hostname,
			Enabled:  h.Enabled,
		})
	}

	fname := "lightxray-backup-" + time.Now().UTC().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bf); err != nil {
		slog.Error("ui backup: encode", "err", err)
	}
}

// backupRestore — POST /ui/backup/restore (multipart form, file field
// "backup").
//
// Semantics: non-destructive merge. Users with an already-present UUID
// are SKIPPED (not overwritten). CDN hosts with an already-present
// hostname are also skipped (via ON CONFLICT DO NOTHING in AddCDNHost).
// Each newly-inserted enabled user is also registered with xray so they
// can immediately connect.
//
// On success, re-renders the page with a summary card.
func (u *UI) backupRestore(w http.ResponseWriter, r *http.Request) {
	// 10 MB cap — way more than enough for tens of thousands of users.
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		u.backupPageWith(w, r, "Upload failed: "+err.Error(), nil)
		return
	}
	f, hdr, err := r.FormFile("backup")
	if err != nil {
		u.backupPageWith(w, r, "No file uploaded.", nil)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 50<<20))
	if err != nil {
		u.backupPageWith(w, r, "File read failed.", nil)
		return
	}
	slog.Info("ui backup restore: parsing", "filename", hdr.Filename, "bytes", len(data))

	var bf backupFile
	if err := json.Unmarshal(data, &bf); err != nil {
		u.backupPageWith(w, r, "Invalid JSON: "+err.Error(), nil)
		return
	}
	if bf.Version != 1 {
		u.backupPageWith(w, r, "Unsupported backup version (expected 1).", nil)
		return
	}

	res := &restoreResult{}
	adminFallback, _ := uuid.Parse(u.cfg.AdminUUID)

	for _, bu := range bf.Users {
		id, err := uuid.Parse(bu.UUID)
		if err != nil {
			res.Errors = append(res.Errors, "bad uuid: "+bu.UUID)
			continue
		}
		addedBy, perr := uuid.Parse(bu.AddedByUUID)
		if perr != nil || addedBy == uuid.Nil {
			addedBy = adminFallback
		}
		_, inserted, err := u.store.RestoreUser(r.Context(), db.RestoreUserInput{
			UUID:            id,
			Name:            bu.Name,
			Comment:         bu.Comment,
			UsageBytes:      bu.UsageBytes,
			UsageLimitBytes: bu.UsageLimitBytes,
			PackageDays:     bu.PackageDays,
			StartDate:       bu.StartDate,
			LastResetTime:   bu.LastResetTime,
			LastOnline:      bu.LastOnline,
			Enable:          bu.Enable,
			Mode:            bu.Mode,
			Lang:            bu.Lang,
			AddedByUUID:     addedBy,
			CreatedAt:       bu.CreatedAt,
		})
		if err != nil {
			res.Errors = append(res.Errors, bu.UUID+": "+err.Error())
			continue
		}
		if inserted {
			res.UsersAdded++
			if bu.Enable {
				if err := u.xc.AddUser(r.Context(), id); err != nil {
					slog.Warn("ui backup restore: xray AddUser", "uuid", id, "err", err)
				}
			}
		} else {
			res.UsersSkipped++
		}
	}

	for _, h := range bf.CDNHosts {
		if h.Hostname == "" {
			continue
		}
		if _, err := u.store.AddCDNHost(r.Context(), h.Hostname); err != nil {
			res.Errors = append(res.Errors, h.Hostname+": "+err.Error())
			continue
		}
		res.HostsTouched++
	}

	u.backupPageWith(w, r, "", res)
}

// restoreResult is the summary the dashboard surfaces after a restore.
type restoreResult struct {
	UsersAdded   int
	UsersSkipped int
	HostsTouched int
	Errors       []string
}

func (u *UI) backupPageWith(w http.ResponseWriter, r *http.Request, errMsg string, res *restoreResult) {
	data := map[string]any{}
	if errMsg != "" {
		data["Error"] = errMsg
	}
	if res != nil {
		data["Restored"] = res
	}
	u.render(w, "backup_page", data)
}
