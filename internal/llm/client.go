package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// DefaultBaseURL is OpenRouter's API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Config configures a Client. Zero values select defaults.
type Config struct {
	BaseURL    string
	APIKey     string
	Header     http.Header // extra headers, e.g. OpenRouter attribution
	HTTPClient *http.Client
	Retries    int           // retries of transient failures; default 3
	RetryDelay time.Duration // first backoff, doubled per retry; default 500ms
}

// Client talks to an OpenAI-compatible API.
type Client struct {
	baseURL    string
	apiKey     string
	header     http.Header
	http       *http.Client
	retries    int
	retryDelay time.Duration
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
	}
	if c.http == nil {
		c.http = http.DefaultClient
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
	return readStream(ctx, resp.Body, onDelta)
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
		return nil, &Error{Kind: KindProtocol, Message: "decode models: " + err.Error()}
	}
	return list.Data, nil
}

type chatBody struct {
	MaxTokens uint64     `json:"max_tokens,omitempty"`
	Model     string     `json:"model"`
	Messages  []Message  `json:"messages"`
	Tools     []Tool     `json:"tools,omitempty"`
	Stream    bool       `json:"stream"`
	Reasoning *reasoning `json:"reasoning,omitempty"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

func newChatBody(req Request) chatBody {
	body := chatBody{Model: req.Model, Messages: req.Messages, Tools: req.Tools, Stream: true, MaxTokens: req.MaxTokens}
	if req.ReasoningEffort != "" {
		body.Reasoning = &reasoning{Effort: req.ReasoningEffort}
	}
	return body
}

// send performs a request, retrying transient failures with exponential
// backoff. Retries happen only before a successful response is returned, so
// a stream that has started is never replayed.
func (c *Client) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.do(ctx, method, path, body)
		if err == nil {
			return resp, nil
		}
		if e, ok := errors.AsType[*Error](err); !ok || !e.Retryable() || attempt == c.retries {
			return nil, err
		}
		select {
		case <-time.After(c.retryDelay << attempt):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Kind: KindUnreachable, Message: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, statusError(resp.StatusCode, snippet)
	}
	return resp, nil
}
