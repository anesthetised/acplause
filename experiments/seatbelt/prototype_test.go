//go:build darwin && seatbelt_prototype

// Package seatbelt_test is a disposable ADR 0025 experiment, never a runtime backend.
package seatbelt_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/tools"
)

var fixtureBin string

// TestMain keeps the native probe outside every payload-writable root.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/private/tmp", "acplause-seatbelt-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureBin = filepath.Join(dir, "probe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, "/usr/bin/clang", "-Wall", "-Wextra", "-Werror", "-O0", "-o", fixtureBin, "testdata/probe.c")
	out, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture compilation: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type lab struct {
	base, workspace, outside, readonly, scratch string
	params, env                                 []string
}

func newLab(t *testing.T) *lab {
	t.Helper()
	base, err := os.MkdirTemp("/private/tmp", "acplause-seatbelt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	l := &lab{base: base, workspace: filepath.Join(base, "workspace"), outside: filepath.Join(base, "outside"), readonly: filepath.Join(base, "readonly"), scratch: filepath.Join(base, "scratch")}
	for _, name := range []string{"workspace", "outside", "readonly", "scratch", "additional", "cache", "workspace-sibling"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goRoot := commandOutput(t, goBin, "env", "GOROOT")
	xcode := commandOutput(t, "/usr/bin/xcode-select", "-p")
	// Xcode's developer tools load frameworks from the adjacent Contents tree.
	if strings.HasSuffix(xcode, ".app/Contents/Developer") {
		xcode = filepath.Dir(xcode)
	}
	l.params = []string{"ROOT_0=" + l.workspace, "ROOT_1=" + filepath.Join(base, "additional"), "ROOT_2=" + filepath.Join(base, "cache"), "ROOT_3=" + l.scratch,
		"READ_0=" + filepath.Dir(fixtureBin), "READ_1=" + l.readonly, "READ_2=" + goRoot, "READ_3=" + xcode}
	l.env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + os.Getenv("HOME"), "TMPDIR=" + l.scratch, "LC_ALL=C", "PROBE=" + fixtureBin,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0", "GOCACHE=" + filepath.Join(base, "cache"), "GOMODCACHE=" + filepath.Join(base, "cache", "mod")}
	return l
}

func commandOutput(t *testing.T, executable string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(executable), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (l *lab) sandboxArgs(args ...string) []string {
	policy, _ := filepath.Abs("testdata/offline.sb")
	launcher := []string{"/usr/bin/sandbox-exec", "-f", policy}
	for _, param := range l.params {
		launcher = append(launcher, "-D", param)
	}
	return append(launcher, args...)
}

func (l *lab) command(ctx context.Context, confined bool, args ...string) *exec.Cmd {
	if confined {
		args = l.sandboxArgs(args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env, cmd.Dir = l.env, l.workspace
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

func (l *lab) run(t *testing.T, confined bool, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := l.command(ctx, confined, args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("fixture exceeded deadline: %v", ctx.Err())
	}
	return strings.TrimSpace(string(out)), err
}

// checkProbe refuses launch failures: a crashed payload proves no denial.
func checkProbe(t *testing.T, out string, err error, allow bool) {
	t.Helper()
	t.Logf("allowed=%t observed=%q error=%v", allow, out, err)
	if allow {
		if err != nil || out != "allowed" {
			t.Errorf("expected successful fixture operation")
		}
		return
	}
	if out != "denied errno=1 (Operation not permitted)" && out != "denied errno=13 (Permission denied)" {
		t.Errorf("expected an access denial from the running fixture")
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Errorf("expected fixture exit 1, got %v", err)
	}
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("synthetic fixture data\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestSeatbeltFilesystem requires a working payload before trusting denials.
func TestSeatbeltFilesystem(t *testing.T) {
	l := newLab(t)
	policy, err := os.ReadFile("testdata/offline.sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("profile sha256=%x; OS=%s; architecture=%s", sha256.Sum256(policy), commandOutput(t, "/usr/bin/sw_vers", "-buildVersion"), commandOutput(t, "/usr/bin/uname", "-m"))
	for _, dir := range []string{l.workspace, l.outside, l.readonly, l.scratch, filepath.Join(l.base, "additional"), filepath.Join(l.base, "cache"), filepath.Join(l.base, "workspace-sibling")} {
		write(t, filepath.Join(dir, "file"))
	}
	if out, err := l.run(t, true, fixtureBin, "read", filepath.Join(l.workspace, "file")); err != nil || out != "allowed" {
		t.Fatalf("confined positive control failed: %v: %s; no negative result is evidence", err, out)
	}
	for _, tc := range []struct {
		name, operation, path string
		allow                 bool
	}{
		{"workspace-read", "read", filepath.Join(l.workspace, "file"), true},
		{"workspace-write", "write", filepath.Join(l.workspace, "file"), true},
		{"additional-write", "write", filepath.Join(l.base, "additional", "file"), true},
		{"private-scratch", "write", filepath.Join(l.scratch, "file"), true},
		{"explicit-cache", "write", filepath.Join(l.base, "cache", "file"), true},
		{"read-only-read", "read", filepath.Join(l.readonly, "file"), true},
		{"read-only-write", "write", filepath.Join(l.readonly, "file"), false},
		{"outside-read", "read", filepath.Join(l.outside, "file"), false},
		{"outside-write", "write", filepath.Join(l.outside, "file"), false},
		{"outside-chmod", "chmod", filepath.Join(l.outside, "file"), false},
		{"outside-stat-exception", "stat", filepath.Join(l.outside, "file"), true},
		{"prefix-collision", "read", filepath.Join(l.base, "workspace-sibling", "file"), false},
		{"var-alias-allowed", "read", strings.TrimPrefix(filepath.Join(l.workspace, "file"), "/private"), true},
		{"var-alias-denied", "read", strings.TrimPrefix(filepath.Join(l.outside, "file"), "/private"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, err := l.run(t, false, fixtureBin, tc.operation, tc.path); err != nil {
				t.Fatalf("unconfined control failed: %v: %s", err, out)
			}
			out, err := l.run(t, true, fixtureBin, tc.operation, tc.path)
			checkProbe(t, out, err, tc.allow)
		})
	}
}

// TestSeatbeltAliases distinguishes pathname escapes from pre-granted objects.
func TestSeatbeltAliases(t *testing.T) {
	l := newLab(t)
	outside := filepath.Join(l.outside, "file")
	write(t, outside)
	for _, tc := range []struct{ name, operation string }{{"symlink-read", "read"}, {"symlink-write", "write"}, {"new-hardlink", "link"}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(l.workspace, tc.name)
			args := []string{fixtureBin, tc.operation, path}
			if tc.operation == "link" {
				args = []string{fixtureBin, "link", outside, path}
			} else if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			out, err := l.run(t, false, args...)
			checkProbe(t, out, err, true)
			if tc.operation == "link" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			out, err = l.run(t, true, args...)
			checkProbe(t, out, err, false)
		})
	}
	t.Run("pre-existing-hardlink-residual", func(t *testing.T) {
		path := filepath.Join(l.workspace, "existing-hardlink")
		if err := os.Link(outside, path); err != nil {
			t.Fatal(err)
		}
		out, err := l.run(t, true, fixtureBin, "write", path)
		checkProbe(t, out, err, true)
		got, err := os.ReadFile(outside)
		if err != nil || !strings.HasSuffix(string(got), "x") {
			t.Fatalf("outside alias did not reflect the allowed write: %v", err)
		}
	})
}

// TestSeatbeltGitProtection exercises relocation as well as direct writes.
func TestSeatbeltGitProtection(t *testing.T) {
	for _, relative := range []string{".git/config", ".git/config.worktree", ".git/hooks/pre-commit", ".git/worktrees/linked/config.worktree", ".git/worktrees/linked/hooks/pre-commit", ".git/modules/mod/config", ".git/modules/mod/hooks/pre-commit", ".git/modules/mod/modules/nested/config"} {
		t.Run(relative, func(t *testing.T) {
			l := newLab(t)
			path := filepath.Join(l.workspace, relative)
			write(t, path)
			out, err := l.run(t, false, fixtureBin, "write", path)
			checkProbe(t, out, err, true)
			out, err = l.run(t, true, fixtureBin, "write", path)
			checkProbe(t, out, err, false)
		})
	}
	for _, tc := range []struct{ name, source, dest, target string }{
		{"rename-dotgit", "repo/.git", "repo/renamed", "repo/renamed/config"},
		{"rename-ancestor", "repo", "renamed", "renamed/.git/config"},
		{"rename-submodule-gitdir", "repo/.git/modules/mod", "escaped", "escaped/config"},
		{"rename-worktree-gitdir", "repo/.git/worktrees/linked", "escaped", "escaped/config.worktree"},
		{"replace-config", "staged-config", "repo/.git/config", "repo/.git/config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, confined := range []bool{false, true} {
				l := newLab(t)
				write(t, filepath.Join(l.workspace, "repo/.git/config"))
				write(t, filepath.Join(l.workspace, "repo/.git/modules/mod/config"))
				write(t, filepath.Join(l.workspace, "repo/.git/worktrees/linked/config.worktree"))
				write(t, filepath.Join(l.workspace, "staged-config"))
				out, err := l.run(t, confined, fixtureBin, "rename-write", filepath.Join(l.workspace, tc.source), filepath.Join(l.workspace, tc.dest), filepath.Join(l.workspace, tc.target))
				checkProbe(t, out, err, !confined)
			}
		})
	}
}

// TestSeatbeltNetwork pairs every denial with a reachable local echo service.
func TestSeatbeltNetwork(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6", "udp4", "udp6", "unix"} {
		t.Run(network, func(t *testing.T) {
			l := newLab(t)
			address := "127.0.0.1:0"
			if strings.HasSuffix(network, "6") {
				address = "[::1]:0"
			}
			if network == "unix" {
				address = filepath.Join(l.workspace, "fixture.sock")
			}
			endpoint := address
			done := make(chan struct{})
			if strings.HasPrefix(network, "udp") {
				pc, err := net.ListenPacket(network, address)
				if err != nil {
					t.Fatalf("required fixture endpoint unavailable: %v", err)
				}
				t.Cleanup(func() { _ = pc.Close(); <-done })
				_, endpoint, _ = net.SplitHostPort(pc.LocalAddr().String())
				go func() {
					defer close(done)
					var data [1]byte
					for {
						n, peer, err := pc.ReadFrom(data[:])
						if err != nil {
							return
						}
						_, _ = pc.WriteTo(data[:n], peer)
					}
				}()
			} else {
				listener, err := net.Listen(network, address)
				if err != nil {
					t.Fatalf("required fixture endpoint unavailable: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close(); <-done })
				if network != "unix" {
					_, endpoint, _ = net.SplitHostPort(listener.Addr().String())
				}
				go func() {
					defer close(done)
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.SetDeadline(time.Now().Add(time.Second))
						_, _ = io.CopyN(conn, conn, 1)
						_ = conn.Close()
					}
				}()
			}
			out, err := l.run(t, false, fixtureBin, network, endpoint)
			checkProbe(t, out, err, true)
			out, err = l.run(t, true, fixtureBin, network, endpoint)
			checkProbe(t, out, err, false)
		})
	}
}

// TestSeatbeltProcessAccess targets only a fixture holding a synthetic marker.
func TestSeatbeltProcessAccess(t *testing.T) {
	l := newLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	victim := l.command(ctx, false, fixtureBin, "victim", "unused")
	marker := "FAKE_SECRET=" + rand.Text()
	victim.Env = append(victim.Env, marker)
	lease, err := victim.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := victim.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close(); _ = victim.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("victim not ready: %v", err)
	}
	pid := fmt.Sprint(victim.Process.Pid)
	for _, operation := range []string{"pidinfo", "procargs", "signal", "taskport"} {
		t.Run(operation, func(t *testing.T) {
			args := []string{fixtureBin, operation, pid}
			if operation == "procargs" {
				args = append(args, marker)
			}
			out, err := l.run(t, false, args...)
			if operation == "taskport" && err != nil {
				t.Errorf("debugging gate UNVERIFIED: unconfined control: %s (%v)", out, err)
				return
			}
			checkProbe(t, out, err, true)
			out, err = l.run(t, true, args...)
			checkProbe(t, out, err, false)
		})
	}
}

// TestSeatbeltRootReplacement samples two substitutions, not arbitrary races.
func TestSeatbeltRootReplacement(t *testing.T) {
	for _, when := range []string{"before-launch", "after-confinement"} {
		t.Run(when, func(t *testing.T) {
			l := newLab(t)
			path := filepath.Join(l.workspace, "file")
			write(t, path)
			write(t, filepath.Join(l.outside, "file"))
			replace := func() {
				if err := os.Rename(l.workspace, filepath.Join(l.base, "original")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(l.outside, l.workspace); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before-launch" {
				// Parameters were captured earlier, but contain no identity pin.
				replace()
				out, err := l.run(t, true, fixtureBin, "read", path)
				checkProbe(t, out, err, false)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := l.command(ctx, true, fixtureBin, "wait-read", path)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				cancel()
				if !waited {
					_ = cmd.Wait()
				}
			}()
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
				_ = stdin.Close()
				t.Fatalf("fixture did not reach confined barrier: %v", err)
			}
			replace()
			_, _ = io.WriteString(stdin, "\n")
			_ = stdin.Close()
			out, err := io.ReadAll(reader)
			if err != nil {
				t.Error(err)
			}
			err = cmd.Wait()
			waited = true
			checkProbe(t, strings.TrimSpace(string(out)), err, false)
		})
	}
}

// TestSeatbeltInheritedDescriptor demonstrates authority a profile cannot revoke.
func TestSeatbeltInheritedDescriptor(t *testing.T) {
	l := newLab(t)
	path := filepath.Join(l.outside, "file")
	write(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, confined := range []bool{false, true} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		cmd := l.command(ctx, confined, fixtureBin, "fdread", "3")
		cmd.ExtraFiles = []*os.File{f}
		out, err := cmd.CombinedOutput()
		_ = f.Close()
		checkProbe(t, strings.TrimSpace(string(out)), err, true)
	}
}

func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

// TestSeatbeltLifecycle invokes the real toolbox and observer. Closed stdio,
// setsid, double fork and exec distinguish cleanup from inherited confinement.
func TestSeatbeltLifecycle(t *testing.T) {
	for _, kind := range []string{"attached", "detached"} {
		for _, end := range []string{"exit", "timeout", "cancel"} {
			t.Run(kind+"-"+end, func(t *testing.T) {
				l := newLab(t)
				lease := filepath.Join(l.workspace, "lease")
				write(t, lease)
				outside := filepath.Join(l.outside, "file")
				write(t, outside)
				t.Cleanup(func() {
					_ = os.Remove(lease)
					if kind == "detached" {
						awaitFile(t, filepath.Join(l.workspace, "done"))
					}
				})
				timeout := time.Duration(0)
				if end == "timeout" {
					timeout = time.Second
				}
				tb, err := tools.New(l.workspace, nil, tools.Options{BashTimeout: &timeout})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = tb.Close() })
				stay := "stay"
				if end == "exit" {
					stay = "exit"
				}
				args := append([]string{"/usr/bin/env", "-i"}, l.env...)
				args = append(args, l.sandboxArgs(fixtureBin, "spawn", outside, l.workspace, kind, stay)...)
				raw, err := json.Marshal(map[string]string{"command": "exec " + shellQuote(args)})
				if err != nil {
					t.Fatal(err)
				}
				call, err := tb.Prepare("bash", string(raw))
				if err != nil {
					t.Fatal(err)
				}
				observer := call.ObserveShell()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result := make(chan tools.Result, 1)
				go func() { result <- call.Run(ctx) }()
				awaitFile(t, filepath.Join(l.workspace, "ready"))
				verdict, err := os.ReadFile(filepath.Join(l.workspace, "ready"))
				if err != nil || string(verdict) != "denied" {
					t.Fatalf("descendant confinement failed: %q (%v)", verdict, err)
				}
				if end == "cancel" {
					cancel()
				}
				select {
				case got := <-result:
					if got.Failed != (end != "exit") {
						t.Errorf("unexpected foreground result: %+v", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("foreground runner failed to return")
				}
				before := observer.Snapshot(0)
				first, _ := os.ReadFile(filepath.Join(l.workspace, "heartbeat"))
				timer := time.NewTimer(100 * time.Millisecond)
				<-timer.C
				second, _ := os.ReadFile(filepath.Join(l.workspace, "heartbeat"))
				after := observer.Snapshot(0)
				if before.Exit == nil || after.Exit == nil || string(before.Output) != string(after.Output) {
					t.Error("terminal observer failed to finalize before returning")
				}
				alive := len(second) > len(first)
				t.Logf("confinement=denied; output-finalized=%t; descendant-active-after-return=%t", after.Exit != nil, alive)
				if alive {
					t.Error("foreground lifecycle gate FAILED: descendant still executing after completion")
				}
			})
		}
	}
}

func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if info, err := os.Stat(path); err == nil && (info.Size() > 0 || filepath.Base(path) == "done") {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("fixture did not publish %s", filepath.Base(path))
		}
	}
}

// TestSeatbeltLocalTools keeps compatibility controls in independent workspaces.
func TestSeatbeltLocalTools(t *testing.T) {
	l := newLab(t)
	goRoot := strings.TrimPrefix(l.params[6], "READ_2=")
	xcode := strings.TrimPrefix(l.params[7], "READ_3=")
	t.Logf("toolchains: %s; %s; %s", commandOutput(t, filepath.Join(goRoot, "bin/go"), "version"), commandOutput(t, "/usr/bin/git", "--version"), commandOutput(t, "/usr/bin/clang", "--version"))
	t.Logf("grants: workspace, additional, cache, scratch writable; fixture bin, readonly, GOROOT=%s and Xcode=%s readable", goRoot, xcode)
	t.Run("git", func(t *testing.T) {
		for _, confined := range []bool{false, true} {
			l := newLab(t)
			// Repository setup is trusted: init creates config and hooks.
			out, err := l.run(t, false, "/usr/bin/git", "-c", "init.defaultBranch=main", "init")
			if err != nil {
				t.Fatalf("git fixture setup: %v: %s", err, out)
			}
			write(t, filepath.Join(l.workspace, "tracked"))
			out, err = l.run(t, confined, "/bin/bash", "--noprofile", "--norc", "-c", "set -eu\ngit status --porcelain\ngit add tracked\ngit commit -m fixture\ngit branch fixture\ngit checkout fixture\ngit status --porcelain")
			t.Logf("confined=%t git offline workflow: %s", confined, out)
			if err != nil {
				t.Errorf("local git workflow failed: %v", err)
			}
		}
	})
	t.Run("go-build-test", func(t *testing.T) {
		l := newLab(t)
		// No module downloads or user cache: only stdlib, private scratch/cache.
		for name, body := range map[string]string{
			"go.mod":       "module example.invalid/fixture\n\ngo 1.27.2\n",
			"main.go":      "package main\nfunc main() { println(\"fixture-built\") }\n",
			"main_test.go": "package main\nimport \"testing\"\nfunc TestFixture(t *testing.T) { main() }\n",
		} {
			if err := os.WriteFile(filepath.Join(l.workspace, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for _, confined := range []bool{false, true} {
			for _, args := range [][]string{{filepath.Join(goRoot, "bin/go"), "build", "-a", "-o", "built-go", "."}, {filepath.Join(l.workspace, "built-go")}, {filepath.Join(goRoot, "bin/go"), "test", "-count=1", "-v", "./..."}} {
				out, err := l.run(t, confined, args...)
				t.Logf("confined=%t %s: %s", confined, filepath.Base(args[0]), out)
				if err != nil {
					t.Errorf("offline Go compatibility gate failed: %v", err)
					return
				}
			}
		}
	})
	t.Run("sdk-and-built-executable", func(t *testing.T) {
		l := newLab(t)
		if err := os.WriteFile(filepath.Join(l.workspace, "built.c"), []byte("#include <stdio.h>\nint main(void) { puts(\"fixture-built\"); return 0; }\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, confined := range []bool{false, true} {
			out, err := l.run(t, confined, "/usr/bin/clang", "-o", "built-c", "built.c")
			t.Logf("confined=%t SDK build: %s", confined, out)
			if err != nil {
				t.Errorf("SDK compatibility gate failed: %v", err)
				return
			}
			out, err = l.run(t, confined, filepath.Join(l.workspace, "built-c"))
			if err != nil || out != "fixture-built" {
				t.Errorf("newly built executable failed: %q (%v)", out, err)
			}
		}
	})
	t.Run("darwin-temporary-directory", func(t *testing.T) {
		l := newLab(t)
		for _, confined := range []bool{false, true} {
			out, err := l.run(t, confined, fixtureBin, "darwin-temp", "unused")
			t.Logf("confined=%t Darwin confstr: %s (%v)", confined, out, err)
			if err != nil {
				t.Error("Darwin temporary-directory discovery unavailable")
			}
		}
	})
}

// TestSeatbeltLoader distinguishes environment stripping from confined startup.
func TestSeatbeltLoader(t *testing.T) {
	l := newLab(t)
	dylib := filepath.Join(l.workspace, "inject.dylib")
	source, err := filepath.Abs("testdata/inject.c")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := l.run(t, false, "/usr/bin/clang", "-Wall", "-Wextra", "-Werror", "-dynamiclib", "-o", dylib, source); err != nil {
		t.Fatalf("loader fixture compilation: %v: %s", err, out)
	}
	for _, confined := range []bool{false, true} {
		canary := filepath.Join(l.outside, fmt.Sprintf("loader-%t", confined))
		saved := l.env
		l.env = append(append([]string{}, saved...), "DYLD_INSERT_LIBRARIES="+dylib, "FIXTURE_CANARY="+canary)
		out, err := l.run(t, confined, fixtureBin, "loader-status", "unused")
		l.env = saved
		_, statErr := os.Stat(canary)
		t.Logf("confined=%t payload loader variable=%s outside-constructor-write=%t", confined, out, statErr == nil)
		if err != nil || (confined && statErr == nil) || (!confined && statErr != nil) {
			t.Errorf("loader canary failed: %v; canary=%v", err, statErr)
		}
	}
	t.Run("BASH_ENV", func(t *testing.T) {
		canary := filepath.Join(l.outside, "bash-env")
		prelude := filepath.Join(l.workspace, "prelude")
		if err := os.WriteFile(prelude, []byte("\"$PROBE\" write \"$FIXTURE_CANARY\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		l.env = append(l.env, "BASH_ENV="+prelude, "FIXTURE_CANARY="+canary)
		if out, err := l.run(t, false, "/bin/bash", "--noprofile", "--norc", "-c", "printf payload-started"); err != nil || !strings.Contains(out, "allowed") {
			t.Fatalf("unconfined BASH_ENV control failed: %v: %s", err, out)
		}
		if err := os.Remove(canary); err != nil {
			t.Fatal(err)
		}
		out, err := l.run(t, true, "/bin/bash", "--noprofile", "--norc", "-c", "printf payload-started")
		t.Logf("BASH_ENV output: %s", out)
		if _, statErr := os.Stat(canary); err != nil || !os.IsNotExist(statErr) || !strings.Contains(out, "payload-started") || !strings.Contains(out, "denied errno=1") {
			t.Errorf("shell startup boundary failed: %v; canary=%v", err, statErr)
		}
	})
}
