// Package xrayconfig renders a server.Server into the on-disk xray
// JSON configuration. The shapes mirror the relevant parts of the
// xray-core schema; only fields we actually emit are modelled.
package xrayconfig

import "encoding/json"

type LogCfg struct {
	Loglevel string `json:"loglevel"`
	// Access "none" disables the per-connection access log. xray-core
	// writes it by default at warning level too, and on a system-wide
	// TUN setup a normal browsing session produces ~MB/min of churn
	// (every TCP connect from every page becomes a line).
	Access string `json:"access,omitempty"`
}

type Inbound struct {
	Tag      string          `json:"tag"`
	Listen   string          `json:"listen,omitempty"`
	Port     int             `json:"port"`
	Protocol string          `json:"protocol"`
	Settings json.RawMessage `json:"settings,omitempty"`
	Sniffing *Sniffing       `json:"sniffing,omitempty"`
}

type Sniffing struct {
	Enabled      bool     `json:"enabled"`
	DestOverride []string `json:"destOverride,omitempty"`
}

type Routing struct {
	DomainStrategy string        `json:"domainStrategy,omitempty"`
	Rules          []RoutingRule `json:"rules"`
}

type RoutingRule struct {
	Type        string   `json:"type,omitempty"`
	OutboundTag string   `json:"outboundTag"`
	IP          []string `json:"ip,omitempty"`
	Domain      []string `json:"domain,omitempty"`
	Network     string   `json:"network,omitempty"`
}

// DNSCfg is the xray DNS block. Emitted only when the active rules
// require IPIfNonMatch resolution (any IP-side rule), or when the
// rules ship explicit Hosts overrides. xray's built-in resolver
// handles the no-rules case fine.
type DNSCfg struct {
	Servers []any             `json:"servers"`
	Hosts   map[string]string `json:"hosts,omitempty"`
	Tag     string            `json:"tag,omitempty"`
}

type Outbound struct {
	Tag            string          `json:"tag"`
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	StreamSettings *StreamSettings `json:"streamSettings,omitempty"`
}

type StreamSettings struct {
	Network         string           `json:"network,omitempty"`
	Security        string           `json:"security,omitempty"`
	TLSSettings     *TLSSettings     `json:"tlsSettings,omitempty"`
	RealitySettings *RealitySettings `json:"realitySettings,omitempty"`
	TCPSettings     *TCPSettings     `json:"tcpSettings,omitempty"`
	WSSettings      *WSSettings      `json:"wsSettings,omitempty"`
	GRPCSettings    *GRPCSettings    `json:"grpcSettings,omitempty"`
	XHTTPSettings   *XHTTPSettings   `json:"xhttpSettings,omitempty"`
	Sockopt         *Sockopt         `json:"sockopt,omitempty"`
}

// Sockopt carries socket-level options applied to every connection an
// outbound opens. We only use SO_MARK today: in system_wide mode every
// outbound (proxy, direct, block) marks its packets so an `ip rule
// fwmark … lookup main` skips them past tun0 and prevents the loop
// where xray's own direct/freedom outbound feeds back through tun2socks
// into socks-in.
type Sockopt struct {
	Mark int `json:"mark,omitempty"`
}

type TLSSettings struct {
	ServerName    string   `json:"serverName,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	ALPN          []string `json:"alpn,omitempty"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
}

type RealitySettings struct {
	ServerName  string `json:"serverName"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"publicKey"`
	ShortID     string `json:"shortId,omitempty"`
	SpiderX     string `json:"spiderX,omitempty"`
}

type TCPSettings struct {
	Header json.RawMessage `json:"header,omitempty"`
}

type WSSettings struct {
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type GRPCSettings struct {
	ServiceName string `json:"serviceName,omitempty"`
}

type XHTTPSettings struct {
	Path  string                     `json:"path"`
	Host  string                     `json:"host,omitempty"`
	Mode  string                     `json:"mode"`
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// VLESS

type vlessSettings struct {
	Vnext []vlessVnext `json:"vnext"`
}

type vlessVnext struct {
	Address string      `json:"address"`
	Port    int         `json:"port"`
	Users   []vlessUser `json:"users"`
}

type vlessUser struct {
	ID         string `json:"id"`
	Encryption string `json:"encryption"`
	Flow       string `json:"flow,omitempty"`
}

// VMess

type vmessSettings struct {
	Vnext []vmessVnext `json:"vnext"`
}

type vmessVnext struct {
	Address string      `json:"address"`
	Port    int         `json:"port"`
	Users   []vmessUser `json:"users"`
}

type vmessUser struct {
	ID       string `json:"id"`
	AlterID  int    `json:"alterId"`
	Security string `json:"security,omitempty"`
}

// Trojan

type trojanSettings struct {
	Servers []trojanServer `json:"servers"`
}

type trojanServer struct {
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

// socksInboundSettings is the inbound shape for a local SOCKS5 listener.
type socksInboundSettings struct {
	Auth string `json:"auth"`
	UDP  bool   `json:"udp"`
}
