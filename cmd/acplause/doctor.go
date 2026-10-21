package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/anesthetised/acplause/internal/llm"
)

// dispatch decides before server setup, so diagnostics cannot open the live
// store or read protocol input. Only explicit online diagnostics load models.
// Supplied arguments are never echoed.
func dispatch(args []string, getenv func(string) string, stderr io.Writer, serve func() error) error {
	if len(args) == 0 {
		return serve()
	}
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(stderr, versionLabel())
		return err
	}
	online := len(args) == 2 && args[1] == "--online"
	if args[0] == "doctor" && (len(args) == 1 || online) {
		return doctor(getenv, stderr, os.UserHomeDir, os.CreateTemp, online)
	}
	return errors.New("usage: acplause [--version | doctor [--online]]")
}

// dataDirectory is shared by serving and diagnostics; only the caller decides
// whether to open a store or probe the resulting directory.
func dataDirectory(configured string, home func() (string, error)) (string, error) {
	if configured != "" {
		return configured, nil
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("finding the data directory: %w; set ACPLAUSE_DATA_DIR", err)
	}
	return filepath.Join(dir, ".acplause"), nil
}

// doctor emits only constant categories and trusted setting names, never a raw
// parsing/filesystem error. The temp-file seam allows deterministic I/O failure
// tests without relying on permissions that privileged users can bypass.
func doctor(getenv func(string) string, stderr io.Writer, home func() (string, error), createTemp func(string, string) (*os.File, error), online bool) error {
	fmt.Fprintln(stderr, versionLabel())
	cfg, err := loadConfig(getenv)
	failed := false
	report := func(name, status, reason string) {
		fmt.Fprintf(stderr, "%s: %s (%s)\n", name, status, reason)
		failed = failed || status == "FAIL"
	}
	if err != nil {
		// Reuse the parser for individual settings instead of trying to redact
		// arbitrary error text. These are names owned by this binary, not inputs.
		var invalid []string
		for _, key := range []string{"ACPLAUSE_READ_CONCURRENCY", "ACPLAUSE_MAX_TURN_TOKENS", "ACPLAUSE_MAX_TURN_COST_USD", "ACPLAUSE_TURN_TIMEOUT", "ACPLAUSE_MCP_TIMEOUT", "ACPLAUSE_MAX_ROUNDS", "ACPLAUSE_BASH_TIMEOUT", "ACPLAUSE_LOG_LEVEL"} {
			_, bad := loadConfig(func(name string) string {
				if name == key {
					return getenv(name)
				}
				return ""
			})
			if bad != nil {
				invalid = append(invalid, key)
			}
		}
		reason := "invalid settings; review configuration syntax"
		if len(invalid) > 0 {
			reason = "invalid syntax: " + strings.Join(invalid, ", ")
		}
		report("configuration", "FAIL", reason)
	} else {
		report("configuration", "PASS", "syntax valid")
	}
	if validAPIURL(cfg.BaseURL) {
		report("API URL", "PASS", "HTTP(S) API root structure valid")
	} else {
		report("API URL", "FAIL", "use an absolute HTTP(S) API root with a host and no userinfo, query or fragment")
	}
	if cfg.APIKey != "" {
		report("API key", "PASS", "set; redacted")
	} else {
		report("API key", "INFO", "not set; compatible local providers may not require one")
	}
	dir, err := dataDirectory(cfg.DataDir, home)
	if err != nil {
		report("data directory", "FAIL", "default location unavailable; set ACPLAUSE_DATA_DIR")
	} else {
		created, reason := probeDataDirectory(dir, createTemp)
		status := "PASS"
		if reason != "" {
			status = "FAIL"
		} else {
			reason = "temporary write and cleanup succeeded"
		}
		if created {
			reason += "; created directory"
		}
		report("data directory", status, reason)
	}
	if online {
		if failed {
			report("provider catalog", "INFO", "skipped until local checks pass")
		} else {
			client := llm.New(llm.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Header: providerHeaders()})
			count, reason := checkCatalog(context.Background(), client.Models)
			if reason != "" {
				report("provider catalog", "FAIL", reason)
			} else {
				report("provider catalog", "PASS", fmt.Sprintf("%d selectable tool-capable models", count))
			}
		}
	}
	if failed {
		return errors.New("doctor checks failed")
	}
	return nil
}

func validAPIURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return false
		}
	}
	return true
}

// probeDataDirectory never opens a database or changes an existing mode. Its
// bounded probe is removed on both success and write/close failure paths.
func probeDataDirectory(dir string, createTemp func(string, string) (*os.File, error)) (created bool, reason string) {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, "cannot create directory; check ACPLAUSE_DATA_DIR and parent access"
		}
		created = true
	} else if err != nil {
		return false, "cannot inspect directory; check ACPLAUSE_DATA_DIR and access"
	} else if !info.IsDir() {
		return false, "not a directory; check ACPLAUSE_DATA_DIR"
	}
	f, err := createTemp(dir, ".acplause-doctor-*")
	if err != nil {
		return created, "cannot create temporary file; check directory write access"
	}
	defer func() {
		if err := f.Close(); err != nil && reason == "" {
			reason = "temporary file close failed"
		}
		if err := os.Remove(f.Name()); err != nil {
			reason = "temporary file cleanup failed; check directory access"
		}
	}()
	if _, err := f.Write([]byte("acplause doctor\n")); err != nil {
		reason = "temporary file write failed; check directory write access"
	}
	return created, reason
}
