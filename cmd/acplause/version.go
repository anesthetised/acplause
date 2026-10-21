package main

import (
	"cmp"
	"net/http"
	"runtime/debug"
)

// Release builders stamp the full tag (including v) and commit even when
// compiling an archive without .git, as the Homebrew formula does.
var buildVersion, buildCommit string

func buildIdentity(v, commit string, info *debug.BuildInfo) (string, string) {
	if info != nil {
		if v == "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		if commit == "" {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					commit = setting.Value
				}
			}
		}
	}
	return cmp.Or(v, "dev"), commit
}

func identity() (string, string) {
	info, _ := debug.ReadBuildInfo()
	return buildIdentity(buildVersion, buildCommit, info)
}

func version() string {
	v, _ := identity()
	return v
}

func versionLabel() string {
	v, commit := identity()
	label := "acplause " + v
	if commit != "" {
		label += " (commit " + commit + ")"
	}
	return label
}

// providerHeaders keeps normal turns and online diagnostics identifiable by
// the same build, without splitting OpenRouter attribution by release.
func providerHeaders() http.Header {
	return http.Header{
		"User-Agent":         {"acplause/" + version()},
		"Http-Referer":       {"https://github.com/anesthetised/acplause"},
		"X-Openrouter-Title": {"acplause"},
	}
}
