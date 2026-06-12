package management

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// probeStatusErr is a minimal cliproxyexecutor.StatusError implementation used
// to exercise the probe error classifier without a live executor.
type probeStatusErr struct {
	code int
	msg  string
}

func (e probeStatusErr) Error() string   { return e.msg }
func (e probeStatusErr) StatusCode() int { return e.code }

func TestPickCodexProbeModel(t *testing.T) {
	cases := []struct {
		name   string
		models []*registry.ModelInfo
		want   string
	}{
		{
			name:   "empty",
			models: nil,
			want:   "",
		},
		{
			name:   "only image is skipped",
			models: []*registry.ModelInfo{{ID: "gpt-image-2"}},
			want:   "",
		},
		{
			name: "prefers mini text model",
			models: []*registry.ModelInfo{
				{ID: "gpt-image-2"},
				{ID: "gpt-5.4"},
				{ID: "gpt-5.4-mini"},
			},
			want: "gpt-5.4-mini",
		},
		{
			name: "falls back to first non-image",
			models: []*registry.ModelInfo{
				{ID: "gpt-image-2"},
				{ID: "gpt-5.4-codex"},
				{ID: "gpt-5.4"},
			},
			want: "gpt-5.4-codex",
		},
		{
			name: "skips blank and nil entries",
			models: []*registry.ModelInfo{
				nil,
				{ID: "  "},
				{ID: "gpt-5.4"},
			},
			want: "gpt-5.4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickCodexProbeModel(tc.models); got != tc.want {
				t.Fatalf("pickCodexProbeModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantStatus   int
		wantCategory string
	}{
		{"nil is ok", nil, http.StatusOK, ""},
		{"429 rate limited", probeStatusErr{code: 429, msg: "upstream 429"}, 429, probeErrCategoryRateLimited},
		{"401 auth invalid", probeStatusErr{code: 401, msg: "unauthorized"}, 401, probeErrCategoryAuthInvalid},
		{"403 generic is auth invalid", probeStatusErr{code: 403, msg: "forbidden"}, 403, probeErrCategoryAuthInvalid},
		{"403 deactivated is banned", probeStatusErr{code: 403, msg: "account_deactivated: your account was deactivated"}, 403, probeErrCategoryBanned},
		{"500 upstream", probeStatusErr{code: 500, msg: "boom"}, 500, probeErrCategoryUpstream},
		{"408 stream disconnect is upstream", probeStatusErr{code: 408, msg: "stream disconnected"}, 408, probeErrCategoryUpstream},
		{"plain error is network", errPlainNetwork{}, 0, probeErrCategoryNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, category := classifyProbeError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if category != tc.wantCategory {
				t.Errorf("category = %q, want %q", category, tc.wantCategory)
			}
		})
	}
}

type errPlainNetwork struct{}

func (errPlainNetwork) Error() string { return "dial tcp: i/o timeout" }

// quotaHeaders mocks the upstream x-codex-* family the executor would have
// stashed in the context; ParseCodexQuotaHeaders is the shared parser used by
// both passive capture and the probe.
func quotaHeaders() http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	h.Set("x-codex-plan-type", "plus")
	return h
}

func TestBuildProbeOutcome_Success(t *testing.T) {
	snapshot := coreauth.ParseCodexQuotaHeaders(quotaHeaders())
	if snapshot == nil {
		t.Fatal("expected snapshot from mock headers")
	}
	out := buildProbeOutcome(snapshot, nil)
	if !out.OK {
		t.Error("success probe should be ok")
	}
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok", out.Status)
	}
	if out.HTTPStatus != http.StatusOK {
		t.Errorf("http status = %d, want 200", out.HTTPStatus)
	}
	if out.ErrorCategory != "" || out.ErrorMessage != "" {
		t.Errorf("success should carry no error, got category=%q message=%q", out.ErrorCategory, out.ErrorMessage)
	}
}

func TestBuildProbeOutcome_RateLimitedStillOK(t *testing.T) {
	// A 429 that still carried quota headers is the canonical "probe success
	// despite error" case: the limit state is the result we wanted.
	snapshot := coreauth.ParseCodexQuotaHeaders(quotaHeaders())
	out := buildProbeOutcome(snapshot, probeStatusErr{code: 429, msg: "upstream 429: rate limit"})
	if !out.OK {
		t.Error("429 with quota headers should count as a successful probe")
	}
	if out.Status != probeErrCategoryRateLimited {
		t.Errorf("status = %q, want %q", out.Status, probeErrCategoryRateLimited)
	}
	if out.HTTPStatus != 429 {
		t.Errorf("http status = %d, want 429", out.HTTPStatus)
	}
	if out.ErrorCategory != probeErrCategoryRateLimited {
		t.Errorf("category = %q, want %q", out.ErrorCategory, probeErrCategoryRateLimited)
	}
}

func TestBuildProbeOutcome_AuthFailureNoSnapshot(t *testing.T) {
	out := buildProbeOutcome(nil, probeStatusErr{code: 401, msg: "unauthorized"})
	if out.OK {
		t.Error("401 without snapshot should not be ok")
	}
	if out.Status != probeErrCategoryAuthInvalid {
		t.Errorf("status = %q, want %q", out.Status, probeErrCategoryAuthInvalid)
	}
	if out.ErrorMessage == "" {
		t.Error("auth failure should surface an error message")
	}
}

func TestBuildProbeOutcome_NetworkFailure(t *testing.T) {
	out := buildProbeOutcome(nil, errPlainNetwork{})
	if out.OK {
		t.Error("network failure should not be ok")
	}
	if out.Status != probeErrCategoryNetwork {
		t.Errorf("status = %q, want %q", out.Status, probeErrCategoryNetwork)
	}
	if out.HTTPStatus != 0 {
		t.Errorf("network failure should have no upstream status, got %d", out.HTTPStatus)
	}
}

func TestSummarizeProbeError(t *testing.T) {
	if got := summarizeProbeError(nil); got != "" {
		t.Errorf("nil error should summarize to empty, got %q", got)
	}
	long := strings.Repeat("x", probeErrorMessageLimit+50)
	got := summarizeProbeError(probeStatusErr{code: 500, msg: long})
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-long message should be truncated with ellipsis, got %q", got)
	}
	if len([]rune(got)) != probeErrorMessageLimit+1 {
		t.Errorf("truncated length = %d runes, want %d", len([]rune(got)), probeErrorMessageLimit+1)
	}
	multiline := summarizeProbeError(probeStatusErr{code: 500, msg: "line1\nline2"})
	if strings.Contains(multiline, "\n") {
		t.Errorf("newlines should be collapsed, got %q", multiline)
	}
}

func TestCodexProbePayloadShape(t *testing.T) {
	payload := string(codexProbePayload("gpt-5.4-mini"))
	for _, want := range []string{`"model":"gpt-5.4-mini"`, `"input":"hi"`, `"max_output_tokens":16`, `"stream":false`} {
		if !strings.Contains(payload, want) {
			t.Errorf("probe payload missing %s: %s", want, payload)
		}
	}
}
