package xrayconfig

import (
	"encoding/json"
	"fmt"

	"github.com/yourgfslove/xray-waybar-ctl/internal/routing"
	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
)

// Options controls the generated xray.json.
//
//   - SocksPort is required.
//   - LogLevel defaults to "warning" when empty.
//   - Rules, when non-nil and non-empty, is materialised into
//     routing.rules + an optional dns block. nil / empty → proxy-all
//     (no rules, every connection goes to the proxy outbound).
//   - ForceAllDirect, when true, overrides Rules entirely and routes
//     every connection through the direct outbound. Used by the
//     "direct" built-in profile.
//   - SOMark, when non-zero, stamps every outbound's sockets with
//     SO_MARK=SOMark. Pairs with an `ip rule fwmark … lookup main`
//     installed by the TUN unit: without it, xray's direct/freedom
//     outbound dials its destination through the default route, which
//     in system_wide mode is tun0 → tun2socks → socks-in, and forms a
//     feedback loop that pegs CPU and OOMKills tun2socks. Leave at 0
//     when system_wide is off — the mark is harmless but pointless.
//
// The tester always passes nil Rules + ForceAllDirect=false so missing
// geoip.dat / rules.json never breaks a benchmark.
type Options struct {
	SocksPort      int
	LogLevel       string
	Rules          *routing.Rules
	ForceAllDirect bool
	SOMark         int
}

// Generate renders a complete xray-core config that routes a local
// SOCKS5 listener through the given upstream server.
//
// The output is deterministic, indented JSON suitable for writing to
// disk and for diff-friendly inspection.
func Generate(s server.Server, opts Options) ([]byte, error) {
	if opts.SocksPort == 0 {
		return nil, fmt.Errorf("xrayconfig: SocksPort is required")
	}
	if opts.LogLevel == "" {
		opts.LogLevel = "warning"
	}

	proxy, err := buildProxyOutbound(s)
	if err != nil {
		return nil, err
	}
	if opts.SOMark != 0 {
		if proxy.StreamSettings == nil {
			proxy.StreamSettings = &StreamSettings{}
		}
		proxy.StreamSettings.Sockopt = &Sockopt{Mark: opts.SOMark}
	}
	socksSettings, _ := json.Marshal(socksInboundSettings{Auth: "noauth", UDP: true})

	rules, domainStrategy, dns := renderRouting(opts)

	out := struct {
		Log       LogCfg     `json:"log"`
		DNS       *DNSCfg    `json:"dns,omitempty"`
		Inbounds  []Inbound  `json:"inbounds"`
		Outbounds []Outbound `json:"outbounds"`
		Routing   Routing    `json:"routing"`
	}{
		Log: LogCfg{Loglevel: opts.LogLevel, Access: "none"},
		DNS: dns,
		Inbounds: []Inbound{{
			Tag:      "socks-in",
			Listen:   "127.0.0.1",
			Port:     opts.SocksPort,
			Protocol: "socks",
			Settings: socksSettings,
			Sniffing: &Sniffing{Enabled: true, DestOverride: []string{"http", "tls"}},
		}},
		Outbounds: []Outbound{
			proxy,
			{Tag: "direct", Protocol: "freedom", StreamSettings: sockoptStream(opts.SOMark)},
			{Tag: "block", Protocol: "blackhole", StreamSettings: sockoptStream(opts.SOMark)},
		},
		Routing: Routing{
			DomainStrategy: domainStrategy,
			Rules:          rules,
		},
	}

	return json.MarshalIndent(out, "", "  ")
}

