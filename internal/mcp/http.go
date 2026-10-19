package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"
)

var errSessionExpired = errors.New("MCP HTTP session expired; operation outcome may be unknown")

type httpTransport struct {
	p                  *peer
	cfg                Config
	client             *http.Client
	mu                 sync.Mutex
	version, sessionID string
	closed             bool
	jobs               sync.WaitGroup
	once               sync.Once
	getOnce            sync.Once
	slots              chan struct{}
}

func newHTTP(p *peer, cfg Config) *httpTransport {
	t := &httpTransport{p: p, cfg: cfg, slots: make(chan struct{}, 64), client: &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	return t
}
func (t *httpTransport) start()                     { go func() { <-t.p.ctx.Done(); _ = t.Close() }() }
func (t *httpTransport) Initialized(version string) { t.mu.Lock(); t.version = version; t.mu.Unlock() }
func (t *httpTransport) request(ctx context.Context, method string, data []byte, cursor string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.cfg.URL, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid MCP HTTP endpoint")
	}
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet {
		req.Header.Set("Accept", "text/event-stream")
	}
	t.mu.Lock()
	version, session := t.version, t.sessionID
	t.mu.Unlock()
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, errors.New("MCP HTTP connection failed; operation outcome may be unknown")
	}
	if resp.StatusCode == http.StatusNotFound && session != "" {
		resp.Body.Close()
		return nil, errSessionExpired
	}
	return resp, nil
}
func (t *httpTransport) launch(ctx context.Context, work func(context.Context) error) (<-chan error, error) {
	select {
	case t.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.p.ctx.Done():
		return nil, t.p.err()
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		<-t.slots
		return nil, errClosed
	}
	t.jobs.Add(1)
	t.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		defer t.jobs.Done()
		defer func() { <-t.slots }()
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(t.p.ctx, cancel)
		defer stop()
		defer cancel()
		done <- work(ctx)
	}()
	return done, nil
}
func (t *httpTransport) Send(ctx context.Context, data []byte) error {
	var msg wire
	if json.Unmarshal(data, &msg) != nil {
		return errors.New("invalid outbound MCP message")
	}
	isCall := msg.Method != "" && msg.ID != nil
	done, err := t.launch(ctx, func(ctx context.Context) error {
		err := t.post(ctx, data, msg, isCall)
		if err != nil && ctx.Err() == nil {
			t.p.fail(err)
		}
		return err
	})
	if err != nil {
		return err
	}
	if isCall {
		return nil
	}
	select {
	case err := <-done:
		if err == nil && msg.Method == "notifications/initialized" {
			t.startGET()
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.p.ctx.Done():
		return t.p.err()
	}
}
func (t *httpTransport) post(ctx context.Context, data []byte, msg wire, isCall bool) error {
	resp, err := t.request(ctx, http.MethodPost, data, "")
	if err != nil {
		return err
	}
	if !isCall {
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("MCP notification/response rejected (HTTP %d)", resp.StatusCode)
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return fmt.Errorf("MCP request rejected (HTTP %d)", resp.StatusCode)
	}
	if msg.Method == "initialize" {
		id := resp.Header.Get("Mcp-Session-Id")
		if len(id) > 1024 {
			resp.Body.Close()
			return errors.New("invalid MCP HTTP session ID")
		}
		for _, c := range id {
			if c < 0x21 || c > 0x7e {
				resp.Body.Close()
				return errors.New("invalid MCP HTTP session ID")
			}
		}
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media == "application/json" {
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
		if err != nil {
			return errors.New("MCP HTTP response interrupted; operation outcome unknown")
		}
		if len(b) > maxMessage {
			return errors.New("MCP HTTP response exceeds 8 MiB")
		}
		t.p.receive(b)
		var id string
		_ = json.Unmarshal(msg.ID, &id)
		if t.p.pending(id) {
			return errors.New("MCP HTTP response did not answer its request")
		}
		return nil
	}
	if media != "text/event-stream" {
		resp.Body.Close()
		return errors.New("unsupported MCP HTTP response content type")
	}
	var id string
	_ = json.Unmarshal(msg.ID, &id)
	cursor := ""
	retry := time.Second
	for {
		err = t.stream(ctx, resp, id, &cursor, &retry)
		if !t.p.pending(id) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errStreamInterrupted) {
			return err
		}
		if cursor == "" {
			return errors.New("MCP response stream lost without replay cursor; operation outcome unknown")
		}
		if err := waitRetry(ctx, retry); err != nil {
			return err
		}
		// Resume a response stream by GET. Repeating the POST could execute the
		// operation twice even when the first response never reached this client.
		resp, err = t.request(ctx, http.MethodGet, nil, cursor)
		if err != nil {
			return err
		}
		media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if resp.StatusCode != 200 || media != "text/event-stream" {
			resp.Body.Close()
			return errors.New("MCP response stream cannot be resumed; operation outcome unknown")
		}
	}
}
func (t *httpTransport) stream(ctx context.Context, resp *http.Response, requestID string, cursor *string, retry *time.Duration) error {
	defer resp.Body.Close()
	r := newEventReader(resp.Body)
	for {
		e, err := r.next()
		if err != nil {
			return err
		}
		if e.hasID {
			*cursor = e.id
		}
		if e.hasRetry {
			*retry = e.retry
		}
		if len(e.data) > 0 {
			t.p.receive(e.data)
		}
		if requestID != "" && !t.p.pending(requestID) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
func (t *httpTransport) startGET() {
	t.getOnce.Do(func() {
		_, _ = t.launch(t.p.ctx, func(ctx context.Context) error {
			cursor := ""
			retry := time.Second
			for ctx.Err() == nil {
				resp, err := t.request(ctx, http.MethodGet, nil, cursor)
				if errors.Is(err, errSessionExpired) {
					t.p.fail(err)
					return err
				}
				if err == nil {
					if resp.StatusCode == http.StatusMethodNotAllowed {
						resp.Body.Close()
						return nil
					}
					media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
					if resp.StatusCode != 200 || media != "text/event-stream" {
						resp.Body.Close()
						t.p.fail(errors.New("invalid MCP GET event stream"))
						return nil
					}
					err = t.stream(ctx, resp, "", &cursor, &retry)
					if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errStreamInterrupted) && ctx.Err() == nil {
						t.p.fail(err)
						return err
					}
				}
				if err := waitRetry(ctx, retry); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
	})
}
func (t *httpTransport) Close() error {
	t.once.Do(func() {
		t.mu.Lock()
		t.closed = true
		session := t.sessionID
		t.mu.Unlock()
		t.p.fail(errClosed)
		t.jobs.Wait()
		if session != "" && !errors.Is(t.p.err(), errSessionExpired) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			resp, err := t.request(ctx, http.MethodDelete, nil, "")
			if err == nil {
				resp.Body.Close()
			}
			cancel()
		}
		t.client.CloseIdleConnections()
	})
	return nil
}
func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *peer) pending(id string) bool { p.mu.Lock(); defer p.mu.Unlock(); return p.calls[id] != nil }
