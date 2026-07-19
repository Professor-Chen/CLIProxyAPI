package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestStartNonStreamingKeepAlive_FlushesHeadersImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Header("Content-Type", "application/json")

	h := &BaseAPIHandler{
		Cfg: &sdkconfig.SDKConfig{
			NonStreamKeepAliveInterval: 30,
		},
	}

	stop := h.StartNonStreamingKeepAlive(c, context.Background())
	defer stop()

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected immediate status 200, got %d", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json content-type, got %q", ct)
	}
	if !c.Writer.Written() {
		t.Fatal("expected response headers/body to be committed immediately")
	}
	if body := recorder.Body.String(); body != "\n" {
		t.Fatalf("expected immediate leading newline keepalive byte, got %q", body)
	}
}

func TestStartNonStreamingKeepAlive_DisabledIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h := &BaseAPIHandler{
		Cfg: &sdkconfig.SDKConfig{
			NonStreamKeepAliveInterval: 0,
		},
	}

	stop := h.StartNonStreamingKeepAlive(c, context.Background())
	stop()

	if c.Writer.Written() {
		t.Fatal("disabled keepalive must not commit the response")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("disabled keepalive must not write body, got %q", recorder.Body.String())
	}
}

func TestStartNonStreamingKeepAlive_PreservesJSONWithLeadingWhitespace(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Header("Content-Type", "application/json")

	h := &BaseAPIHandler{
		Cfg: &sdkconfig.SDKConfig{
			NonStreamKeepAliveInterval: 60,
		},
	}

	stop := h.StartNonStreamingKeepAlive(c, context.Background())
	payload := []byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[]}`)
	_, _ = c.Writer.Write(payload)
	stop()

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("leading keepalive whitespace must remain valid JSON: %v body=%q", err, recorder.Body.String())
	}
	if decoded["id"] != "chatcmpl-test" {
		t.Fatalf("unexpected decoded id: %#v", decoded["id"])
	}
}

func TestStartNonStreamingKeepAlive_EmitsPeriodicBlankLines(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Header("Content-Type", "application/json")

	h := &BaseAPIHandler{
		Cfg: &sdkconfig.SDKConfig{
			NonStreamKeepAliveInterval: 1,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := h.StartNonStreamingKeepAlive(c, ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(recorder.Body.String(), "\n") >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()

	if got := strings.Count(recorder.Body.String(), "\n"); got < 2 {
		t.Fatalf("expected at least one periodic keepalive after the immediate flush, got %d newlines in %q", got, recorder.Body.String())
	}
}
