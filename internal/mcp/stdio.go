package mcp

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type writeItem struct {
	ctx      context.Context
	data     []byte
	done     chan error
	dispatch *dispatch
}
type stdioTransport struct {
	p        *peer
	cmd      *exec.Cmd
	in, out  *os.File
	writes   chan writeItem
	exited   chan struct{}
	once     sync.Once
	process  *processControl
	closeErr error
}

// processControl keeps platform ownership attached to the invocation, so a
// reconfigured MCP server cannot redirect cleanup to a newer process.
type processControl struct {
	signal func(bool)
	close  func() error
}

func startStdio(p *peer, cfg Config, cwd string) (*stdioTransport, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cwd
	cmd.Env = processEnvironment(cfg.Env)
	cmd.Stdin = inR
	cmd.Stdout = outW
	// A nil stderr uses the null device directly, without an exec copy goroutine.
	// Detached descendants cannot keep cmd.Wait blocked on an inherited pipe.
	process, err := startProcess(cmd)
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, errors.New("cannot launch MCP server")
	}
	inR.Close()
	outW.Close()
	t := &stdioTransport{p: p, cmd: cmd, process: process, in: inW, out: outR, writes: make(chan writeItem, 64), exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(t.exited) }()
	return t, nil
}
func (t *stdioTransport) start() {
	go func() {
		sc := bufio.NewScanner(t.out)
		sc.Buffer(make([]byte, 4096), maxMessage+1)
		for sc.Scan() {
			t.p.receive(slices.Clone(sc.Bytes()))
		}
		t.p.fail(errors.New("MCP server disconnected; an in-flight operation may have completed"))
	}()
	go func() {
		for {
			select {
			case item := <-t.writes:
				if err := item.ctx.Err(); err != nil {
					item.done <- err
					continue
				}
				if item.dispatch != nil {
					if err := item.dispatch.admit(); err != nil {
						item.done <- err
						continue
					}
				}
				stop := context.AfterFunc(item.ctx, func() { t.p.fail(errors.New("MCP write interrupted; operation outcome unknown")) })
				_, err := t.in.Write(append(item.data, '\n'))
				stop()
				item.done <- err
				if err != nil {
					t.p.fail(errClosed)
					return
				}
			case <-t.p.ctx.Done():
				return
			}
		}
	}()
	go func() { <-t.p.ctx.Done(); _ = t.Close() }()
}
func (t *stdioTransport) Send(ctx context.Context, b []byte) error {
	return t.sendRequest(ctx, b, nil)
}

func (t *stdioTransport) sendRequest(ctx context.Context, b []byte, dispatch *dispatch) error {
	select {
	case <-t.p.ctx.Done():
		return t.p.err()
	default:
	}
	pending := ctx
	var pendingDone <-chan struct{}
	if dispatch != nil {
		pending = dispatch.pending
		pendingDone = pending.Done()
	}
	item := writeItem{ctx: ctx, data: b, done: make(chan error, 1), dispatch: dispatch}
	select {
	case t.writes <- item:
	case <-pending.Done():
		return pending.Err()
	case <-t.p.ctx.Done():
		return t.p.err()
	}
	// Send owns the write's context until delivery, including notifications
	// whose callers cancel their short timeout immediately after it returns.
	for {
		select {
		case err := <-item.done:
			return err
		case <-pendingDone:
			select {
			case <-dispatch.admitted:
				pendingDone = nil
			default:
				return pending.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-t.p.ctx.Done():
			return t.p.err()
		}
	}
}
func (t *stdioTransport) Initialized(string) {}
func (t *stdioTransport) Close() error {
	t.once.Do(func() {
		t.p.fail(errClosed)
		_ = t.in.Close()
		select {
		case <-t.exited:
		case <-time.After(time.Second):
			t.process.signal(false)
			select {
			case <-t.exited:
			case <-time.After(time.Second):
			}
		}
		// Include descendants even when the direct child already exited.
		t.process.signal(true)
		t.closeErr = t.process.close()
		_ = t.out.Close()
		<-t.exited
	})
	return t.closeErr
}
func processEnvironment(extra map[string]string) []string {
	allowed := map[string]bool{}
	for _, k := range strings.Fields("HOME PATH USER LOGNAME TMPDIR TMP TEMP LANG SYSTEMROOT WINDIR COMSPEC PATHEXT USERPROFILE APPDATA LOCALAPPDATA") {
		allowed[k] = true
	}
	env := map[string]string{}
	key := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	for _, v := range os.Environ() {
		k, value, _ := strings.Cut(v, "=")
		if allowed[strings.ToUpper(k)] || strings.HasPrefix(k, "LC_") {
			env[key(k)] = value
		}
	}
	for k, v := range extra {
		env[key(k)] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
