package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	msgBadQuota        = "monthlyTokenQuota must be a whole number from 1 to 1000000000000000, or null"
	msgNeedsAdminQuota = "only admins change the token quota"
)

// isQuotaKey reports whether key is the token quota setting, which only an
// admin changes or sees.
func isQuotaKey(key string) bool { return key == "monthlyTokenQuota" }

// quotaValue reads a quota from its JSON text: null (none) or a whole number
// from 1 to config.MaxMonthlyTokenQuota. The text itself is read, never a
// float, so a fraction, an exponent, a string and a number a float cannot hold
// exactly are all refused, and nothing is rounded.
func quotaValue(raw json.RawMessage) (*int64, bool) {
	text := bytes.TrimSpace(raw)
	if string(text) == "null" {
		return nil, true
	}
	for i, c := range text {
		if (c < '0' || c > '9') && !(c == '-' && i == 0) {
			return nil, false
		}
	}
	n, err := strconv.ParseInt(string(text), 10, 64)
	if err != nil || !config.ValidTokenQuota(n) {
		return nil, false
	}
	return &n, true
}

// quotaSetting reads monthlyTokenQuota: a whole number of tokens or null (no
// quota of its own).
func quotaSetting(raw json.RawMessage) (func(*store.DashboardRepo), string) {
	v, ok := quotaValue(raw)
	if !ok {
		return nil, msgBadQuota
	}
	return func(row *store.DashboardRepo) { row.MonthlyTokenQuota = v }, ""
}

// quotaView is what repo and its instance used this month against their
// quotas, for an admin. A usage that cannot be read is logged and nil: the
// page opens without it.
func (d *Dashboard) quotaView(ctx context.Context, se Session, repo settings.Effective) *QuotaView {
	if se.Role(repo.Instance, repo.Project) < RoleAdmin {
		return nil
	}
	id, _, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.Log.Warn("dashboard: reading a repository's project for its quota failed", "instance", repo.Instance, "project", repo.Project, "error", err.Error())
		return nil
	}
	u, err := quota.Read(ctx, d.Store, d.now(), repo.Instance, id, repo)
	if err != nil {
		d.Log.Warn("dashboard: reading the token usage failed", "instance", repo.Instance, "project", repo.Project, "error", err.Error())
		return nil
	}
	return &QuotaView{
		Repository: QuotaUseView{Used: u.Repository, Limit: u.RepositoryLimit},
		Instance:   QuotaUseView{Used: u.Instance, Limit: u.InstanceLimit},
	}
}

// putInstanceQuota sets a dashboard instance's monthly token quota: 204, or
// 409 for a config instance (its quota is in the config file) or one being
// removed.
func (d *Dashboard) putInstanceQuota(w http.ResponseWriter, r *http.Request) {
	se, name := session(r), r.PathValue("name")
	inst, ok := d.registry.Get(name)
	if !ok {
		notFound(w)
		return
	}
	if inst.Source == instances.SourceConfig {
		writeError(w, http.StatusConflict, msgConfigInstance)
		return
	}
	var in struct {
		Quota json.RawMessage `json:"monthlyTokenQuota"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, msgBadQuota)
		return
	}
	v, ok := quotaValue(in.Quota)
	if !ok {
		writeError(w, http.StatusBadRequest, msgBadQuota)
		return
	}
	ctx := r.Context()
	found, err := d.Store.SetInstanceTokenQuota(ctx, name, v)
	if errors.Is(err, store.ErrRemoving) {
		writeError(w, http.StatusConflict, msgInstanceRemoving)
		return
	}
	if err != nil {
		d.Log.Error("dashboard: setting an instance's token quota failed", "instance", name, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if !found {
		notFound(w)
		return
	}
	d.registry.SetTokenQuota(name, v)
	value := quotaText(v)
	detail := "monthly_token_quota=" + value
	d.Log.Info("dashboard: instance updated", "instance", name, "monthly_token_quota", value, "by", se.Username)
	d.audit(ctx, se, "instance_updated", name, "", detail)
	w.WriteHeader(http.StatusNoContent)
}

// quotaText is a quota as the log and the audit log name it: the number, or
// "none" for no quota.
func quotaText(v *int64) string {
	if v == nil {
		return "none"
	}
	return strconv.FormatInt(*v, 10)
}
