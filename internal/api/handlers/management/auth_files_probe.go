package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// codexProbeProvider is the provider key that supports active quota probing.
const codexProbeProvider = "codex"

// Probe error categories returned to the caller so new-api can drive credential
// health classification (banned / expired / limited / network) without parsing
// raw upstream bodies.
const (
	probeErrCategoryRateLimited = "rate_limited"
	probeErrCategoryAuthInvalid = "auth_invalid"
	probeErrCategoryBanned      = "banned"
	probeErrCategoryNetwork     = "network"
	probeErrCategoryUpstream    = "upstream"
)

// probeUpstreamTimeout bounds a single probe so a hung upstream cannot block the
// management request indefinitely. Throttling/cooldown across probes is the
// caller's responsibility (new-api side, per design decision D5).
const probeUpstreamTimeout = 45 * time.Second

// probeMaxOutputTokens keeps the probe request minimal: we only need the
// x-codex-* quota headers the upstream attaches to the response, not a useful
// completion. Kept above 1 so reasoning models still return a well-formed
// (possibly incomplete) response that carries the headers.
const probeMaxOutputTokens = 16

// probeErrorMessageLimit truncates upstream error bodies echoed back to the
// caller so a verbose provider error cannot bloat the management response.
const probeErrorMessageLimit = 500

// probeOutcome is the protocol-independent result of a single probe attempt.
// It is computed by pure helpers (so it is unit-testable without a live
// executor) and then rendered into the JSON response body.
type probeOutcome struct {
	OK            bool
	Status        string
	HTTPStatus    int
	ErrorCategory string
	ErrorMessage  string
}

// ProbeAuthFile actively probes a single codex credential's quota.
//
// It locates the named credential, issues a minimal Codex /responses request
// through that credential's own executor — which means the call automatically
// rides the credential's own proxy_url egress, exactly like business traffic,
// so it cannot leak the server's real IP — and then lifts the x-codex-* quota
// family out of the upstream response headers. The executor records those
// headers into the context before it inspects the status code, so even a 429
// reply yields a usable snapshot; per the contract a 429 is treated as a
// successful probe because the limit state it reports is itself the result.
//
// The parsed snapshot is persisted onto the credential through the same
// Metadata["codex_quota"] storage the passive collector uses, so the
// auth-files list reflects the fresh value immediately. No timers are started
// here (decision D5) and no image-generation request is issued (decision D2).
func (h *Handler) ProbeAuthFile(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	targetAuth := h.findAuthByNameOrID(name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(targetAuth.Provider), codexProbeProvider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "probe is only supported for codex credentials"})
		return
	}

	executor, ok := h.authManager.Executor(targetAuth.Provider)
	if !ok || executor == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex executor unavailable"})
		return
	}

	model := pickCodexProbeModel(registry.GetGlobalRegistry().GetModelsForClient(targetAuth.ID))
	if model == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "credential has no probe-able codex text model registered"})
		return
	}

	// The executor stashes upstream response headers into the context via
	// logging.SetResponseHeaders; we must seed the holders so they survive
	// here even when Execute returns an error (429/timeout/etc.).
	execCtx := logging.WithResponseStatusHolder(c.Request.Context())
	execCtx = logging.WithResponseHeadersHolder(execCtx)
	execCtx, cancel := context.WithTimeout(execCtx, probeUpstreamTimeout)
	defer cancel()

	req2 := cliproxyexecutor.Request{
		Model:   model,
		Payload: codexProbePayload(model),
		Format:  sdktranslator.FormatOpenAIResponse,
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		OriginalRequest: codexProbePayload(model),
	}

	_, execErr := executor.Execute(execCtx, targetAuth, req2, opts)

	snapshot := coreauth.ParseCodexQuotaHeaders(logging.GetResponseHeaders(execCtx))
	if snapshot != nil && coreauth.ApplyCodexQuotaSnapshot(targetAuth, snapshot) {
		targetAuth.UpdatedAt = time.Now()
		if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
			log.WithError(err).Warnf("probe: failed to persist codex quota for %s", name)
		}
	}

	outcome := buildProbeOutcome(snapshot, execErr)

	resp := gin.H{
		"name":      name,
		"model":     model,
		"probed_at": time.Now().Unix(),
		"ok":        outcome.OK,
		"status":    outcome.Status,
	}
	if outcome.HTTPStatus > 0 {
		resp["http_status"] = outcome.HTTPStatus
	}
	if snapshot != nil {
		resp["quota"] = snapshot.ToMetadata()
	}
	if outcome.ErrorCategory != "" {
		resp["error_category"] = outcome.ErrorCategory
	}
	if outcome.ErrorMessage != "" {
		resp["error"] = outcome.ErrorMessage
	}
	c.JSON(http.StatusOK, resp)
}

// findAuthByNameOrID resolves a credential by auth ID or by file name, matching
// the lookup the other auth-file handlers use.
func (h *Handler) findAuthByNameOrID(name string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	if auth, ok := h.authManager.GetByID(name); ok {
		return auth
	}
	for _, auth := range h.authManager.List() {
		if auth != nil && auth.FileName == name {
			return auth
		}
	}
	return nil
}

