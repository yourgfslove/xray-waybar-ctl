package xrayconfig_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yourgfslove/xray-waybar-ctl/internal/store"
	"github.com/yourgfslove/xray-waybar-ctl/internal/subscription"
	"github.com/yourgfslove/xray-waybar-ctl/internal/xrayconfig"
)

// Exercise the full subscription -> disk cache -> outbound path. A transport
// can parse successfully yet still fail when the tester or connect renders it.
func TestXHTTPSubscriptionToConfig(t *testing.T) {
	const uuid = "00000000-0000-0000-0000-000000000001"
	const extra = `{"noGRPCHeader":true,"xPaddingBytes":"100-200","xmux":{"maxConcurrency":"8-16"},"downloadSettings":{"address":"down.example.com","port":443,"network":"xhttp","security":"tls","tlsSettings":{"serverName":"down.example.com"},"xhttpSettings":{"path":"/down%2Fload","mode":"auto"}}}`
	vmess := func(extraValue any) string {
		raw, err := json.Marshal(map[string]any{
			"add": "vpn.example.com", "port": "443", "id": uuid,
			"net": "xhttp", "tls": "tls", "sni": "vpn.example.com",
			"path": "/vpn", "host": "cdn.example.com", "mode": "stream-up", "extra": extraValue,
		})
		if err != nil {
			t.Fatal(err)
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(raw)
	}
	query := "&path=%2Fvpn&host=cdn.example.com&mode=stream-up&extra=" + url.QueryEscape(extra)
	tests := []struct {
		name, body, security, path, host, mode, extra string
	}{
		{"vless-tls", "vless://" + uuid + "@vpn.example.com:443?type=xhttp&security=tls&sni=vpn.example.com" + query, "tls", "/vpn", "cdn.example.com", "stream-up", extra},
		{"vless-reality", "vless://" + uuid + "@vpn.example.com:443?type=xhttp&security=reality&sni=vpn.example.com&pbk=J_c2B4hmAaiRlwyCjrJgU7DntDiBmULe1FrMbN1uNE0&sid=deadbeef&fp=chrome" + query, "reality", "/vpn", "cdn.example.com", "stream-up", extra},
		{"splithttp-defaults", "vless://" + uuid + "@vpn.example.com:80?type=splithttp", "", "/", "", "auto", ""},
		{"packet-up", "vless://" + uuid + "@vpn.example.com:80?type=xhttp&mode=packet-up", "", "/", "", "packet-up", ""},
		{"stream-one", "vless://" + uuid + "@vpn.example.com:443?type=xhttp&security=tls&mode=stream-one", "tls", "/", "", "stream-one", ""},
		{"trojan", "trojan://password@vpn.example.com:443?type=xhttp&sni=vpn.example.com" + query, "tls", "/vpn", "cdn.example.com", "stream-up", extra},
		{"vmess-object", vmess(json.RawMessage(extra)), "tls", "/vpn", "cdn.example.com", "stream-up", extra},
		{"vmess-string", vmess(extra), "tls", "/vpn", "cdn.example.com", "stream-up", extra},
		{"clash", `proxies:
  - name: XHTTP
    type: vless
    server: vpn.example.com
    port: 443
    uuid: ` + uuid + `
    network: xhttp
    tls: true
    servername: vpn.example.com
    xhttp-opts:
      path: /vpn
      host: cdn.example.com
      mode: stream-up
      headers:
        X-Test: custom
`, "tls", "/vpn", "cdn.example.com", "stream-up", `{"headers":{"X-Test":"custom"}}`},
	}
	xray, _ := exec.LookPath("xray")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Also exercise base64-wrapped subscriptions used by providers.
			body := tt.body
			if tt.name == "vless-reality" {
				body = base64.StdEncoding.EncodeToString([]byte(body))
			}
			servers, errs := subscription.Parse([]byte(body))
			if len(errs) != 0 || len(servers) != 1 {
				t.Fatalf("parse: servers=%d errors=%v", len(servers), errs)
			}
			dir := t.TempDir()
			cachePath := filepath.Join(dir, "cache.json")
			if err := store.SaveCache(cachePath, &store.Cache{Servers: servers}); err != nil {
				t.Fatal(err)
			}
			cache, err := store.LoadCache(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			// The indented cache changes whitespace inside RawMessage values.
			// Compare compact JSON so formatting is not mistaken for data loss.
			before, err := json.Marshal(servers)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(cache.Servers)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("transport options changed during cache round trip")
			}
			raw, err := xrayconfig.Generate(cache.Servers[0], xrayconfig.Options{SocksPort: 1080})
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Outbounds []struct {
					StreamSettings struct {
						Network, Security string
						XHTTPSettings     *struct {
							Path, Host, Mode string
							Extra            json.RawMessage
						}
					}
				}
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			stream := cfg.Outbounds[0].StreamSettings
			xh := stream.XHTTPSettings
			if stream.Network != "xhttp" || stream.Security != tt.security || xh == nil {
				t.Fatalf("incorrect stream settings: %s", raw)
			}
			if xh.Path != tt.path || xh.Host != tt.host || xh.Mode != tt.mode {
				t.Errorf("xhttp settings: %+v", xh)
			}
			if tt.extra != "" {
				var got, want any
				if err := json.Unmarshal(xh.Extra, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(tt.extra), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("extra changed: got %s want %s", xh.Extra, tt.extra)
				}
			} else if len(xh.Extra) != 0 {
				t.Errorf("unexpected extra: %s", xh.Extra)
			}
			if xray == "" {
				t.Log("xray not installed; skipping core configuration validation")
				return
			}
			configPath := filepath.Join(dir, "xray.json")
			if err := os.WriteFile(configPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(ctx, xray, "run", "-test", "-config", configPath).CombinedOutput(); err != nil {
				t.Fatalf("xray rejected config: %v\n%s", err, output)
			}
		})
	}
}

func TestXHTTPRejectsInvalidExtra(t *testing.T) {
	for _, extra := range []string{`{`, `[]`, `null`, `true`, `"text"`, `42`} {
		for _, protocol := range []string{"vless", "trojan"} {
			t.Run(protocol+"/"+extra, func(t *testing.T) {
				bad := protocol + "://id@vpn.example.com:443?type=xhttp&extra=" + url.QueryEscape(extra)
				good := "vless://id@vpn.example.com:443?type=xhttp"
				servers, errs := subscription.Parse([]byte(bad + "\n" + good))
				if len(servers) != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "xhttp extra must be a JSON object") {
					t.Fatalf("expected only the valid server: servers=%d errors=%v", len(servers), errs)
				}
			})
		}
	}
}
