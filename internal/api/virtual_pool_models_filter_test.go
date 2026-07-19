package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestFilterVirtualPoolModelsResponseOpenAI(t *testing.T) {
	in := []byte(`{
  "object":"list",
  "data":[
    {"id":"gpt-5.5","object":"model"},
    {"id":"plus/gpt-5.5","object":"model"},
    {"id":"plus/codex-auto-review","object":"model"},
    {"id":"k12/gpt-5.5","object":"model"},
    {"id":"k12/codex-auto-review","object":"model"},
    {"id":"codex-auto-review","object":"model"},
    {"id":"plus/gpt-5.4","object":"model"}
  ]
}`)
	out, ok := filterVirtualPoolModelsResponse(in, "plus")
	if !ok {
		t.Fatal("expected filter ok")
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, out)
	}
	got := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		got = append(got, m.ID)
	}
	want := []string{"gpt-5.5", "codex-auto-review", "gpt-5.4"}
	if len(got) != len(want) {
		t.Fatalf("ids=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids[%d]=%q want %q full=%v", i, got[i], want[i], got)
		}
	}
}

func TestFilterVirtualPoolModelsResponseDropsOtherPools(t *testing.T) {
	in := []byte(`{"object":"list","data":[{"id":"k12/gpt-5.5"},{"id":"gpt-5.5"}]}`)
	out, ok := filterVirtualPoolModelsResponse(in, "plus")
	if !ok {
		t.Fatal("expected filter ok")
	}
	if gjsonGetDataLen(out) != 0 {
		t.Fatalf("plus pool should drop k12-only + bare ambiguous, body=%s", out)
	}
}

func TestIsVirtualPoolModelsListPath(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if !isVirtualPoolModelsListPath(req) {
		t.Fatal("GET /v1/models should match")
	}
	req2, _ := http.NewRequest(http.MethodPost, "/v1/models", nil)
	if isVirtualPoolModelsListPath(req2) {
		t.Fatal("POST should not match")
	}
	req3, _ := http.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	if isVirtualPoolModelsListPath(req3) {
		t.Fatal("chat should not match")
	}
}

func gjsonGetDataLen(body []byte) int {
	var parsed struct {
		Data []any `json:"data"`
	}
	_ = json.Unmarshal(body, &parsed)
	return len(parsed.Data)
}
