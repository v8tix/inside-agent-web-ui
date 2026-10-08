package devtools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// observerScript is the observer, as the init script that puts it in each new page.
func observerScript() string { return script("observer") }

// installLate puts the observer in a page that is already showing a reply, for
// when the init script did not run. What it records starts too late to time the
// reply, which the result says (ObserverLate).
func installLate() string {
	return "// eval:installLate\n() => {\n" + observerScript() + "\nwindow.__agentEval.markLate();\nreturn window.__agentEval.collect();\n}"
}

// observed is what the observer's collect returns.
type observed struct {
	KeydownAt *float64 `json:"keydownAt"`
	StopAt    *float64 `json:"stopAt"`
	Updates   []struct {
		At    float64 `json:"at"`
		Chars int     `json:"chars"`
	} `json:"updates"`
	Statuses []struct {
		At     float64 `json:"at"`
		Status string  `json:"status"`
	} `json:"statuses"`
	Scrolls []struct {
		At           float64 `json:"at"`
		FromBottomPx float64 `json:"fromBottomPx"`
		Scrollable   bool    `json:"scrollable"`
	} `json:"scrolls"`
	Late    bool `json:"late"`
	Message *struct {
		ID               *string `json:"id"`
		Status           *string `json:"status"`
		TTFT             *string `json:"ttft"`
		Duration         *string `json:"duration"`
		PromptTokens     *string `json:"promptTokens"`
		CompletionTokens *string `json:"completionTokens"`
		ToolCalls        *string `json:"toolCalls"`
	} `json:"message"`
	VisibleText string `json:"visibleText"`
	Unsafe      *struct {
		Scripts  int `json:"scripts"`
		Handlers int `json:"handlers"`
		External int `json:"external"`
	} `json:"unsafe"`
}

// Collect returns what the browser observed since the page was opened: the
// observer's record of the reply, the console, and the requests.
func (d *Driver) Collect(ctx context.Context) (domain.UIObservations, error) {
	var rec *observed
	if err := d.tab.eval(ctx, script("collect"), &rec); err != nil {
		return domain.UIObservations{}, err
	}
	if rec == nil { // the init script did not run
		d.log.Warn("the observer is not in the page; installing it late")
		if err := d.tab.eval(ctx, installLate(), &rec); err != nil {
			return domain.UIObservations{}, err
		}
		if rec == nil {
			return domain.UIObservations{}, errors.New("the observer could not be installed in the page")
		}
	}

	obs := domain.UIObservations{ObserverLate: rec.Late, VisibleText: rec.VisibleText}
	if rec.KeydownAt != nil {
		obs.KeydownAt = *rec.KeydownAt
	}
	if rec.StopAt != nil {
		obs.StopAt = *rec.StopAt
	}
	for _, u := range rec.Updates {
		obs.Updates = append(obs.Updates, domain.Update{At: u.At, Chars: u.Chars})
	}
	for _, s := range rec.Statuses {
		obs.Statuses = append(obs.Statuses, domain.StatusChange{At: s.At, Status: domain.ReplyStatus(s.Status)})
	}
	for _, s := range rec.Scrolls {
		obs.Scrolls = append(obs.Scrolls, domain.ScrollSample{At: s.At, FromBottomPx: s.FromBottomPx, Scrollable: s.Scrollable})
	}
	if rec.Unsafe != nil {
		obs.Unsafe = domain.BodyFindings{Scripts: rec.Unsafe.Scripts, EventHandlerAttrs: rec.Unsafe.Handlers, ExternalImages: rec.Unsafe.External}
	}
	if m := rec.Message; m != nil {
		obs.Message = domain.MessageInfo{
			TTFTMs:           atoi64(m.TTFT),
			DurationMs:       atoi64(m.Duration),
			PromptTokens:     atoi(m.PromptTokens),
			CompletionTokens: atoi(m.CompletionTokens),
		}
		if m.ID != nil {
			obs.Message.ID = *m.ID
		}
		if m.Status != nil {
			obs.Message.Status = domain.ReplyStatus(*m.Status)
		}
		if m.ToolCalls != nil && *m.ToolCalls != "" {
			obs.Message.ToolCalls = strings.Split(*m.ToolCalls, ",")
		}
	}

	console, err := d.console(ctx)
	if err != nil {
		return domain.UIObservations{}, fmt.Errorf("reading the console: %w", err)
	}
	obs.Console = console
	requests, err := d.requests(ctx)
	if err != nil {
		return domain.UIObservations{}, fmt.Errorf("reading the network log: %w", err)
	}
	obs.Requests = requests
	return obs, nil
}

func atoi64(s *string) *int64 {
	if s == nil {
		return nil
	}
	n, err := strconv.ParseInt(*s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func atoi(s *string) *int {
	n := atoi64(s)
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// pagePattern finds "Page 2 of 3" in the footer a paged listing prints.
var pagePattern = regexp.MustCompile(`Page (\d+) of (\d+)`)

// maxPages bounds a listing, so a page that logs without end cannot keep the
// evaluator reading it for ever. Fifty pages are ten thousand entries, which is
// more than an attempt on a page that works leaves behind.
const maxPages = 50

// paged reads the pages of a listing tool, up to maxPages of them, and returns
// the text of each. A listing that is longer is cut there, with a warning: what
// the page logged after that is not in the observations.
func (d *Driver) paged(ctx context.Context, tool string) ([]string, error) {
	var pages []string
	total := 0
	for idx := range maxPages {
		res, err := d.tab.call(ctx, tool, map[string]any{"pageIdx": idx, "pageSize": 200})
		if err != nil {
			return nil, err
		}
		pages = append(pages, res.Text)
		m := pagePattern.FindStringSubmatch(res.Text)
		if m == nil {
			return pages, nil
		}
		current, _ := strconv.Atoi(m[1])
		total, _ = strconv.Atoi(m[2])
		if current >= total {
			return pages, nil
		}
	}
	d.log.Warn("a listing was cut at the page limit", "tool", tool, "pages", len(pages), "pages_listed", total)
	return pages, nil
}

func (d *Driver) console(ctx context.Context) ([]domain.ConsoleMessage, error) {
	pages, err := d.paged(ctx, "list_console_messages")
	if err != nil {
		return nil, err
	}
	var out []domain.ConsoleMessage
	for _, text := range pages {
		for _, m := range ParseConsole(text) {
			out = append(out, domain.ConsoleMessage{Type: m.Type, Text: m.Text})
		}
	}
	return out, nil
}

func (d *Driver) requests(ctx context.Context) ([]domain.NetworkRequest, error) {
	pages, err := d.paged(ctx, "list_network_requests")
	if err != nil {
		return nil, err
	}
	var out []domain.NetworkRequest
	for _, text := range pages {
		for _, r := range ParseRequests(text) {
			out = append(out, domain.NetworkRequest{Method: r.Method, URL: r.URL, Status: r.Status, Failed: r.Failure != ""})
		}
	}
	return out, nil
}
