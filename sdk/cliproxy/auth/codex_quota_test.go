package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestParseCodexQuotaHeaders_FullPrimaryAndSecondary(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "12.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", "1900000000")
	h.Set("x-codex-secondary-used-percent", "47")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", "1900500000")
	h.Set("x-codex-plan-type", "plus")

	snap := ParseCodexQuotaHeaders(h)
	if snap == nil {
		t.Fatal("expected snapshot, got nil")
	}
	if snap.PlanType != "plus" {
		t.Errorf("plan type = %q, want plus", snap.PlanType)
	}
	if snap.CapturedAt == 0 {
		t.Error("captured_at should be set")
	}
	if snap.Primary == nil || snap.Primary.UsedPercent != 12.5 {
		t.Fatalf("primary used percent wrong: %+v", snap.Primary)
	}
	if snap.Primary.WindowMinutes != 300 {
		t.Errorf("primary window = %d, want 300", snap.Primary.WindowMinutes)
	}
	if snap.Primary.ResetsAt != 1900000000 {
		t.Errorf("primary reset = %d, want 1900000000", snap.Primary.ResetsAt)
	}
	if snap.Secondary == nil || snap.Secondary.UsedPercent != 47 {
		t.Fatalf("secondary used percent wrong: %+v", snap.Secondary)
	}
	if snap.Secondary.WindowMinutes != 10080 {
		t.Errorf("secondary window = %d, want 10080", snap.Secondary.WindowMinutes)
	}
}

func TestParseCodexQuotaHeaders_ResetAtMilliseconds(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "5")
	h.Set("x-codex-primary-reset-at", "1900000000000") // ms
	snap := ParseCodexQuotaHeaders(h)
	if snap == nil || snap.Primary == nil {
		t.Fatal("expected primary snapshot")
	}
	if snap.Primary.ResetsAt != 1900000000 {
		t.Errorf("reset = %d, want 1900000000 (ms normalised to s)", snap.Primary.ResetsAt)
	}
}

func TestParseCodexQuotaHeaders_ResetAfterSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "5")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	before := time.Now().Add(3600 * time.Second).Unix()
	snap := ParseCodexQuotaHeaders(h)
	after := time.Now().Add(3600 * time.Second).Unix()
	if snap == nil || snap.Primary == nil {
		t.Fatal("expected primary snapshot")
	}
	if snap.Primary.ResetsAt < before || snap.Primary.ResetsAt > after {
		t.Errorf("reset = %d, want within [%d,%d]", snap.Primary.ResetsAt, before, after)
	}
}

func TestParseCodexQuotaHeaders_AbsoluteResetWinsOverRelative(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "5")
	h.Set("x-codex-primary-reset-at", "1900000000")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	snap := ParseCodexQuotaHeaders(h)
	if snap == nil || snap.Primary == nil {
		t.Fatal("expected primary snapshot")
	}
	if snap.Primary.ResetsAt != 1900000000 {
		t.Errorf("reset = %d, want absolute 1900000000 to win", snap.Primary.ResetsAt)
	}
}

func TestParseCodexQuotaHeaders_NoQuotaHeaders(t *testing.T) {
	if snap := ParseCodexQuotaHeaders(http.Header{}); snap != nil {
		t.Errorf("empty headers should yield nil, got %+v", snap)
	}
	h := http.Header{}
	h.Set("content-type", "application/json")
	if snap := ParseCodexQuotaHeaders(h); snap != nil {
		t.Errorf("non-quota headers should yield nil, got %+v", snap)
	}
}

func TestParseCodexQuotaHeaders_PlanTypeOnly(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-plan-type", "pro")
	snap := ParseCodexQuotaHeaders(h)
	if snap == nil {
		t.Fatal("plan-type alone should still yield a snapshot")
	}
	if snap.Primary != nil || snap.Secondary != nil {
		t.Error("no windows expected when only plan-type present")
	}
	if snap.PlanType != "pro" {
		t.Errorf("plan = %q, want pro", snap.PlanType)
	}
}

func TestParseCodexQuotaHeaders_InvalidUsedPercentIgnored(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "not-a-number")
	h.Set("x-codex-secondary-used-percent", "30")
	snap := ParseCodexQuotaHeaders(h)
	if snap == nil {
		t.Fatal("expected snapshot from valid secondary")
	}
	if snap.Primary != nil {
		t.Errorf("invalid primary used-percent should be dropped, got %+v", snap.Primary)
	}
	if snap.Secondary == nil || snap.Secondary.UsedPercent != 30 {
		t.Fatalf("secondary should parse: %+v", snap.Secondary)
	}
}

func TestCodexQuotaSnapshot_ToMetadataRoundTrip(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "12.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", "1900000000")
	h.Set("x-codex-plan-type", "plus")
	snap := ParseCodexQuotaHeaders(h)

	m := snap.ToMetadata()
	if m["plan_type"] != "plus" {
		t.Errorf("plan_type = %v", m["plan_type"])
	}
	if _, ok := m["captured_at"]; !ok {
		t.Error("captured_at missing from metadata")
	}
	primary, ok := m["primary"].(map[string]any)
	if !ok {
		t.Fatalf("primary metadata not a map: %T", m["primary"])
	}
	if primary["used_percent"] != 12.5 {
		t.Errorf("used_percent = %v, want 12.5", primary["used_percent"])
	}
	if primary["window_minutes"] != int64(300) {
		t.Errorf("window_minutes = %v (%T), want int64 300", primary["window_minutes"], primary["window_minutes"])
	}
	if primary["resets_at"] != int64(1900000000) {
		t.Errorf("resets_at = %v, want 1900000000", primary["resets_at"])
	}
	if _, ok := m["secondary"]; ok {
		t.Error("secondary should be absent when not provided")
	}
}

func TestApplyCodexQuotaFromContext_NonCodexProviderSkipped(t *testing.T) {
	a := &Auth{}
	applyCodexQuotaFromContext(nil, a, "gemini")
	if a.Metadata != nil {
		t.Errorf("non-codex provider should not write metadata, got %+v", a.Metadata)
	}
}

// Mirrors the 429 path: the executor stashed the upstream headers into ctx and
// MarkResult later lifts the quota (incl. reset timers) onto the credential.
func TestApplyCodexQuotaFromContext_CapturesFromHeaders(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	logging.SetResponseHeaders(ctx, h)

	a := &Auth{Provider: codexProviderKey}
	applyCodexQuotaFromContext(ctx, a, codexProviderKey)

	if a.Metadata == nil {
		t.Fatal("expected codex_quota metadata to be written")
	}
	q, ok := a.Metadata[codexQuotaMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("codex_quota not a map: %T", a.Metadata[codexQuotaMetadataKey])
	}
	primary, ok := q["primary"].(map[string]any)
	if !ok {
		t.Fatalf("primary window missing: %v", q)
	}
	if primary["used_percent"] != 100.0 {
		t.Errorf("used_percent = %v, want 100", primary["used_percent"])
	}
	if _, ok := primary["resets_at"]; !ok {
		t.Error("resets_at expected (derived from reset-after-seconds on the 429 reply)")
	}
}

func TestApplyCodexQuotaFromContext_NoHeadersLeavesMetadataUntouched(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{}) // header-less failure (timeout/network)

	a := &Auth{Provider: codexProviderKey}
	applyCodexQuotaFromContext(ctx, a, codexProviderKey)
	if a.Metadata != nil {
		t.Errorf("no quota headers should not write metadata, got %+v", a.Metadata)
	}
}
