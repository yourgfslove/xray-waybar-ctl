package subscription

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
	"gopkg.in/yaml.v3"
)

// isClashYAML returns true when body looks like a Clash/Clash.Meta config
// (top-level `proxies:` key present). Conservative on purpose — base64'd
// URI lists never contain that string at column 0.
func isClashYAML(body []byte) bool {
	s := string(body)
	return strings.HasPrefix(s, "proxies:") || strings.Contains(s, "\nproxies:")
}

type clashDoc struct {
	Proxies []clashProxy `yaml:"proxies"`
}

// clashProxy covers the subset of fields we map to server.Server for
// vless / vmess / trojan. Unknown protocols are skipped with an error.
type clashProxy struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	Server         string   `yaml:"server"`
	Port           int      `yaml:"port"`
	UUID           string   `yaml:"uuid"`
	Password       string   `yaml:"password"`
	AlterID        int      `yaml:"alterId"`
	Cipher         string   `yaml:"cipher"`
	Flow           string   `yaml:"flow"`
	Network        string   `yaml:"network"`
	TLS            bool     `yaml:"tls"`
	Servername     string   `yaml:"servername"`
	SNI            string   `yaml:"sni"`
	Fingerprint    string   `yaml:"client-fingerprint"`
	ALPN           []string `yaml:"alpn"`
	SkipCertVerify bool     `yaml:"skip-cert-verify"`

	RealityOpts *clashReality `yaml:"reality-opts"`
	WSOpts      *clashWS      `yaml:"ws-opts"`
	GRPCOpts    *clashGRPC    `yaml:"grpc-opts"`
	XHTTPOpts   *clashXHTTP   `yaml:"xhttp-opts"`
}

type clashReality struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
	SpiderX   string `yaml:"spider-x"`
}

type clashWS struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

type clashGRPC struct {
	ServiceName string `yaml:"grpc-service-name"`
}

type clashXHTTP struct {
	Path    string            `yaml:"path"`
	Host    string            `yaml:"host"`
	Mode    string            `yaml:"mode"`
	Headers map[string]string `yaml:"headers"`
}

// parseClash maps a Clash YAML document into a list of servers. Empty
// `proxies: []` is a legitimate empty subscription, not an error.
func parseClash(body []byte) ([]server.Server, []error) {
	var doc clashDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, []error{fmt.Errorf("clash: yaml: %w", err)}
	}
	var (
		servers []server.Server
		errs    []error
	)
	for i, p := range doc.Proxies {
		s, err := clashToServer(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("proxy %d (%s): %w", i+1, p.Name, err))
			continue
		}
		servers = append(servers, s)
	}
	return servers, errs
}

func clashToServer(p clashProxy) (server.Server, error) {
	if p.Server == "" || p.Port == 0 {
		return server.Server{}, fmt.Errorf("missing server/port")
	}
	s := server.Server{
		Name:          p.Name,
		Protocol:      strings.ToLower(p.Type),
		Address:       p.Server,
		Port:          p.Port,
		Network:       firstNonEmpty(p.Network, "tcp"),
		Flow:          p.Flow,
		SNI:           firstNonEmpty(p.SNI, p.Servername),
		Fingerprint:   p.Fingerprint,
		ALPN:          p.ALPN,
		AllowInsecure: p.SkipCertVerify,
	}
	if p.TLS {
		s.Security = "tls"
	} else {
		s.Security = "none"
	}
	if p.RealityOpts != nil && p.RealityOpts.PublicKey != "" {
		s.Security = "reality"
		s.PublicKey = p.RealityOpts.PublicKey
		s.ShortID = p.RealityOpts.ShortID
		s.SpiderX = p.RealityOpts.SpiderX
	}
	if p.WSOpts != nil {
		s.Path = p.WSOpts.Path
		if h := p.WSOpts.Headers["Host"]; h != "" {
			s.Host = h
		}
	}
	if p.GRPCOpts != nil {
		s.ServiceName = p.GRPCOpts.ServiceName
	}
	if s.Network == "xhttp" || s.Network == "splithttp" {
		s.Network = "xhttp"
		if p.XHTTPOpts != nil {
			s.Path = p.XHTTPOpts.Path
			s.Host = p.XHTTPOpts.Host
			s.XHTTPMode = p.XHTTPOpts.Mode
			if len(p.XHTTPOpts.Headers) > 0 {
				headers, err := json.Marshal(p.XHTTPOpts.Headers)
				if err != nil {
					return server.Server{}, fmt.Errorf("xhttp headers: %w", err)
				}
				s.XHTTPExtra = map[string]json.RawMessage{"headers": headers}
			}
		}
	}

	switch s.Protocol {
	case "vless":
		if p.UUID == "" {
			return server.Server{}, fmt.Errorf("vless: missing uuid")
		}
		s.UUID = p.UUID
		s.Encryption = "none"
	case "vmess":
		if p.UUID == "" {
			return server.Server{}, fmt.Errorf("vmess: missing uuid")
		}
		s.UUID = p.UUID
		s.AlterID = p.AlterID
		s.Encryption = firstNonEmpty(p.Cipher, "auto")
	case "trojan":
		if p.Password == "" {
			return server.Server{}, fmt.Errorf("trojan: missing password")
		}
		s.Password = p.Password
		if !p.TLS {
			// trojan requires TLS in xray; Clash makes it implicit
			s.Security = "tls"
		}
	default:
		return server.Server{}, fmt.Errorf("unsupported type %q", p.Type)
	}

	if s.Name == "" {
		s.Name = s.Endpoint()
	}
	return s, nil
}
