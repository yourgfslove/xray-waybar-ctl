// Package subscription fetches a subscription body and parses it into
// a list of server.Server entries. The expected wire format is a base64
// blob whose decoded payload is one proxy URI per line.
package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
)

// Parse decodes a raw subscription body (the bytes returned from the
// HTTP endpoint) into a list of servers. Lines that fail to parse are
// skipped; their errors are returned alongside the successful entries.
func Parse(body []byte) ([]server.Server, []error) {
	if isClashYAML(body) {
		return parseClash(body)
	}
	decoded := decodeBase64Loose(string(body))
	lines := strings.Split(string(decoded), "\n")

	var (
		servers []server.Server
		errs    []error
	)
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s, err := ParseURI(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
			continue
		}
		servers = append(servers, s)
	}
	return servers, errs
}

// ParseURI dispatches on the URI scheme. Unsupported schemes produce
// an error so they can be reported up and skipped.
func ParseURI(uri string) (server.Server, error) {
	switch {
	case strings.HasPrefix(uri, "vless://"):
		return parseVLESS(uri)
	case strings.HasPrefix(uri, "vmess://"):
		return parseVMess(uri)
	case strings.HasPrefix(uri, "trojan://"):
		return parseTrojan(uri)
	default:
		scheme := uri
		if i := strings.Index(uri, "://"); i > 0 {
			scheme = uri[:i]
		}
		return server.Server{}, fmt.Errorf("unsupported scheme %q", scheme)
	}
}

func parseVLESS(uri string) (server.Server, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return server.Server{}, fmt.Errorf("vless: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return server.Server{}, errors.New("vless: missing uuid")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return server.Server{}, fmt.Errorf("vless: port: %w", err)
	}
	q := u.Query()
	s := server.Server{
		Protocol:    "vless",
		Address:     u.Hostname(),
		Port:        port,
		UUID:        u.User.Username(),
		Encryption:  firstNonEmpty(q.Get("encryption"), "none"),
		Flow:        q.Get("flow"),
		Network:     firstNonEmpty(q.Get("type"), "tcp"),
		Security:    firstNonEmpty(q.Get("security"), "none"),
		SNI:         q.Get("sni"),
		Fingerprint: q.Get("fp"),
		PublicKey:   q.Get("pbk"),
		ShortID:     q.Get("sid"),
		SpiderX:     q.Get("spx"),
		Path:        q.Get("path"),
		Host:        q.Get("host"),
		ServiceName: q.Get("serviceName"),
		HeaderType:  q.Get("headerType"),
		Raw:         uri,
	}
	if alpn := q.Get("alpn"); alpn != "" {
		s.ALPN = splitCSV(alpn)
	}
	if v := q.Get("allowInsecure"); v == "1" || v == "true" {
		s.AllowInsecure = true
	}
	if err := parseXHTTPOptions(&s, q.Get("mode"), q.Get("extra")); err != nil {
		return server.Server{}, fmt.Errorf("vless: %w", err)
	}
	s.Name = u.Fragment // net/url already URL-decoded it
	if s.Name == "" {
		s.Name = s.Endpoint()
	}
	return s, nil
}

// vmessURI is the JSON body packed inside `vmess://<base64-json>`.
// Some clients use string types for numeric fields, so port/aid use any.
type vmessURI struct {
	V     string          `json:"v"`
	PS    string          `json:"ps"`
	Add   string          `json:"add"`
	Port  any             `json:"port"`
	ID    string          `json:"id"`
	Aid   any             `json:"aid"`
	Scy   string          `json:"scy"`
	Net   string          `json:"net"`
	Type  string          `json:"type"`
	Host  string          `json:"host"`
	Path  string          `json:"path"`
	TLS   string          `json:"tls"`
	SNI   string          `json:"sni"`
	ALPN  string          `json:"alpn"`
	FP    string          `json:"fp"`
	Mode  string          `json:"mode"`
	Extra json.RawMessage `json:"extra"`
}

