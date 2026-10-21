package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, stampedVersion, stampedCommit string
		info                                *debug.BuildInfo
		version, commit                     string
	}{
		{"unknown", "", "", nil, "dev", ""},
		{"development", "", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev", ""},
		{"go install", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.202610.0"}}, "v0.202610.0", ""},
		{"checkout", "", "", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}}}, "dev", "abc123"},
		{"release overrides metadata", "v0.202610.2", "def456", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-old"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}}}, "v0.202610.2", "def456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, c := buildIdentity(tc.stampedVersion, tc.stampedCommit, tc.info)
			if v != tc.version || c != tc.commit {
				t.Fatalf("got %q, %q", v, c)
			}
		})
	}
}

func TestVersionDispatchDoesNotReadConfigOrServe(t *testing.T) {
	var report bytes.Buffer
	err := dispatch([]string{"--version"}, func(string) string { t.Fatal("read configuration"); return "" }, &report, func() error { t.Fatal("entered ACP server"); return nil })
	if err != nil || !strings.HasPrefix(report.String(), "acplause "+version()) {
		t.Fatalf("report=%q err=%v", report.String(), err)
	}
	if err := dispatch([]string{"--version", "extra"}, nil, &report, nil); err == nil {
		t.Fatal("accepted extra argument")
	}
}

func TestProviderHeadersIdentifyBuild(t *testing.T) {
	h := providerHeaders()
	if h.Get("User-Agent") != "acplause/"+version() || h.Get("X-OpenRouter-Title") != "acplause" || h.Get("HTTP-Referer") != "https://github.com/anesthetised/acplause" {
		t.Fatalf("headers=%v", h)
	}
}
