//go:build unix

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrokenStdoutReleasesSession(t *testing.T) {
	for _, phase := range []string{"stdin open", "final drain"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary)
			cmd.Env = append(cleanEnv(), "ACPLAUSE_BASE_URL="+llm.srv.URL, "ACPLAUSE_API_KEY=test-key", "ACPLAUSE_DATA_DIR="+dir, "ACPLAUSE_DEFAULT_MODEL=test/model")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
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
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()

			// Read just the two setup responses. In the final-drain case the
			// client then leaves an oversized response blocked in the OS pipe.
			_, err = fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"test","version":"1"}}}
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%q,"mcpServers":[]}}
`, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			v, err := loadValidator()
			if err != nil {
				t.Fatal(err)
			}
			var session newSessionResult
			reader := bufio.NewReader(stdout)
			for _, method := range []string{"initialize", "session/new"} {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var msg message
				if err := v.decodeLine(line, &msg); err != nil {
					t.Fatal(err)
				}
				if err := v.validatePayload(&msg, method); err != nil || msg.Error != nil {
					t.Fatalf("%s: %v, %+v", method, err, msg.Error)
				}
				if method == "session/new" {
					if err := json.Unmarshal(msg.Result, &session); err != nil {
						t.Fatal(err)
					}
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(dir, "acplause.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			owner := func() string {
				t.Helper()
				var got string
				if err := db.QueryRowContext(ctx, "SELECT owner FROM sessions WHERE id = ?", session.SessionID).Scan(&got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			if owner() == "" {
				t.Fatal("new session has no lease")
			}
			if phase == "final drain" {
				if _, err := fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":3,"method":"%s"}`+"\n", strings.Repeat("x", 4<<20)); err != nil {
					t.Fatal(err)
				}
				if err := stdin.Close(); err != nil {
					t.Fatal(err)
				}
				// Serve observed EOF and Shutdown released the lease. Only
				// the writer can still be blocked on the oversized error.
				for owner() != "" {
					time.Sleep(5 * time.Millisecond)
				}
			}
			if err := stdout.Close(); err != nil {
				t.Fatal(err)
			}
			if phase == "stdin open" {
				if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":3,"method":"unknown"}`+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			_, report, _ := strings.Cut(stderr.String(), "\nacplause: ")
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.HasSuffix(strings.TrimSpace(report), "broken pipe") {
				t.Fatalf("expected exit 1 reporting the write failure, got %v; stderr %s", err, &stderr)
			}
			if got := owner(); got != "" {
				t.Fatalf("broken stdout left the session owned by %q", got)
			}
		})
	}
}