func parseVMess(uri string) (server.Server, error) {
	encoded := strings.TrimPrefix(uri, "vmess://")
	var fragment string
	if i := strings.IndexByte(encoded, '#'); i >= 0 {
		fragment = encoded[i+1:]
		encoded = encoded[:i]
	}
	jsonBytes := decodeBase64Loose(encoded)
	if len(jsonBytes) == 0 || jsonBytes[0] != '{' {
		return server.Server{}, errors.New("vmess: payload is not JSON")
	}
	var v vmessURI
	if err := json.Unmarshal(jsonBytes, &v); err != nil {
		return server.Server{}, fmt.Errorf("vmess: json: %w", err)
	}
	port, err := anyToInt(v.Port)
	if err != nil {
		return server.Server{}, fmt.Errorf("vmess: port: %w", err)
	}
	aid, _ := anyToInt(v.Aid)
	s := server.Server{
		Protocol:    "vmess",
		Address:     v.Add,
		Port:        port,
		UUID:        v.ID,
		AlterID:     aid,
		Encryption:  firstNonEmpty(v.Scy, "auto"),
		Network:     firstNonEmpty(v.Net, "tcp"),
		Security:    firstNonEmpty(v.TLS, "none"),
		SNI:         v.SNI,
		Fingerprint: v.FP,
		Path:        v.Path,
		Host:        v.Host,
		HeaderType:  v.Type,
		Raw:         uri,
	}
	if v.ALPN != "" {
		s.ALPN = splitCSV(v.ALPN)
	}
	// VMess exporters use either an embedded object or a JSON string for extra.
	extra := string(v.Extra)
	if strings.HasPrefix(extra, `"`) {
		if err := json.Unmarshal(v.Extra, &extra); err != nil {
			return server.Server{}, fmt.Errorf("vmess: xhttp extra: %w", err)
		}
	}
	if err := parseXHTTPOptions(&s, v.Mode, extra); err != nil {
		return server.Server{}, fmt.Errorf("vmess: %w", err)
	}
	decodedFragment, _ := url.QueryUnescape(fragment)
	s.Name = firstNonEmpty(v.PS, decodedFragment, s.Endpoint())
	return s, nil
}

func parseTrojan(uri string) (server.Server, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return server.Server{}, fmt.Errorf("trojan: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return server.Server{}, errors.New("trojan: missing password")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return server.Server{}, fmt.Errorf("trojan: port: %w", err)
	}
	q := u.Query()
	s := server.Server{
		Protocol:    "trojan",
		Address:     u.Hostname(),
		Port:        port,
		Password:    u.User.Username(),
		Network:     firstNonEmpty(q.Get("type"), "tcp"),
		Security:    firstNonEmpty(q.Get("security"), "tls"),
		SNI:         q.Get("sni"),
		Fingerprint: q.Get("fp"),
		Path:        q.Get("path"),
		Host:        q.Get("host"),
		ServiceName: q.Get("serviceName"),
		Raw:         uri,
	}
	if alpn := q.Get("alpn"); alpn != "" {
		s.ALPN = splitCSV(alpn)
	}
	if v := q.Get("allowInsecure"); v == "1" || v == "true" {
		s.AllowInsecure = true
	}
	if err := parseXHTTPOptions(&s, q.Get("mode"), q.Get("extra")); err != nil {
		return server.Server{}, fmt.Errorf("trojan: %w", err)
	}
	s.Name = u.Fragment
	if s.Name == "" {
		s.Name = s.Endpoint()
	}
	return s, nil
}

// Query values have already been URL-decoded by net/url. In particular, do
// not unescape extra a second time: its nested paths may contain literal %.
func parseXHTTPOptions(s *server.Server, mode, extra string) error {
	if s.Network != "xhttp" && s.Network != "splithttp" {
		return nil
	}
	s.Network = "xhttp"
	s.XHTTPMode = mode
	if extra != "" {
		if err := json.Unmarshal([]byte(extra), &s.XHTTPExtra); err != nil {
			return fmt.Errorf("xhttp extra must be a JSON object: %w", err)
		}
		if s.XHTTPExtra == nil {
			return errors.New("xhttp extra must be a JSON object")
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func anyToInt(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.Atoi(x)
	case json.Number:
		i, err := x.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("unexpected numeric type %T", v)
	}
}
