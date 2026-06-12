package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// codexProviderKey is the provider identifier reported by the Codex executor.
const codexProviderKey = "codex"

// codexQuotaMetadataKey is the Auth.Metadata key under which the most recent
// Codex quota snapshot is stored. Persisting it here means the data survives
// restarts and is exposed verbatim through the management API.
const codexQuotaMetadataKey = "codex_quota"

// CodexQuotaWindow captures one usage window parsed from the x-codex-* response
// header family OpenAI Codex returns on /responses traffic. "primary" is the
// short (5h, window=300) window and "secondary" is the weekly (window=10080)
// window.
type CodexQuotaWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes,omitempty"`
	ResetsAt      int64   `json:"resets_at,omitempty"` // unix seconds; 0 when unknown
}

// CodexQuotaSnapshot is the parsed x-codex-* quota family for one credential.
type CodexQuotaSnapshot struct {
	Primary    *CodexQuotaWindow `json:"primary,omitempty"`
	Secondary  *CodexQuotaWindow `json:"secondary,omitempty"`
	PlanType   string            `json:"plan_type,omitempty"`
	CapturedAt int64             `json:"captured_at"` // unix seconds
}

// ParseCodexQuotaHeaders extracts the x-codex-* rate-limit family from a set of
// response headers. It returns nil when none of the quota headers are present
// (non-codex responses, or responses that omit the family) so callers can keep
// any previously captured snapshot untouched.
func ParseCodexQuotaHeaders(h http.Header) *CodexQuotaSnapshot {
	if len(h) == 0 {
		return nil
	}
	primary := parseCodexQuotaWindow(h, "X-Codex-Primary")
	secondary := parseCodexQuotaWindow(h, "X-Codex-Secondary")
	plan := strings.TrimSpace(h.Get("X-Codex-Plan-Type"))
	if primary == nil && secondary == nil && plan == "" {
		return nil
	}
	return &CodexQuotaSnapshot{
		Primary:    primary,
		Secondary:  secondary,
		PlanType:   plan,
		CapturedAt: time.Now().Unix(),
	}
}

func parseCodexQuotaWindow(h http.Header, prefix string) *CodexQuotaWindow {
	usedRaw := strings.TrimSpace(h.Get(prefix + "-Used-Percent"))
	if usedRaw == "" {
		return nil
	}
	used, err := strconv.ParseFloat(usedRaw, 64)
	if err != nil {
		return nil
	}
	if used < 0 {
		used = 0
	}
	window := &CodexQuotaWindow{UsedPercent: used}
	if raw := strings.TrimSpace(h.Get(prefix + "-Window-Minutes")); raw != "" {
		if n, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && n > 0 {
			window.WindowMinutes = n
		}
	}
	window.ResetsAt = parseCodexReset(h, prefix)
	return window
}

// parseCodexReset normalises the reset hint into an absolute unix-seconds
// timestamp. OpenAI has shipped two shapes for this: an absolute -Reset-At
// (seconds, occasionally milliseconds) and a relative -Reset-After-Seconds.
// Both are accepted; the absolute form wins when present.
func parseCodexReset(h http.Header, prefix string) int64 {
	if raw := strings.TrimSpace(h.Get(prefix + "-Reset-At")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			if n > 1e12 { // milliseconds -> seconds
				n /= 1000
			}
			return n
		}
	}
	if raw := strings.TrimSpace(h.Get(prefix + "-Reset-After-Seconds")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return time.Now().Add(time.Duration(n) * time.Second).Unix()
		}
	}
	return 0
}

// ToMetadata renders the snapshot as a plain map so it can live inside
// Auth.Metadata and round-trip through JSON persistence as a generic value
// (a freshly parsed snapshot and a reloaded one then look identical).
func (s *CodexQuotaSnapshot) ToMetadata() map[string]any {
	if s == nil {
		return nil
	}
	out := map[string]any{"captured_at": s.CapturedAt}
	if s.PlanType != "" {
		out["plan_type"] = s.PlanType
	}
	if m := s.Primary.toMetadata(); m != nil {
		out["primary"] = m
	}
	if m := s.Secondary.toMetadata(); m != nil {
		out["secondary"] = m
	}
	return out
}

func (w *CodexQuotaWindow) toMetadata() map[string]any {
	if w == nil {
		return nil
	}
	out := map[string]any{"used_percent": w.UsedPercent}
	if w.WindowMinutes > 0 {
		out["window_minutes"] = w.WindowMinutes
	}
	if w.ResetsAt > 0 {
		out["resets_at"] = w.ResetsAt
	}
	return out
}

// ApplyCodexQuotaSnapshot persists a parsed Codex quota snapshot onto the
// credential's Metadata under the shared codex_quota key. Passive capture
// (MarkResult, via applyCodexQuotaFromContext) and the active probe management
// endpoint both funnel through here so they converge on identical storage and
// the auth-files API exposes one shape regardless of how the snapshot was
// obtained. Nil auth or nil snapshot are no-ops; the boolean reports whether
// anything was written so callers can decide whether to persist the auth.
func ApplyCodexQuotaSnapshot(auth *Auth, snapshot *CodexQuotaSnapshot) bool {
	if auth == nil || snapshot == nil {
		return false
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[codexQuotaMetadataKey] = snapshot.ToMetadata()
	return true
}

// preserveCodexQuotaMetadata carries an existing codex_quota snapshot forward
// onto an incoming auth that lacks one. Manager.Update rebuilds the credential
// from the caller's snapshot, and token refresh in particular clones a
// credential *before* refreshing and then calls Update with that stale clone.
// Without this, a refresh racing with quota capture (passive MarkResult or the
// active probe endpoint) would overwrite a freshly stored codex_quota with an
// older Metadata map that no longer contains it. When the incoming auth already
// carries a codex_quota entry it wins, since it is the more recent write.
// Callers must hold the manager lock (it reads the existing credential).
func preserveCodexQuotaMetadata(incoming, existing *Auth) {
	if incoming == nil || existing == nil || len(existing.Metadata) == 0 {
		return
	}
	existingQuota, ok := existing.Metadata[codexQuotaMetadataKey]
	if !ok || existingQuota == nil {
		return
	}
	if incoming.Metadata != nil {
		if current, exists := incoming.Metadata[codexQuotaMetadataKey]; exists && current != nil {
			return
		}
	}
	if incoming.Metadata == nil {
		incoming.Metadata = make(map[string]any)
	}
	incoming.Metadata[codexQuotaMetadataKey] = existingQuota
}

// applyCodexQuotaFromContext is the passive collector. MarkResult calls this on
// every codex execution result (success or failure) with the same context the
// executor used; the executor stashes the upstream response headers via
// logging.SetResponseHeaders *before* it inspects the status code, so a 429
// reply reaches us with its reset timers intact while a success reply refreshes
// the used-percent. We lift the x-codex-* quota family out and persist it onto
// the credential. Header-less failures (network errors, timeouts) yield no
// quota headers and are silently skipped. Callers must hold the manager lock
// (this only mutates the passed-in auth, which MarkResult then persists).
func applyCodexQuotaFromContext(ctx context.Context, auth *Auth, provider string) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(provider), codexProviderKey) {
		return
	}
	snapshot := ParseCodexQuotaHeaders(logging.GetResponseHeaders(ctx))
	if snapshot == nil {
		return
	}
	ApplyCodexQuotaSnapshot(auth, snapshot)
}