// renderRouting turns Options into the three pieces Generate plugs in:
// the rules list, the domainStrategy, and (optionally) a DNS block.
//
// Order of consideration:
//  1. ForceAllDirect → single catch-all "direct" rule, no DNS.
//  2. Rules.HasRules() → emit block/proxy/direct rules in RouteOrder.
//     DomainStrategy mirrors the rules-file value when set; otherwise
//     IPIfNonMatch is chosen if there are IP rules, AsIs otherwise.
//     A DNS block is emitted only when IPIfNonMatch is active or the
//     rules file declares Hosts.
//  3. Otherwise → empty rules (proxy-all), no DNS.
func renderRouting(opts Options) ([]RoutingRule, string, *DNSCfg) {
	if opts.ForceAllDirect {
		return []RoutingRule{
			{Type: "field", OutboundTag: "direct", Network: "tcp,udp"},
		}, "AsIs", nil
	}

	r := opts.Rules
	if !r.HasRules() {
		// Cover both nil and an all-empty rules file. xray defaults to
		// the first outbound (proxy) for unmatched traffic, which is
		// exactly what proxy-all wants.
		if r != nil && len(r.Hosts) > 0 {
			return nil, "AsIs", &DNSCfg{Servers: defaultDNSServers(r), Hosts: r.Hosts}
		}
		return []RoutingRule{}, "AsIs", nil
	}

	// Order: block first, then either proxy-then-direct or
	// direct-then-proxy depending on rules.RouteOrder. xray applies the
	// first matching rule, so the order is meaningful.
	var rules []RoutingRule
	rules = appendRule(rules, "block", r.BlockDomains, r.BlockIPs)
	if r.RouteOrder == "block-direct-proxy" {
		rules = appendRule(rules, "direct", r.DirectDomains, r.DirectIPs)
		rules = appendRule(rules, "proxy", r.ProxyDomains, r.ProxyIPs)
	} else {
		// Default and upstream HAPP convention: block, proxy, direct.
		rules = appendRule(rules, "proxy", r.ProxyDomains, r.ProxyIPs)
		rules = appendRule(rules, "direct", r.DirectDomains, r.DirectIPs)
	}

	strategy := r.DomainStrategy
	if strategy == "" {
		if r.HasIPRules() {
			strategy = "IPIfNonMatch"
		} else {
			strategy = "AsIs"
		}
	}

	var dns *DNSCfg
	if strategy == "IPIfNonMatch" || strategy == "IPOnDemand" || len(r.Hosts) > 0 {
		dns = &DNSCfg{Servers: defaultDNSServers(r), Hosts: r.Hosts}
	}
	return rules, strategy, dns
}

// appendRule packs domain+ip lists for one outbound tag into the
// routing.rules slice. xray accepts both fields on the same rule, but
// keeps semantics cleaner if we emit one rule per (tag, kind) pair so
// missing categories surface clearly in the rendered xray.json.
func appendRule(rules []RoutingRule, tag string, domains, ips []string) []RoutingRule {
	if len(domains) > 0 {
		rules = append(rules, RoutingRule{Type: "field", OutboundTag: tag, Domain: domains})
	}
	if len(ips) > 0 {
		rules = append(rules, RoutingRule{Type: "field", OutboundTag: tag, IP: ips})
	}
	return rules
}

// defaultDNSServers builds the dns.servers list. If the rules file
// names a Domestic DNS we expose it as the trusted resolver for the
// direct-side domains; otherwise plain Cloudflare + Google are used.
// Order matters — xray queries them in sequence.
func defaultDNSServers(r *routing.Rules) []any {
	var servers []any

	// Domestic DNS handles the direct-side domain set. Without that
	// scoping it would also resolve proxied domains, which leaks the
	// query through the local ISP. Empty DirectDomains → fall through
	// to the bare-IP entry below.
	if r != nil && r.DomesticDNS != "" && len(r.DirectDomains) > 0 {
		servers = append(servers, map[string]any{
			"address": r.DomesticDNS,
			"domains": r.DirectDomains,
		})
	}
	switch {
	case r != nil && r.RemoteDNS != "":
		servers = append(servers, r.RemoteDNS)
	default:
		servers = append(servers, "1.1.1.1")
	}
	return servers
}

