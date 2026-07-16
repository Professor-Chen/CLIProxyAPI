package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeOpenAIResponsesReasoningEncryptedContentDropsPlaintext(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"当前任务中确认。","summary":[]},{"role":"user","content":"hi"}]}`)
	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("plaintext encrypted_content should be dropped: %s", string(got))
	}
}

func TestSanitizeOpenAIResponsesReasoningEncryptedContentDropsInvalidCompaction(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction","encrypted_content":"not-a-signature"},{"role":"user","content":"hi"}]}`)
	got := sanitizeOpenAIResponsesReasoningEncryptedContent(context.Background(), "test", body)
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("invalid compaction encrypted_content should be dropped: %s", string(got))
	}
}

func TestStripOpenAIResponsesEncryptedContentRemovesAll(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"gAAAAABaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"type":"compaction","encrypted_content":"gAAAAABbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"role":"user","content":"hi"}]}`)
	got, n := stripOpenAIResponsesEncryptedContent(body)
	if n != 2 {
		t.Fatalf("stripped=%d, want 2; body=%s", n, string(got))
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() || gjson.GetBytes(got, "input.1.encrypted_content").Exists() {
		t.Fatalf("encrypted_content should be removed: %s", string(got))
	}
}

func TestIsCodexThinkingSignatureInvalidRecognizesVerifyMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"The encrypted content gAAA...kpk= could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`)
	if !isCodexThinkingSignatureInvalid(400, body) {
		t.Fatal("expected thinking_signature_invalid classification")
	}
}

func TestParseOpenAIResponsesMissingStoredItemID(t *testing.T) {
	body := []byte(`{"error":{"message":"Item with id 'rs_e4b629791682303c4be63272' not found. Items are not persisted when ` + "`store`" + ` is set to false. Try again with ` + "`store`" + ` set to true, or remove this item from your input."}}`)
	got := parseOpenAIResponsesMissingStoredItemID(body)
	if got != "rs_e4b629791682303c4be63272" {
		t.Fatalf("got %q", got)
	}
}

func TestStripOpenAIResponsesInputItemsByID(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_bad"},{"type":"message","role":"user","content":"hi"},{"type":"item_reference","id":"rs_bad"}]}`)
	got, n := stripOpenAIResponsesInputItemsByID(body, "rs_bad")
	if n != 2 {
		t.Fatalf("removed=%d want 2 body=%s", n, got)
	}
	if gjson.GetBytes(got, "input.#").Int() != 1 {
		t.Fatalf("kept=%s", got)
	}
	if gjson.GetBytes(got, "input.0.content").String() != "hi" {
		t.Fatalf("unexpected kept item: %s", got)
	}
}

func TestStripOpenAIResponsesItemReferences(t *testing.T) {
	body := []byte(`{"input":[{"type":"item_reference","id":"rs_1"},{"type":"message","role":"user","content":"hi"}]}`)
	got, n := stripOpenAIResponsesItemReferences(body)
	if n != 1 {
		t.Fatalf("removed=%d want 1", n)
	}
	if gjson.GetBytes(got, "input.#").Int() != 1 || gjson.GetBytes(got, "input.0.content").String() != "hi" {
		t.Fatalf("body=%s", got)
	}
}

func TestPrepareCodexResponsesRetryBodyMissingStoredItem(t *testing.T) {
	req := []byte(`{"input":[{"type":"reasoning","id":"rs_e4b629791682303c4be63272"},{"type":"message","role":"user","content":"hi"}]}`)
	errBody := []byte(`{"error":{"message":"Item with id 'rs_e4b629791682303c4be63272' not found. Items are not persisted when ` + "`store`" + ` is set to false."}}`)
	next, reason, ok := prepareCodexResponsesRetryBody(req, 404, errBody)
	if !ok || !strings.Contains(reason, "missing_stored_item") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
	if gjson.GetBytes(next, "input.#").Int() != 1 {
		t.Fatalf("next=%s", next)
	}
}
