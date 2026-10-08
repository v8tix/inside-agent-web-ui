package devtools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// tab is the browser tab a driver works in. Every tool call names the tab, so
// that one driver never acts on another's page.
type tab struct {
	s  *Session
	id int
}

func (t *tab) call(ctx context.Context, tool string, args map[string]any, opts ...CallOption) (Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	args["pageId"] = t.id
	return t.s.Call(ctx, tool, args, opts...)
}

// snapshot reads the accessibility tree. An empty tree is an error: a page that
// has loaded has at least a root.
func (t *tab) snapshot(ctx context.Context) (Snapshot, error) {
	res, err := t.call(ctx, "take_snapshot", nil)
	if err != nil {
		return Snapshot{}, err
	}
	snap := ParseSnapshot(res.Text)
	if len(snap.Nodes) == 0 {
		return Snapshot{}, fmt.Errorf("the page snapshot has no elements: %q", shorten(res.Text, 120))
	}
	return snap, nil
}

func (t *tab) click(ctx context.Context, uid string) error {
	_, err := t.call(ctx, "click", map[string]any{"uid": uid})
	return err
}

func (t *tab) fill(ctx context.Context, uid, value string) error {
	_, err := t.call(ctx, "fill", map[string]any{"uid": uid, "value": value})
	return err
}

func (t *tab) press(ctx context.Context, key string) error {
	_, err := t.call(ctx, "press_key", map[string]any{"key": key})
	return err
}

// typeText sends keystrokes to the element that has focus, and presses
// submitKey after them when it is not empty.
func (t *tab) typeText(ctx context.Context, text, submitKey string) error {
	args := map[string]any{"text": text}
	if submitKey != "" {
		args["submitKey"] = submitKey
	}
	_, err := t.call(ctx, "type_text", args)
	return err
}

// navigate goes to url. initScript, when set, runs in the new document before
// any of the page's own code: that is how the observer gets in (see observer.js).
func (t *tab) navigate(ctx context.Context, url, initScript string) error {
	args := map[string]any{"type": "url", "url": url}
	if initScript != "" {
		args["initScript"] = initScript
	}
	_, err := t.call(ctx, "navigate_page", args)
	return err
}

// eval runs a function in the page and decodes the JSON it returns into out.
// It does not wait for the page to settle, which a page that is streaming never
// does: the tool would wait for several seconds on every call.
func (t *tab) eval(ctx context.Context, function string, out any) error {
	res, err := t.call(ctx, "evaluate_script", map[string]any{"function": function, "waitForStableDom": false})
	if err != nil {
		return fmt.Errorf("%s: %w", scriptMarker(function), err)
	}
	raw, err := ParseScriptResult(res.Text)
	if err != nil {
		return fmt.Errorf("%s: %w", scriptMarker(function), err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: reading the result: %w", scriptMarker(function), err)
	}
	return nil
}

// clickNamed finds an element by role and name in a fresh snapshot and clicks
// it. A uid is only good for the snapshot it came from, and the page may change
// between reading and clicking, so a click that finds the uid gone is tried once
// more with a new snapshot.
func (t *tab) clickNamed(ctx context.Context, q Query) error { return t.clickNth(ctx, q, 0) }

// clickNth clicks the element that comes after skip others that match, for
// pages whose elements share a name.
func (t *tab) clickNth(ctx context.Context, q Query, skip int) error {
	var lastErr error
	for range 2 {
		snap, err := t.snapshot(ctx)
		if err != nil {
			return err
		}
		node, err := snap.RequireNth(q, skip)
		if err != nil {
			return err
		}
		lastErr = t.click(ctx, node.UID)
		if !isStale(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// isStale reports whether a tool said that an element is no longer on the page.
func isStale(err error) bool {
	var toolErr *ToolError
	return errors.As(err, &toolErr) && strings.Contains(toolErr.Message, "not found")
}
