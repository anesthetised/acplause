package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is OpenRouter's API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultIdleTimeout bounds silence from the provider, before the response
// headers and between body reads. OpenRouter sends keepalive comments while a
// request is queued, so a minute or two of nothing is a fault, such as a
// half-open connection.
const DefaultIdleTimeout = 2 * time.Minute

// maxRetryAfter caps how long a provider's Retry-After can delay a retry, so
// a hostile or misconfigured header cannot park a turn.
const maxRetryAfter = 30 * time.Second

// errIdle is the cancellation cause of a request that went silent.
var errIdle = errors.New("idle timeout")

// Config configures a Client. Zero values select defaults.
type Config struct {
	BaseURL    string
	APIKey     string
	Header     http.Header // extra headers, e.g. OpenRouter attribution
	HTTPClient *http.Client
	Retries    int           // retries of transient failures; default 3
	RetryDelay time.Duration // first backoff, doubled per retry; default 500ms
	// IdleTimeout is the longest provider silence; nil selects
	// DefaultIdleTimeout and 0 disables it.
	IdleTimeout *time.Duration
}

// Client talks to an OpenAI-compatible API.
type Client struct {
	baseURL    string
	apiKey     string
	header     http.Header
	http       *http.Client
	retries    int
	retryDelay time.Duration

	idleTimeout time.Duration
	// after is time.After; tests replace it to observe retry waits.
	after func(time.Duration) <-chan time.Time
}

// New returns a Client for cfg.
func New(cfg Config) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultBaseURL), "/"),
		apiKey:     cfg.APIKey,
		header:     cfg.Header,
		http:       cfg.HTTPClient,
		retries:    cfg.Retries,
		retryDelay: cfg.RetryDelay,

		idleTimeout: DefaultIdleTimeout,
		after:       time.After,
	}
	if cfg.IdleTimeout != nil {
		c.idleTimeout = cmp.Or(*cfg.IdleTimeout, math.MaxInt64) // 0: a timer that never fires
	}
	if c.http == nil {
		c.http = &http.Client{CheckRedirect: noRedirects}
	}
	if c.retries == 0 {
		c.retries = 3
	}
	if c.retryDelay == 0 {
		c.retryDelay = 500 * time.Millisecond
	}
	return c
}

// Chat streams a completion, reporting text and reasoning through onDelta
// as they arrive. Cancelling ctx aborts the HTTP stream and returns
// ctx.Err(); other failures are *Error.
func (c *Client) Chat(ctx context.Context, req Request, onDelta func(Delta)) (*Completion, error) {
	body, err := json.Marshal(newChatBody(req))
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "application/json" {
		return nil, jsonInsteadOfStream(resp.Body)
	}
	return readStream(ctx, resp.Body, onDelta)
}

// jsonInsteadOfStream reports a successful status whose body is JSON, such as
// an error envelope, rather than the event stream that was requested.
func jsonInsteadOfStream(body io.Reader) error {
	data, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	var envelope struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
		return envelope.Error.toError()
	}
	return &Error{Kind: KindProtocol, Message: "expected an event stream, got a JSON response"}
}

// Model is an entry of the models list.
type Model struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	ContextLength       uint64   `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	// TopProvider supplies OpenRouter's advertised output ceiling; zero is unknown.
	TopProvider struct {
		MaxCompletionTokens uint64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

// Vision reports whether the model explicitly lists image input. Missing
// metadata means unknown, which is not permission to send images.
func (m Model) Vision() bool {
	return slices.Contains(m.Architecture.InputModalities, "image")
}

// Supports reports whether the model accepts a request parameter, such as
// "tools" or "reasoning". Models that publish no parameter list are assumed
// to accept everything.
func (m Model) Supports(param string) bool {
	return m.SupportedParameters == nil || slices.Contains(m.SupportedParameters, param)
}

// Models lists the available models.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	resp, err := c.send(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			return nil, e // an idle timeout from the body
		}
		return nil, &Error{Kind: KindProtocol, Message: "decode models: " + err.Error()}
	}
	return list.Data, nil
}

type chatBody struct {
	MaxTokens uint64        `json:"max_tokens,omitempty"`
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []Tool        `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
	Reasoning *reasoning    `json:"reasoning,omitempty"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

func newChatBody(req Request) chatBody {
	body := chatBody{Model: req.Model, Messages: wireMessages(req.Messages, req.Vision), Tools: req.Tools, Stream: true, MaxTokens: req.MaxTokens}
	if req.ReasoningEffort != "" {
		body.Reasoning = &reasoning{Effort: req.ReasoningEffort}
	}
	return body
}

// send performs a request, retrying transient failures with exponential
// backoff, or after the provider's Retry-After when it sent one. Retries
// happen only before a successful response is returned, so a stream that has
// started is never replayed.
func (c *Client) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, method, path, body)
		if err == nil {
			return resp, nil
		}
		e, ok := errors.AsType[*Error](err)
		if !ok || !e.Retryable() || attempt == c.retries {
			return nil, err
		}
		delay := c.retryDelay << attempt
		if e.retryAfter > 0 {
			delay = min(e.retryAfter, maxRetryAfter)
		}
		select {
		case <-c.after(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// noRedirects keeps requests, and the key they carry, on the configured API
// root: Go keeps Authorization on redirects to the same host or a subdomain,
// even on another port (ADR 0022).
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// do performs one attempt. The response body it returns fails with a
// KindTimeout *Error once the provider has been silent for the idle timeout;
// any bytes, keepalive comments included, count as activity.
func (c *Client) do(parent context.Context, method, path string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(parent)
	idle := time.AfterFunc(c.idleTimeout, func() { cancel(errIdle) })
	handedOff := false
	defer func() {
		if !handedOff {
			idle.Stop()
			cancel(nil)
		}
	}()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	for key, values := range c.header {
		req.Header[key] = values
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if errors.Is(context.Cause(ctx), errIdle) {
			return nil, c.idleError("no response")
		}
		return nil, &Error{Kind: KindUnreachable, Message: err.Error()}
	}
	if resp.StatusCode/100 == 3 {
		resp.Body.Close()
		return nil, &Error{Kind: KindUnknown, Status: resp.StatusCode, Message: "the API redirected; redirects are not followed, so configure the final API root"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := statusError(resp.StatusCode, snippet)
		e.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return nil, e
	}
	handedOff = true
	resp.Body = &idleBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timer: idle, client: c}
	return resp, nil
}

func (c *Client) idleError(what string) *Error {
	return &Error{Kind: KindTimeout, Message: fmt.Sprintf("%s from the provider for %s", what, c.idleTimeout)}
}

// idleBody resets the idle timer on every read that yields bytes and turns the
// cancellation the timer causes into a KindTimeout error.
type idleBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	client *Client
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.client.idleTimeout)
	}
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.ctx), errIdle) {
		err = b.client.idleError("no data")
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	b.cancel(nil)
	return b.ReadCloser.Close()
}

// parseRetryAfter reads a Retry-After value, delta-seconds or an HTTP date,
// relative to now. Zero means absent, malformed or already elapsed.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}
