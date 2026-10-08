package main

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

func env(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

// checkHealth runs the health check on url and returns its exit status and what it
// wrote to standard error. The check writes there itself, so the test puts a file
// in its place for the length of the call.
func checkHealth(t *testing.T, url string) (status int, said string) {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	status = func() int {
		stderr := os.Stderr
		os.Stderr = file
		defer func() { os.Stderr = stderr }()
		return healthcheck([]string{url})
	}()
	require.NoError(t, file.Close())
	written, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	return status, string(written)
}

// The health check of the container asks the agent for its apps: it is healthy when
// the answer is a 200 that names the app within the first 64 KiB.
func TestHealthcheck(t *testing.T) {
	const read = 1 << 16 // how much of an answer the check reads
	listed := `["` + testagent.AppName + `"]`
	answering := func(status int, body string) func(*testing.T) string {
		return func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(srv.Close)
			return srv.URL + "/api/list-apps"
		}
	}
	tests := []struct {
		name     string
		agent    func(*testing.T) string // starts what answers, and returns the address to check
		want     int
		wantSaid string
	}{
		{"the app is named", answering(http.StatusOK, listed), 0, ""},
		{"the app is not named", answering(http.StatusOK, `["another_agent"]`), 1, "unhealthy: status 200"},
		{"another status", answering(http.StatusServiceUnavailable, listed), 1, "unhealthy: status 503"},
		{"nothing listens", func(t *testing.T) string {
			srv := httptest.NewServer(http.NotFoundHandler())
			srv.Close()
			return srv.URL
		}, 1, "unhealthy:"},
		{"the name ends within 64 KiB", answering(http.StatusOK, strings.Repeat(" ", read-len(listed))+listed), 0, ""},
		{"the name comes after 64 KiB", answering(http.StatusOK, strings.Repeat(" ", read)+listed), 1, "unhealthy: status 200"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, said := checkHealth(t, tc.agent(t))

			assert.Equal(t, tc.want, status)
			if tc.wantSaid == "" {
				assert.Zero(t, len(said), "a healthy agent is not written about: %.60q", said)
				return
			}
			assert.True(t, strings.HasPrefix(said, tc.wantSaid), "it says why: %.60q", said)
		})
	}
}

func TestModelOptions(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"nothing set keeps the defaults", nil, ""},
		{"empty values count as unset", map[string]string{"TEST_AGENT_SLOW_CHUNKS": "", "TEST_AGENT_SLOW_DELAY": ""}, ""},
		{"a longer slow reply", map[string]string{"TEST_AGENT_SLOW_CHUNKS": "200", "TEST_AGENT_SLOW_DELAY": "50ms"}, ""},
		{"a word delay of zero is allowed", map[string]string{"TEST_AGENT_WORD_DELAY": "0s"}, ""},
		{"a bad duration", map[string]string{"TEST_AGENT_SLOW_DELAY": "fast"}, "TEST_AGENT_SLOW_DELAY"},
		{"a negative duration", map[string]string{"TEST_AGENT_WORD_DELAY": "-1s"}, "TEST_AGENT_WORD_DELAY"},
		{"chunks that are not a number", map[string]string{"TEST_AGENT_SLOW_CHUNKS": "many"}, "TEST_AGENT_SLOW_CHUNKS"},
		{"no chunks at all", map[string]string{"TEST_AGENT_SLOW_CHUNKS": "0"}, "TEST_AGENT_SLOW_CHUNKS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := modelOptions(env(tc.env))

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, opts, 2)
		})
	}
}

