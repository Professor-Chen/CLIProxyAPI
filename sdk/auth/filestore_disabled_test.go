package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testTokenStorage struct {
	meta map[string]any
}

func (s *testTokenStorage) SetMetadata(meta map[string]any) { s.meta = meta }

func (s *testTokenStorage) SaveTokenToFile(authFilePath string) error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, raw, 0o600)
}

func TestFileTokenStore_Save_DisabledPersistsFlagForTokenStorage(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled.json")

	if err := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	storage := &testTokenStorage{}

	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
}

func TestFileTokenStore_Save_TokenStorageSkipsUnchangedTargetWrite(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	auth := &cliproxyauth.Auth{
		ID:       "same.json",
		Provider: "test",
		FileName: "same.json",
		Storage:  &testTokenStorage{},
		Metadata: map[string]any{"type": "test", "access_token": "token"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}
	path := filepath.Join(baseDir, "same.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth file before second save: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth file after second save: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged token save updated auth file mtime: before=%s after=%s", before.ModTime(), after.ModTime())
	}

	auth.Metadata["access_token"] = "new-token"
	time.Sleep(20 * time.Millisecond)
	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("changed Save() error: %v", err)
	}
	changed, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat auth file after changed save: %v", err)
	}
	if !changed.ModTime().After(after.ModTime()) {
		t.Fatalf("changed token save did not update auth file mtime: unchanged=%s changed=%s", after.ModTime(), changed.ModTime())
	}
}
