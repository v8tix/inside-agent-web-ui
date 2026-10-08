package adkrest

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"iter"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

var _ resource.AgentGateway = (*Client)(nil)

// maxErrorBody caps how much of an error response is kept for logs.
const maxErrorBody = 512

// dialTimeout is how long the default client may take to connect to the agent.
const dialTimeout = 5 * time.Second

// HTTPError is a non-success answer from the agent. Body is for logs: it is
// whatever the agent said and may contain internals.
type HTTPError struct {
	Status int
	Body   string
}

// Error implements error.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("agent returned HTTP %d: %s", e.Status, e.Body)
}

// Client implements the core's AgentGateway over the ADK REST API.
type Client struct {
	base    string
	app     string
	timeout time.Duration
	http    *http.Client
}

// NewClient builds a client for cfg. A nil httpClient gets a default one that
// bounds connecting but not the stream itself: a reply may legitimately take
// minutes, and cfg.RequestTimeout bounds each run instead.
func NewClient(cfg config.Agent, httpClient *http.Client) *Client {
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
		// Every reply being written holds a connection to the one agent. The default
		// keeps two idle, and every run beyond them would dial anew.
		transport.MaxIdleConnsPerHost = 64
		httpClient = &http.Client{Transport: transport}
	}
	return &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		app:     cfg.AppName,
		timeout: cfg.RequestTimeout,
		http:    httpClient,
	}
}

type runRequest struct {
	AppName    string     `json:"appName"`
	UserID     string     `json:"userId"`
	SessionID  string     `json:"sessionId"`
	NewMessage newMessage `json:"newMessage"`
	Streaming  bool       `json:"streaming"`
}

type newMessage struct {
	Role  string     `json:"role"`
	Parts []textPart `json:"parts"`
}

type textPart struct {
	Text string `json:"text"`
}

// Run implements resource.AgentGateway.
//
// The ADK server needs a session before it will run a turn. Rather than check
// first, Run runs and, on the 404 that means "no such session" (a new
// conversation, or an agent that restarted and lost its in-memory sessions),
// creates the session and retries once.
//
// Pattern: Lazy Session, One Retry
// Pattern: Cancel on Early Exit
func (c *Client) Run(ctx context.Context, req domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		runCtx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel() // also cancels the upstream request when the caller stops iterating

		body, err := c.open(runCtx, req)
		if err != nil {
			yield(domain.AgentEvent{}, c.classify(ctx, runCtx, err))
			return
		}
		defer func() { _ = body.Close() }()

		for ev, err := range parseStream(body) {
			if err != nil {
				yield(domain.AgentEvent{}, c.classify(ctx, runCtx, err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// classify turns context errors into what they mean: the caller stopping, or
// the run taking too long. Anything else passes through.
func (c *Client) classify(caller, run context.Context, err error) error {
	switch {
	case caller.Err() != nil:
		return caller.Err()
	case run.Err() != nil:
		return fmt.Errorf("agent run timed out after %s: %w", c.timeout, context.DeadlineExceeded)
	}
	return err
}

func (c *Client) open(ctx context.Context, req domain.AgentRequest) (io.ReadCloser, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.postRun(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
		}
		if resp.StatusCode == http.StatusOK {
			if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("unexpected content type %q from agent (expected text/event-stream)",
					resp.Header.Get("Content-Type"))
			}
			return resp.Body, nil
		}

		httpErr := readHTTPError(resp)
		if resp.StatusCode == http.StatusNotFound && attempt == 0 && strings.Contains(httpErr.Body, "session not found") {
			if err := c.createSession(ctx, req); err != nil {
				return nil, fmt.Errorf("creating session: %w", err)
			}
			continue
		}
		return nil, classifyStatus(httpErr)
	}
}

func (c *Client) postRun(ctx context.Context, req domain.AgentRequest) (*http.Response, error) {
	payload, err := json.Marshal(runRequest{
		AppName:    c.app,
		UserID:     req.UserID.String(),
		SessionID:  req.ConversationID.String(),
		NewMessage: newMessage{Role: "user", Parts: []textPart{{Text: req.Text}}},
		Streaming:  true,
	})
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/run_sse", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	return c.http.Do(hr)
}

// createSession makes the agent-side session for a conversation. ADK answers
// 500 "already exists" when it is there already (for example another request
// created it first); that is fine.
func (c *Client) createSession(ctx context.Context, req domain.AgentRequest) error {
	target := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s",
		c.base, url.PathEscape(c.app), req.UserID, req.ConversationID)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
	}
	if resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return nil
	}
	httpErr := readHTTPError(resp)
	if strings.Contains(httpErr.Body, "already exists") {
		return nil
	}
	return classifyStatus(httpErr)
}

// Ping implements resource.AgentGateway.
func (c *Client) Ping(ctx context.Context) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/list-apps", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(readHTTPError(resp))
	}
	defer func() { _ = resp.Body.Close() }()

	var apps []string
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &apps); err != nil {
		return fmt.Errorf("decoding list-apps response: %w", err)
	}
	if !slices.Contains(apps, c.app) {
		return fmt.Errorf("agent does not serve app %q (it serves %q)", c.app, apps)
	}
	return nil
}

// readHTTPError reads (a bounded part of) an error response and closes it.
func readHTTPError(resp *http.Response) *HTTPError {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}

// classifyStatus marks gateway-class failures as "agent unavailable".
func classifyStatus(e *HTTPError) error {
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Errorf("%w: %w", domain.ErrAgentUnavailable, e)
	}
	return e
}