// codexProbePayload builds the minimal OpenAI Responses request used to probe a
// credential. It is intentionally tiny — a single "hi" capped at a handful of
// output tokens — because the goal is to elicit the quota headers, not a
// completion.
func codexProbePayload(model string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"input":"hi","max_output_tokens":%d,"stream":false}`, model, probeMaxOutputTokens))
}

// pickCodexProbeModel selects a lightweight text model to probe with from the
// credential's registered models. It skips image models (probing must never
// trigger an image generation, decision D2), prefers a "mini" text model when
// available, and otherwise falls back to the first non-image model.
func pickCodexProbeModel(models []*registry.ModelInfo) string {
	fallback := ""
	for _, m := range models {
		if m == nil {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if strings.Contains(strings.ToLower(id), "image") {
			continue
		}
		if strings.Contains(strings.ToLower(id), "mini") {
			return id
		}
		if fallback == "" {
			fallback = id
		}
	}
	return fallback
}

// buildProbeOutcome turns the parsed quota snapshot and execution error into the
// protocol-independent outcome. A probe is "ok" when it produced a usable
// snapshot (covers the 429 case) or the upstream call succeeded outright.
func buildProbeOutcome(snapshot *coreauth.CodexQuotaSnapshot, execErr error) probeOutcome {
	httpStatus, category := classifyProbeError(execErr)
	out := probeOutcome{
		HTTPStatus: httpStatus,
		OK:         snapshot != nil || execErr == nil,
	}
	if execErr == nil {
		out.Status = "ok"
		return out
	}
	out.ErrorCategory = category
	out.Status = category
	out.ErrorMessage = summarizeProbeError(execErr)
	return out
}

// classifyProbeError maps an execution error to an upstream HTTP status (0 when
// the failure never reached the upstream) and a coarse category. A failure that
// carries no StatusError is treated as a transport/network problem (timeout,
// connection refused, proxy failure).
func classifyProbeError(execErr error) (int, string) {
	if execErr == nil {
		return http.StatusOK, ""
	}
	var se cliproxyexecutor.StatusError
	if errors.As(execErr, &se) && se != nil {
		code := se.StatusCode()
		return code, categorizeProbeStatus(code, execErr.Error())
	}
	return 0, probeErrCategoryNetwork
}

// categorizeProbeStatus assigns a category to an upstream status code. 403 is
// split into a hard ban versus a generic auth failure by inspecting the error
// body for deactivation/ban wording.
func categorizeProbeStatus(code int, message string) string {
	switch code {
	case http.StatusTooManyRequests:
		return probeErrCategoryRateLimited
	case http.StatusUnauthorized:
		return probeErrCategoryAuthInvalid
	case http.StatusForbidden:
		if isProbeBanMessage(message) {
			return probeErrCategoryBanned
		}
		return probeErrCategoryAuthInvalid
	case http.StatusProxyAuthRequired:
		return probeErrCategoryNetwork
	default:
		return probeErrCategoryUpstream
	}
}

// banAccountPhrases are contiguous phrases that anchor "disabled" to the
// account, covering both word orders. Anchoring on a phrase — instead of
// testing whether "disabled" and "account" merely co-occur anywhere in the
// body — is what keeps "image generation is disabled for this account" (a
// recoverable feature disablement) out of the ban bucket while still catching
// "account has been disabled for using model X" (a real ban that happens to
// mention a model).
var banAccountPhrases = []string{
	"account has been disabled",
	"account is disabled",
	"account was disabled",
	"account disabled",
	"disabled your account",
	"disabled this account",
	"disabled the account",
}

// isProbeBanMessage classifies an upstream 403 body as an account ban using
// ordered phrase anchoring; the steps are evaluated in order and the first
// match wins:
//
//  1. Unambiguous account-ban verbs (deactivated/suspended/terminated/banned)
//     win regardless of any feature/model wording elsewhere in the message.
//  2. Account-anchored "disabled" phrases (banAccountPhrases). Because this
//     runs before any feature/model check, a real ban such as "account has
//     been disabled for using model X" is caught here and the trailing "model"
//     cannot pull it into auth_invalid.
//
// Anything else — feature/model disablement ("this model is disabled") and
// generic 403s — is not a ban; the caller routes it to the recoverable
// auth_invalid bucket.
//
// This is a deliberately bounded single-probe heuristic: a maliciously phrased
// body could still slip past these anchors. That is acceptable because the
// new-api health classifier has a statistical fallback (sustained 401/403 with
// zero successes over 24h -> banned) that self-heals any single-probe
// banned<->auth_invalid misclassification. The edge is therefore non-fatal, and
// we intentionally stop adding substring rules to chase ever-more-contrived
// phrasings.
func isProbeBanMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, verb := range []string{"deactivated", "suspended", "terminated", "banned"} {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	for _, phrase := range banAccountPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// summarizeProbeError trims an upstream error to a bounded, single-line string
// suitable for embedding in the JSON response.
func summarizeProbeError(execErr error) string {
	if execErr == nil {
		return ""
	}
	msg := strings.TrimSpace(execErr.Error())
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > probeErrorMessageLimit {
		msg = msg[:probeErrorMessageLimit] + "…"
	}
	return msg
}
