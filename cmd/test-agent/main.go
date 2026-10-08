// Command test-agent runs a scripted ADK agent: the real ADK REST API around a
// model that answers from a script. Start it like any ADK agent:
//
//	test-agent web --port=9093 api
//
// The ADK launcher listens on 127.0.0.1 only; add --host=0.0.0.0 to be reached
// from another container (the Docker image does). It also ends a reply that has
// streamed for two minutes; "api --sse-write-timeout=10m" raises that.
//
// "test-agent healthcheck [url]" exits 0 when the agent answers, for container
// health checks in images that have no curl.
//
// The pace of its replies can be changed in .env (written down in .env.example;
// a variable in the environment wins, as it does in the container), for tests that
// need a long reply (stopping one, scrolling while one streams):
//
//	TEST_AGENT_WORD_DELAY   pause between words of ordinary replies (default 10ms)
//	TEST_AGENT_SLOW_DELAY   pause between the chunks of a #slow reply (default 100ms)
//	TEST_AGENT_SLOW_CHUNKS  how many chunks a #slow reply has (default 50)
//	TEST_AGENT_FIXTURE      a file scripting the answers to the evaluator's self-test problems
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/console"
	"google.golang.org/adk/v2/cmd/launcher/universal"
	"google.golang.org/adk/v2/cmd/launcher/web"
	"google.golang.org/adk/v2/cmd/launcher/web/api"
	"google.golang.org/adk/v2/cmd/launcher/web/webui"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

const defaultHealthURL = "http://127.0.0.1:9093/api/list-apps"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}

	lookup, err := envcfg.EnvLookup(".env")
	if err != nil {
		log.Fatalf("test-agent: %v", err)
	}
	opts, err := modelOptions(lookup)
	if err != nil {
		log.Fatalf("test-agent: %v", err)
	}
	root, err := testagent.BuildAgent(testagent.NewModel(opts...))
	if err != nil {
		log.Fatalf("building agent: %v", err)
	}
	cfg := &launcher.Config{AgentLoader: agent.NewSingleLoader(root)}
	l := universal.NewLauncher(console.NewLauncher(), web.NewLauncher(webui.NewLauncher(), api.NewLauncher()))
	if err := l.Execute(context.Background(), cfg, os.Args[1:]); err != nil {
		log.Fatalf("run failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

// healthcheck asks the agent for its app list and expects testagent.AppName.
func healthcheck(args []string) int {
	url := defaultHealthURL
	if len(args) > 0 {
		url = args[0]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), testagent.AppName) {
		fmt.Fprintf(os.Stderr, "unhealthy: status %d, body %q\n", resp.StatusCode, body)
		return 1
	}
	return 0
}

// modelOptions reads the pacing settings. A value that is set must be valid, so
// a typo does not quietly leave a test with a reply of the wrong length.
func modelOptions(lookup func(string) (string, bool)) ([]testagent.Option, error) {
	duration := func(key string, fallback time.Duration) (time.Duration, error) {
		raw, ok := lookup(key)
		if !ok || raw == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("%s=%q is not a duration such as 100ms", key, raw)
		}
		return d, nil
	}
	word, err := duration("TEST_AGENT_WORD_DELAY", testagent.DefaultWordDelay)
	if err != nil {
		return nil, err
	}
	slow, err := duration("TEST_AGENT_SLOW_DELAY", testagent.DefaultSlowDelay)
	if err != nil {
		return nil, err
	}
	chunks := testagent.DefaultSlowChunks
	if raw, ok := lookup("TEST_AGENT_SLOW_CHUNKS"); ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("TEST_AGENT_SLOW_CHUNKS=%q is not a positive number", raw)
		}
		chunks = n
	}
	opts := []testagent.Option{testagent.WithWordDelay(word), testagent.WithSlow(slow, chunks)}
	if path, ok := lookup("TEST_AGENT_FIXTURE"); ok && path != "" {
		f, err := testagent.LoadFixture(path)
		if err != nil {
			return nil, fmt.Errorf("TEST_AGENT_FIXTURE: %w", err)
		}
		opts = append(opts, testagent.WithFixture(f))
	}
	return opts, nil
}
