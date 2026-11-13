// Package server defines the domain model shared by subscription parsing,
// xray config generation, the URL tester and the CLI.
package server

import (
	"encoding/json"
	"fmt"
)

// Server is a normalized proxy server record. It is decoupled from any
// specific URI scheme: vless / vmess / trojan URIs are parsed into this
// shape, then rendered into an xray outbound by the xrayconfig package.
type Server struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`

	UUID       string `json:"uuid"`
	Password   string `json:"password,omitempty"` // trojan / shadowsocks
	Encryption string `json:"encryption,omitempty"`
	AlterID    int    `json:"alter_id,omitempty"` // vmess
	Flow       string `json:"flow,omitempty"`

	Network  string `json:"network"`
	Security string `json:"security"`

	SNI           string   `json:"sni,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	ALPN          []string `json:"alpn,omitempty"`
	AllowInsecure bool     `json:"allow_insecure,omitempty"`

	// REALITY
	PublicKey string `json:"public_key,omitempty"`
	ShortID   string `json:"short_id,omitempty"`
	SpiderX   string `json:"spider_x,omitempty"`

	// transport-specific
	Path        string `json:"path,omitempty"`         // ws / http / xhttp
	Host        string `json:"host,omitempty"`         // header Host
	ServiceName string `json:"service_name,omitempty"` // grpc
	HeaderType  string `json:"header_type,omitempty"`  // tcp/none, http
	XHTTPMode   string `json:"xhttp_mode,omitempty"`
	// Preserve arbitrary Xray extra options, including nested downloadSettings,
	// without converting JSON numbers to float64 during subscription parsing.
	XHTTPExtra map[string]json.RawMessage `json:"xhttp_extra,omitempty"`

	Raw string `json:"raw"`
}

// Endpoint returns the address:port pair for logging and tooltip text.
func (s Server) Endpoint() string {
	return fmt.Sprintf("%s:%d", s.Address, s.Port)
}

// DisplayName returns Name, falling back to address:port when empty —
// some subscriptions ship URIs without a fragment.
func (s Server) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Endpoint()
}
