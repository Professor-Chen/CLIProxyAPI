package api

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Reproduces the production bug: reloading virtual-pool-listeners while
// tls.enable=false must keep listeners on plain HTTP. Passing a polluted
// empty s.server.TLSConfig into startVirtualPoolListeners creates
// "tls: no certificates configured" TLS sockets that break HTTP clients.
func TestReloadVirtualPoolListenersStayPlainWhenTLSDisabled(t *testing.T) {
	mainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen main: %v", err)
	}
	defer mainLn.Close()
	mainPort := mainLn.Addr().(*net.TCPAddr).Port

	proPort := freeTCPPort(t)
	k12Port := freeTCPPort(t)

	server := newTestServer(t)
	server.cfg.Host = "127.0.0.1"
	server.cfg.Port = mainPort
	server.cfg.TLS.Enable = false
	server.cfg.TLS.Cert = "/nonexistent/vpool.crt"
	server.cfg.TLS.Key = "/nonexistent/vpool.key"
	server.cfg.VirtualPoolListeners = []proxyconfig.VirtualPoolListener{
		{Name: "pro", Port: proPort, Prefix: "pro"},
	}
	server.server.Addr = fmt.Sprintf("127.0.0.1:%d", mainPort)
	// Polluted empty TLSConfig — mirrors the production failure mode where
	// reload wraps listeners in TLS without certificates.
	server.server.TLSConfig = &tls.Config{}

	if err := server.startVirtualPoolListeners(nil); err != nil {
		t.Fatalf("initial start with nil tls: %v", err)
	}
	t.Cleanup(func() {
		server.virtualPoolMu.Lock()
		defer server.virtualPoolMu.Unlock()
		server.closeVirtualPoolServersLocked()
	})

	time.Sleep(50 * time.Millisecond)
	assertPlainHTTP(t, proPort, "initial pro")

	oldCfg := server.cfg.CloneForRuntime()
	server.cfg.VirtualPoolListeners = []proxyconfig.VirtualPoolListener{
		{Name: "pro", Port: proPort, Prefix: "pro"},
		{Name: "k12", Port: k12Port, Prefix: "k12"},
	}
	server.reloadVirtualPoolListenersIfNeeded(oldCfg, server.cfg)
	time.Sleep(50 * time.Millisecond)

	assertPlainHTTP(t, proPort, "reloaded pro")
	assertPlainHTTP(t, k12Port, "reloaded k12")
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func assertPlainHTTP(t *testing.T, port int, label string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		t.Fatalf("%s: GET error = %v", label, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	if strings.Contains(strings.ToLower(string(body)), "http request to an https server") {
		t.Fatalf("%s: got HTTPS mismatch body %q (listener is TLS, want plain HTTP)", label, string(body))
	}
}
