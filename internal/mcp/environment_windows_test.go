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
