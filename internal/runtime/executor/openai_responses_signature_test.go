package executor

import (
	"context"
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