// A setting that is accepted and does nothing is as bad as a typo, so the values
// are followed into the model: how many pieces a #slow reply has, and how long
// the model pauses between the pieces of a slow reply and between words.
func TestModelOptions_ValuesReachTheModel(t *testing.T) {
	ask := func(t *testing.T, settings map[string]string, prompt string) (pieces int, took time.Duration) {
		t.Helper()
		opts, err := modelOptions(env(settings))
		require.NoError(t, err)
		req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}}
		started := time.Now()
		for resp, err := range testagent.NewModel(opts...).GenerateContent(t.Context(), req, true) {
			require.NoError(t, err)
			if resp.Partial {
				pieces++
			}
		}
		return pieces, time.Since(started)
	}
	quick := map[string]string{"TEST_AGENT_WORD_DELAY": "0s", "TEST_AGENT_SLOW_DELAY": "0s"}
	with := func(extra map[string]string) map[string]string {
		out := maps.Clone(quick)
		maps.Copy(out, extra)
		return out
	}

	t.Run("a #slow reply has the pieces it is told to have", func(t *testing.T) {
		pieces, _ := ask(t, with(map[string]string{"TEST_AGENT_SLOW_CHUNKS": "7"}), "go on #slow")

		assert.Equal(t, 7, pieces)
	})
	t.Run("and fifty unless told otherwise, with no pause when told there is none", func(t *testing.T) {
		pieces, took := ask(t, quick, "go on #slow")

		assert.Equal(t, testagent.DefaultSlowChunks, pieces)
		assert.Less(t, took, 2*time.Second, "fifty pauses of the default length would take five seconds")
	})
	t.Run("the pause between its pieces is the one asked for", func(t *testing.T) {
		_, took := ask(t, with(map[string]string{"TEST_AGENT_SLOW_CHUNKS": "4", "TEST_AGENT_SLOW_DELAY": "40ms"}), "go on #slow")

		assert.GreaterOrEqual(t, took, 3*40*time.Millisecond, "three pauses between four pieces")
	})
	t.Run("the pause between words is the one asked for, and not the slow one", func(t *testing.T) {
		_, took := ask(t, map[string]string{"TEST_AGENT_WORD_DELAY": "30ms", "TEST_AGENT_SLOW_DELAY": "0s"}, "one two three four")

		assert.GreaterOrEqual(t, took, 3*30*time.Millisecond, "a pause between words")
	})
	t.Run("the slow pause does not slow ordinary replies", func(t *testing.T) {
		_, took := ask(t, map[string]string{"TEST_AGENT_WORD_DELAY": "0s", "TEST_AGENT_SLOW_DELAY": "300ms"}, "one two three four")

		assert.Less(t, took, 250*time.Millisecond)
	})
}

func TestModelOptions_Fixture(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "fixture.json")
	require.NoError(t, os.WriteFile(good, []byte(`{"version":1,"replies":{"a-1":{"text":"x"}}}`), 0o600))
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"version":1,"replies":{}}`), 0o600))

	t.Run("a fixture adds an option", func(t *testing.T) {
		opts, err := modelOptions(env(map[string]string{"TEST_AGENT_FIXTURE": good}))

		require.NoError(t, err)
		assert.Len(t, opts, 3)
	})
	t.Run("empty counts as unset", func(t *testing.T) {
		opts, err := modelOptions(env(map[string]string{"TEST_AGENT_FIXTURE": ""}))

		require.NoError(t, err)
		assert.Len(t, opts, 2)
	})
	t.Run("a fixture that cannot be read stops the agent starting", func(t *testing.T) {
		for _, path := range []string{filepath.Join(dir, "missing.json"), bad} {
			_, err := modelOptions(env(map[string]string{"TEST_AGENT_FIXTURE": path}))

			require.ErrorContains(t, err, "TEST_AGENT_FIXTURE", path)
		}
	})
}

// Settings live in .env, and .env.example is where they are written down: a key
// the test agent reads and the example lacks is a setting nobody can find.
func TestModelOptions_EveryKeyIsInTheExample(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)
	example := string(raw)

	var asked []string
	_, err = modelOptions(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	require.NoError(t, err)
	require.NotEmpty(t, asked)

	for _, key := range asked {
		documented := strings.Contains(example, "\n"+key+"=") || strings.Contains(example, "\n# "+key+"=")
		assert.True(t, documented, "%s is read by the test agent but not documented in .env.example", key)
	}
}
