package acp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// MCPVariable is an environment variable or HTTP header supplied by the client.
type MCPVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is a configured stdio or HTTP server. Unknown transport payloads
// are retained verbatim and ignored, as required by ACP extensibility.
type MCPServer struct {
	Type    string        `json:"type"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []MCPVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []MCPVariable `json:"headers,omitempty"`
	raw     json.RawMessage
}

// UnmarshalJSON decodes only fields belonging to a known variant.
func (s *MCPServer) UnmarshalJSON(b []byte) error {
	var tag struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &tag); err != nil {
		return err
	}
	*s = MCPServer{Type: tag.Type, raw: append(json.RawMessage(nil), b...)}
	switch s.Type {
	case "stdio":
		var v struct {
			Name, Command string
			Args          []string
			Env           []MCPVariable
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		s.Name, s.Command, s.Args, s.Env = v.Name, v.Command, v.Args, v.Env
	case "http":
		var v struct {
			Name, URL string
			Headers   []MCPVariable
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		s.Name, s.URL, s.Headers = v.Name, v.URL, v.Headers
	}
	return nil
}

// MarshalJSON preserves setup extensions when forwarding a parsed value.
func (s MCPServer) MarshalJSON() ([]byte, error) {
	if s.raw != nil {
		return s.raw, nil
	}
	type plain MCPServer
	return jsonrpc.Marshal(plain(s))
}

func validateMCPServers(servers []MCPServer) error {
	names := map[string]bool{}
	for _, s := range servers {
		if s.Type == "" {
			return fmt.Errorf("mcpServers.type is required")
		}
		if s.Type != "stdio" && s.Type != "http" {
			continue
		}
		if s.Name == "" || names[s.Name] {
			return fmt.Errorf("MCP server names must be nonempty and unique")
		}
		names[s.Name] = true
		if len(names) > 16 {
			return fmt.Errorf("at most 16 MCP servers are supported")
		}
		if s.Type == "stdio" {
			if err := requireAbsolute("MCP command", s.Command); err != nil {
				return err
			}
			for _, a := range append([]string{s.Command}, s.Args...) {
				if strings.ContainsRune(a, 0) {
					return fmt.Errorf("MCP command/args contain NUL")
				}
			}
			for _, v := range s.Env {
				if v.Name == "" || strings.ContainsAny(v.Name, "=\x00") || strings.ContainsRune(v.Value, 0) {
					return fmt.Errorf("invalid MCP environment variable")
				}
			}
		} else {
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
				return fmt.Errorf("MCP URL must be an absolute HTTP(S) URL without userinfo or fragment")
			}
			for _, h := range s.Headers {
				if !validHeaderName(h.Name) || strings.ContainsAny(h.Value, "\r\n\x00") {
					return fmt.Errorf("invalid MCP HTTP header")
				}
				switch http.CanonicalHeaderKey(h.Name) {
				case "Host", "Accept", "Content-Type", "Content-Length", "Transfer-Encoding", "Connection", "Mcp-Session-Id", "Mcp-Protocol-Version":
					return fmt.Errorf("MCP protocol headers cannot be overridden")
				}
			}
		}
	}
	return nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

// MCPCapabilities advertises only implemented MCP transports.
type MCPCapabilities struct {
	Stdio *Supported `json:"stdio,omitempty"`
	HTTP  *Supported `json:"http,omitempty"`
}
