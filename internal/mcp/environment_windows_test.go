package mcp

import (
	"strings"
	"testing"
)

func TestWindowsEnvironmentOverridesCaseInsensitive(t *testing.T) {
	t.Setenv("Path", "inherited-path")
	t.Setenv("Acplause_Api_Key", "must-not-inherit")
	env := processEnvironment(map[string]string{"pAtH": "configured-path", "SystemRoot": "configured-root"})
	counts := map[string]int{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		counts[key]++
		if key == "PATH" && value != "configured-path" || key == "SYSTEMROOT" && value != "configured-root" {
			t.Errorf("explicit environment entry lost: %s", key)
		}
		if key == "ACPLAUSE_API_KEY" {
			t.Error("provider credential inherited")
		}
	}
	if counts["PATH"] != 1 || counts["SYSTEMROOT"] != 1 {
		t.Fatalf("case aliases were not collapsed: %v", counts)
	}
}

func TestWindowsEnvironmentInheritsAllowlistCaseInsensitively(t *testing.T) {
	t.Setenv("Https_Proxy", "proxy")
	t.Setenv("xdg_config_home", "xdg")
	t.Setenv("Ssl_Cert_File", "ca")
	t.Setenv("Aws_Secret_Access_Key", "must-not-inherit")
	got := map[string]string{}
	for _, entry := range processEnvironment(nil) {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}
	for key, want := range map[string]string{"HTTPS_PROXY": "proxy", "XDG_CONFIG_HOME": "xdg", "SSL_CERT_FILE": "ca"} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("unrelated secret inherited")
	}
}