func buildProxyOutbound(s server.Server) (Outbound, error) {
	if s.Address == "" || s.Port == 0 {
		return Outbound{}, fmt.Errorf("xrayconfig: server is missing address/port")
	}

	var settings json.RawMessage
	switch s.Protocol {
	case "vless":
		if s.UUID == "" {
			return Outbound{}, fmt.Errorf("xrayconfig: vless: missing uuid")
		}
		raw, err := json.Marshal(vlessSettings{Vnext: []vlessVnext{{
			Address: s.Address,
			Port:    s.Port,
			Users: []vlessUser{{
				ID:         s.UUID,
				Encryption: defaultStr(s.Encryption, "none"),
				Flow:       s.Flow,
			}},
		}}})
		if err != nil {
			return Outbound{}, err
		}
		settings = raw

	case "vmess":
		if s.UUID == "" {
			return Outbound{}, fmt.Errorf("xrayconfig: vmess: missing uuid")
		}
		raw, err := json.Marshal(vmessSettings{Vnext: []vmessVnext{{
			Address: s.Address,
			Port:    s.Port,
			Users: []vmessUser{{
				ID:       s.UUID,
				AlterID:  s.AlterID,
				Security: defaultStr(s.Encryption, "auto"),
			}},
		}}})
		if err != nil {
			return Outbound{}, err
		}
		settings = raw

	case "trojan":
		if s.Password == "" {
			return Outbound{}, fmt.Errorf("xrayconfig: trojan: missing password")
		}
		raw, err := json.Marshal(trojanSettings{Servers: []trojanServer{{
			Address:  s.Address,
			Port:     s.Port,
			Password: s.Password,
		}}})
		if err != nil {
			return Outbound{}, err
		}
		settings = raw

	default:
		return Outbound{}, fmt.Errorf("xrayconfig: unsupported protocol %q", s.Protocol)
	}

	stream, err := buildStreamSettings(s)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{
		Tag:            "proxy",
		Protocol:       s.Protocol,
		Settings:       settings,
		StreamSettings: stream,
	}, nil
}

func buildStreamSettings(s server.Server) (*StreamSettings, error) {
	ss := &StreamSettings{
		Network:  defaultStr(s.Network, "tcp"),
		Security: s.Security,
	}
	if ss.Security == "none" {
		ss.Security = ""
	}

	switch ss.Security {
	case "tls":
		ss.TLSSettings = &TLSSettings{
			ServerName:    s.SNI,
			Fingerprint:   s.Fingerprint,
			ALPN:          s.ALPN,
			AllowInsecure: s.AllowInsecure,
		}
	case "reality":
		if s.PublicKey == "" {
			return nil, fmt.Errorf("xrayconfig: reality: missing publicKey (pbk)")
		}
		ss.RealitySettings = &RealitySettings{
			ServerName:  s.SNI,
			Fingerprint: defaultStr(s.Fingerprint, "chrome"),
			PublicKey:   s.PublicKey,
			ShortID:     s.ShortID,
			SpiderX:     s.SpiderX,
		}
	case "", "none":
		// no transport security
	default:
		return nil, fmt.Errorf("xrayconfig: unsupported security %q", ss.Security)
	}

	switch ss.Network {
	case "tcp":
		if s.HeaderType == "http" {
			hdr := map[string]any{
				"type": "http",
				"request": map[string]any{
					"path":    []string{defaultStr(s.Path, "/")},
					"headers": map[string][]string{"Host": {defaultStr(s.Host, s.SNI)}},
				},
			}
			raw, _ := json.Marshal(hdr)
			ss.TCPSettings = &TCPSettings{Header: raw}
		}
	case "ws":
		ws := &WSSettings{Path: defaultStr(s.Path, "/")}
		if s.Host != "" {
			ws.Headers = map[string]string{"Host": s.Host}
		}
		ss.WSSettings = ws
	case "grpc":
		ss.GRPCSettings = &GRPCSettings{ServiceName: s.ServiceName}
	case "xhttp", "splithttp":
		ss.Network = "xhttp"
		ss.XHTTPSettings = &XHTTPSettings{
			Path:  defaultStr(s.Path, "/"),
			Host:  s.Host,
			Mode:  defaultStr(s.XHTTPMode, "auto"),
			Extra: s.XHTTPExtra,
		}
	default:
		return nil, fmt.Errorf("xrayconfig: unsupported network %q", ss.Network)
	}

	return ss, nil
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// sockoptStream wraps SO_MARK into a minimal StreamSettings for outbounds
// (direct, block) that otherwise need no streamSettings. Returns nil for
// mark=0 so the JSON stays clean when system_wide is off.
func sockoptStream(mark int) *StreamSettings {
	if mark == 0 {
		return nil
	}
	return &StreamSettings{Sockopt: &Sockopt{Mark: mark}}
}
