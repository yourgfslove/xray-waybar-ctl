package tester

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
)

// Opt in because this test starts real Xray processes and binds loopback
// sockets. No Internet access, subscription, or running VPN is required.
func TestXHTTPTunnel(t *testing.T) {
	if os.Getenv("XRAY_INTEGRATION") != "1" {
		t.Skip("set XRAY_INTEGRATION=1 to test real local XHTTP tunnels")
	}
	xray, err := exec.LookPath("xray")
	if err != nil {
		t.Fatal(err)
	}
	const uuid = "00000000-0000-0000-0000-000000000001"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	// REALITY needs a TLS target. Keep that target local as well.
	tlsTarget := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	tlsTarget.EnableHTTP2 = true
	tlsTarget.StartTLS()
	defer tlsTarget.Close()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	for _, security := range []string{"none", "reality"} {
		t.Run(security, func(t *testing.T) {
			port := unusedTCPPort(t)
			s := server.Server{
				Protocol: "vless", Address: "127.0.0.1", Port: port, UUID: uuid,
				Network: "xhttp", Path: "/integration", Security: security,
			}
			stream := map[string]any{
				"network": "xhttp", "security": security,
				"xhttpSettings": map[string]any{"path": s.Path},
			}
			if security == "reality" {
				s.SNI = "example.com"
				s.Fingerprint = "chrome"
				s.PublicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
				s.ShortID = "0123456789abcdef"
				stream["realitySettings"] = map[string]any{
					"target":      strings.TrimPrefix(tlsTarget.URL, "https://"),
					"serverNames": []string{s.SNI}, "shortIds": []string{s.ShortID},
					"privateKey": base64.RawURLEncoding.EncodeToString(key.Bytes()),
				}
			}
			cfg := map[string]any{
				"log": map[string]any{"loglevel": "debug", "access": "none"},
				"inbounds": []any{map[string]any{
					"listen": "127.0.0.1", "port": port, "protocol": "vless",
					"settings":       map[string]any{"clients": []any{map[string]any{"id": uuid}}, "decryption": "none"},
					"streamSettings": stream,
				}},
				"outbounds": []any{map[string]any{"protocol": "freedom"}},
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			configPath := filepath.Join(dir, "server.json")
			if err := os.WriteFile(configPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "server.log")
			log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, xray, "run", "-config", configPath)
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			if err := waitForPort(ctx, port, 3*time.Second); err != nil {
				logs, _ := os.ReadFile(logPath)
				t.Fatalf("start server: %v\n%s", err, logs)
			}
			results := Test(ctx, []server.Server{s}, Options{
				XrayBin: xray, TestURL: target.URL, Timeout: 5 * time.Second,
				Concurrency: 1, StartPort: unusedTCPPort(t),
			})
			if !results[0].Alive {
				logs, _ := os.ReadFile(logPath)
				t.Fatalf("HTTP through XHTTP/%s failed: %v\n%s", security, results[0].Error, logs)
			}
		})
	}
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
