package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// parseOpenAIResponsesMissingStoredItemID extracts the item id from:
// "Item with id 'rs_xxx' not found. Items are not persisted when `store` is set to false..."
func parseOpenAIResponsesMissingStoredItemID(body []byte) string {
	msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if msg == "" {
		msg = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "items are not persisted when") || !strings.Contains(lower, "not found") {
		return ""
	}
	const marker = "item with id '"
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(marker):]
	end := strings.IndexByte(rest, '\'')
	if end <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func isOpenAIResponsesMissingStoredItem(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound && statusCode != http.StatusBadRequest {
		return false
	}
	return parseOpenAIResponsesMissingStoredItemID(body) != ""
}

// stripOpenAIResponsesInputItemsByID removes input entries whose id matches itemID
// (including item_reference rows). Used after store=false upstream 404s.
func stripOpenAIResponsesInputItemsByID(body []byte, itemID string) ([]byte, int) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return body, 0
	}
	return filterOpenAIResponsesInputItems(body, func(item gjson.Result) bool {
		return strings.TrimSpace(item.Get("id").String()) == itemID
	})
}

// stripOpenAIResponsesItemReferences drops item_reference rows. With store=false
// (Codex OAuth) server-side lookups always fail for these references.
func stripOpenAIResponsesItemReferences(body []byte) ([]byte, int) {
	return filterOpenAIResponsesInputItems(body, func(item gjson.Result) bool {
		return strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "item_reference")
	})
}

func filterOpenAIResponsesInputItems(body []byte, drop func(gjson.Result) bool) ([]byte, int) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() || drop == nil {
		return body, 0
	}
	kept := make([]json.RawMessage, 0, len(input.Array()))
	removed := 0
	for _, item := range input.Array() {
		if drop(item) {
			removed++
			continue
		}
		kept = append(kept, json.RawMessage(item.Raw))
	}
	if removed == 0 {
		return body, 0
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return body, 0
	}
	out, err := sjson.SetRawBytes(body, "input", raw)
	if err != nil {
		return body, 0
	}
	return out, removed
}

// prepareCodexResponsesRetryBody rewrites the request after a recoverable upstream
// rejection (missing store=false item, or invalid encrypted_content).
func prepareCodexResponsesRetryBody(requestBody []byte, statusCode int, errBody []byte) (next []byte, reason string, ok bool) {
	if missingID := parseOpenAIResponsesMissingStoredItemID(errBody); missingID != "" &&
		(statusCode == http.StatusNotFound || statusCode == http.StatusBadRequest) {
		stripped, n := stripOpenAIResponsesInputItemsByID(requestBody, missingID)
		if n > 0 {
			return stripped, "missing_stored_item:" + missingID, true
		}
	}
	if isCodexThinkingSignatureInvalid(statusCode, errBody) {
		stripped, n := stripOpenAIResponsesEncryptedContent(requestBody)
		if n > 0 {
			return stripped, "encrypted_content", true
		}
	}
	return requestBody, "", false
}
