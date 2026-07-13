package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func sanitizeOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	updated := body
	for index, item := range input.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		// reasoning + compaction both carry opaque encrypted_content that must
		// stay account-bound; drop plaintext / foreign / malformed blobs early.
		if itemType != "reasoning" && itemType != "compaction" {
			continue
		}

		encryptedContentPath := fmt.Sprintf("input.%d.encrypted_content", index)
		encryptedContent := gjson.GetBytes(updated, encryptedContentPath)
		if !encryptedContent.Exists() {
			continue
		}

		reason := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
				reason = err.Error()
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			continue
		}

		next, err := sjson.DeleteBytes(updated, encryptedContentPath)
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop invalid %s encrypted_content at input[%d]: %v", provider, itemType, index, err)
			continue
		}
		updated = next

		itemID := strings.TrimSpace(gjson.GetBytes(updated, fmt.Sprintf("input.%d.id", index)).String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		helps.LogWithRequestID(ctx).Debugf("%s: dropped invalid %s encrypted_content at input[%d] item_id=%q reason=%s", provider, itemType, index, itemID, reason)
	}
	return updated
}

// stripOpenAIResponsesEncryptedContent removes every encrypted_content field from
// input reasoning/compaction items. Used after upstream rejects a shape-valid but
// non-decryptable blob (typically cross-account sticky miss).
func stripOpenAIResponsesEncryptedContent(body []byte) ([]byte, int) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, 0
	}
	updated := body
	stripped := 0
	for index, item := range input.Array() {
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "reasoning" && itemType != "compaction" {
			continue
		}
		path := fmt.Sprintf("input.%d.encrypted_content", index)
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(updated, path)
		if err != nil {
			continue
		}
		updated = next
		stripped++
	}
	return updated, stripped
}

func isCodexThinkingSignatureInvalid(statusCode int, body []byte) bool {
	code, _, ok := codexStatusErrorClassification(statusCode, body)
	return ok && code == "thinking_signature_invalid"
}
