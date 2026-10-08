package gaia_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/gaia"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

const canary = "hf_CANARY_0123456789abcdef"

// fakeHub is a Hugging Face datasets server and file host in one, plus a second
// host standing for the CDN that downloads are redirected to.
type fakeHub struct {
	t        *testing.T
	server   *httptest.Server
	cdn      *httptest.Server
	files    map[string][]byte // "config/split/name" -> content
	override func(w http.ResponseWriter, r *http.Request) bool

	mu       sync.Mutex
	requests []seen
}

type seen struct {
	host, path, auth string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, files: map[string][]byte{}}
	h.cdn = httptest.NewServer(http.HandlerFunc(h.serveCDN))
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.cdn.Close)
	t.Cleanup(h.server.Close)
	return h
}

func (h *fakeHub) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, seen{host: r.Host, path: r.URL.Path, auth: r.Header.Get("Authorization")})
}

func (h *fakeHub) count(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.requests {
		if strings.HasPrefix(r.path, prefix) {
			n++
		}
	}
	return n
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.record(r)
	if h.override != nil && h.override(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/parquet":
		if r.Header.Get("Authorization") != "Bearer "+canary {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":"Invalid token %s"}`, r.Header.Get("Authorization")) // a server that echoes it
			return
		}
		type file struct {
			Dataset  string `json:"dataset"`
			Config   string `json:"config"`
			Split    string `json:"split"`
			URL      string `json:"url"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		}
		var list []file
		for key, content := range h.files {
			parts := strings.Split(key, "/")
			list = append(list, file{"gaia-benchmark/GAIA", parts[0], parts[1], h.server.URL + "/files/" + key, parts[2], int64(len(content))})
		}
		_ = json.MarshalWrite(w, map[string]any{"parquet_files": list, "pending": []any{}, "failed": []any{}})
	case strings.HasPrefix(r.URL.Path, "/files/"):
		key := strings.TrimPrefix(r.URL.Path, "/files/")
		if _, ok := h.files[key]; !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, h.cdn.URL+"/blob/"+key+"?sig=signed", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) serveCDN(w http.ResponseWriter, r *http.Request) {
	h.record(r)
	key := strings.TrimPrefix(r.URL.Path, "/blob/")
	_, _ = w.Write(h.files[key])
}

func (h *fakeHub) fetcher(logs *bytes.Buffer) *gaia.Fetcher {
	f := &gaia.Fetcher{ServerURL: h.server.URL, HubURL: h.server.URL, Token: envcfg.Secret(canary)}
	if logs != nil {
		f.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	return f
}

// parquetBytes is a parquet file of n rows whose task ids start with prefix.
func parquetBytes(t *testing.T, prefix string, n int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.parquet")
	writeParquet(t, path, gaiaRows(prefix, "1", n), 100)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestFetch_Downloads(t *testing.T) {
	hub := newFakeHub(t)
	hub.files["2023_level1/validation/0000.parquet"] = parquetBytes(t, "a", 3)
	hub.files["2023_level1/validation/0001.parquet"] = parquetBytes(t, "b", 2)
	hub.files["2023_level1/test/0000.parquet"] = parquetBytes(t, "c", 4)       // another split
	hub.files["2023_level2/validation/0000.parquet"] = parquetBytes(t, "d", 5) // another config
	dir := t.TempDir()

	got, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"2023_level1"}, "validation", dir)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{"0000.parquet", "0001.parquet"}, []string{got[0].Name, got[1].Name})
	for _, f := range got {
		assert.True(t, f.Downloaded)
		assert.Equal(t, "2023_level1", f.Config)
		assert.Equal(t, "validation", f.Split)
		assert.Positive(t, f.Bytes)
	}

	t.Run("only what was asked for is on disk, under the documented layout", func(t *testing.T) {
		var found []string
		require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, p)
				found = append(found, filepath.ToSlash(rel))
			}
			return err
		}))
		assert.ElementsMatch(t, []string{"2023_level1/validation/0000.parquet", "2023_level1/validation/0001.parquet"}, found,
			"no other split, no other config, no leftover temporary files")
	})
	t.Run("the downloaded files are readable by the loader", func(t *testing.T) {
		problems, err := collect(t, gaia.NewParquet(dir, []string{"2023_level1"}, "validation").Problems(t.Context()))

		require.NoError(t, err)
		assert.Len(t, problems, 5)
	})
}

func TestFetch_SkipsWhatIsAlreadyThere(t *testing.T) {
	hub := newFakeHub(t)
	hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 3)
	dir := t.TempDir()
	f := hub.fetcher(nil)
	_, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)
	require.NoError(t, err)
	before := hub.count("/files/")

	again, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)

	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.False(t, again[0].Downloaded)
	assert.Equal(t, before, hub.count("/files/"), "the file itself was not asked for again")

	t.Run("a file of another size is replaced", func(t *testing.T) {
		path := filepath.Join(dir, "c", "validation", "0000.parquet")
		require.NoError(t, os.WriteFile(path, []byte("half a file"), 0o644))

		redo, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)

		require.NoError(t, err)
		assert.True(t, redo[0].Downloaded)
		got, _ := os.ReadFile(path)
		assert.Equal(t, hub.files["c/validation/0000.parquet"], got)
	})
}

func TestFetch_Problems(t *testing.T) {
	good := func(t *testing.T) *fakeHub {
		hub := newFakeHub(t)
		hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 2)
		return hub
	}

	t.Run("no token", func(t *testing.T) {
		hub := good(t)
		f := hub.fetcher(nil)
		f.Token = ""

		_, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.ErrorIs(t, err, gaia.ErrNoToken)
		assert.Contains(t, err.Error(), "HF_TOKEN is not set")
		assert.Zero(t, hub.count("/"), "nothing was sent anywhere")
	})
	t.Run("a refused token", func(t *testing.T) {
		hub := good(t)
		f := hub.fetcher(nil)
		f.Token = "hf_wrong_token_value"

		_, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "was refused")
		assert.ErrorContains(t, err, "accepted the dataset's terms")
		assert.NotContains(t, err.Error(), "hf_wrong_token_value", "even though the server echoed it back")
	})
	t.Run("an unknown dataset", func(t *testing.T) {
		hub := good(t)
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			http.NotFound(w, r)
			return true
		}

		_, err := hub.fetcher(nil).Fetch(t.Context(), "me/none", []string{"c"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "check HF_GAIA_DATASET")
	})
	t.Run("a server error", func(t *testing.T) {
		hub := good(t)
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "HTTP 502")
	})
	t.Run("a config that is not listed", func(t *testing.T) {
		_, err := good(t).fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"2023_all"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "no parquet files for config 2023_all, split validation")
		assert.ErrorContains(t, err, "HF_GAIA_LEVELS")
	})
	t.Run("a listing that is still converting", func(t *testing.T) {
		hub := good(t)
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/parquet" {
				return false
			}
			_, _ = w.Write([]byte(`{"parquet_files":[],"pending":[{"config":"c"}],"failed":[]}`))
			return true
		}

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "still being converted")
	})
	t.Run("a listing that cannot be read", func(t *testing.T) {
		hub := good(t)
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			_, _ = w.Write([]byte("<html>sign in</html>"))
			return true
		}

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.ErrorContains(t, err, "reading the file list")
	})
	t.Run("a file name that tries to leave the directory", func(t *testing.T) {
		hub := good(t)
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/parquet" {
				return false
			}
			_, _ = w.Write([]byte(`{"parquet_files":[{"config":"c","split":"validation","url":"http://x/y","filename":"../../evil.parquet","size":5}]}`))
			return true
		}
		dir := t.TempDir()

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)

		require.ErrorContains(t, err, "not a plain parquet file name")
		_, statErr := os.Stat(filepath.Join(dir, "..", "..", "evil.parquet"))
		assert.True(t, os.IsNotExist(statErr))
	})
	t.Run("a page of HTML saved as a parquet file", func(t *testing.T) {
		hub := newFakeHub(t)
		hub.files["c/validation/0000.parquet"] = []byte("<html>please sign in to continue, this is not data at all</html>")
		dir := t.TempDir()

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)

		require.ErrorContains(t, err, "not a parquet file")
		assert.NoFileExists(t, filepath.Join(dir, "c", "validation", "0000.parquet"))
		assertNoTemporaryFiles(t, dir)
	})
	t.Run("a download that ends early", func(t *testing.T) {
		hub := good(t)
		full := hub.files["c/validation/0000.parquet"]
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/files/") {
				return false
			}
			_, _ = w.Write(full[:len(full)/2]) // a short body behind a 200
			return true
		}
		dir := t.TempDir()

		_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)

		require.ErrorContains(t, err, "the listing says")
		assert.NoFileExists(t, filepath.Join(dir, "c", "validation", "0000.parquet"), "no half file that looks whole")
		assertNoTemporaryFiles(t, dir)
	})
	t.Run("a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := good(t).fetcher(nil).Fetch(ctx, "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

		require.Error(t, err)
	})
}

func assertNoTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			assert.NotContains(t, filepath.Base(p), ".part-", "a temporary file was left behind")
		}
		return nil
	})
}

func TestFetch_TheTokenGoesOnlyWhereItBelongs(t *testing.T) {
	hub := newFakeHub(t)
	hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 2)

	_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())
	require.NoError(t, err)

	hub.mu.Lock()
	defer hub.mu.Unlock()
	var toServer, toCDN int
	for _, r := range hub.requests {
		switch {
		case strings.HasPrefix(r.path, "/blob/"):
			toCDN++
			assert.Empty(t, r.auth, "the CDN a download is redirected to must not get the token")
		default:
			toServer++
			assert.Equal(t, "Bearer "+canary, r.auth, r.path)
		}
	}
	assert.Equal(t, 2, toServer, "the listing and the first leg of the download")
	assert.Equal(t, 1, toCDN)
}

func TestFetch_ListingPointingElsewhereGetsNoToken(t *testing.T) {
	hub := newFakeHub(t)
	data := parquetBytes(t, "x", 1)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.record(r)
		_, _ = w.Write(data)
	}))
	t.Cleanup(other.Close)
	hub.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/parquet" {
			return false
		}
		_ = json.MarshalWrite(w, map[string]any{"parquet_files": []map[string]any{{
			"config": "c", "split": "validation", "url": other.URL + "/file", "filename": "0000.parquet", "size": len(data),
		}}})
		return true
	}

	_, err := hub.fetcher(nil).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

	require.NoError(t, err)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, r := range hub.requests {
		if r.path == "/file" {
			assert.Empty(t, r.auth, "a host the listing names is not a host the token was given for")
		}
	}
}

func TestFetch_TokenNeverLeaks(t *testing.T) {
	hub := newFakeHub(t)
	hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 2)
	dir := t.TempDir()
	var logs bytes.Buffer
	f := hub.fetcher(&logs)

	var errs []string
	run := func(name string, mutate func()) {
		if mutate != nil {
			mutate()
		}
		_, err := f.Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", dir)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
		}
	}
	run("success", nil)
	run("server error echoing the token", func() {
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"error":"boom for %s"}`, canary)
			return true
		}
	})
	run("a refused token", func() {
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"error":"token %s is not allowed"}`, canary)
			return true
		}
	})
	run("a body that echoes it", func() {
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			_, _ = w.Write([]byte(canary + canary))
			return true
		}
	})

	assert.NotEmpty(t, errs, "the failing cases did fail")
	for _, e := range errs {
		assert.NotContains(t, e, canary, "an error message")
	}
	assert.NotContains(t, logs.String(), canary, "the log")
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v", f, f, f), canary, "the fetcher printed")

	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(p)
		require.NoError(t, readErr)
		assert.NotContains(t, string(content), canary, "a file on disk: %s", p)
		assert.NotContains(t, filepath.Base(p), canary)
		return nil
	}))
}

func TestFetch_RedirectsKeepTheTokenOnTrustedHostsOnly(t *testing.T) {
	// Go forwards Authorization to another port of the same host name and to
	// subdomains. The fake CDN is exactly that case (127.0.0.1 on another port).
	hub := newFakeHub(t)
	hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 1)

	t.Run("a redirect back to a trusted host keeps it", func(t *testing.T) {
		hub.mu.Lock()
		hub.requests = nil
		hub.mu.Unlock()
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/hop" {
				http.Redirect(w, r, "/parquet?dataset=gaia-benchmark/GAIA", http.StatusFound)
				return true
			}
			return false
		}
		f := hub.fetcher(nil)

		resp, err := f.GetForTest(t.Context(), hub.server.URL+"/hop")

		require.NoError(t, err)
		_ = resp.Body.Close()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		require.Len(t, hub.requests, 2)
		assert.Equal(t, "Bearer "+canary, hub.requests[1].auth)
	})
	t.Run("a redirect to another port of the same host loses it", func(t *testing.T) {
		hubURL, err := url.Parse(hub.server.URL)
		require.NoError(t, err)
		cdnURL, err := url.Parse(hub.cdn.URL)
		require.NoError(t, err)
		require.Equal(t, hubURL.Hostname(), cdnURL.Hostname(), "the hub and the CDN share a host name")
		require.NotEqual(t, hubURL.Port(), cdnURL.Port(), "and listen on two ports")
		hub.mu.Lock()
		hub.requests = nil
		hub.mu.Unlock()
		hub.override = nil // the hub sends a download on to the CDN

		resp, err := hub.fetcher(nil).GetForTest(t.Context(), hub.server.URL+"/files/c/validation/0000.parquet")

		require.NoError(t, err)
		_ = resp.Body.Close()
		hub.mu.Lock()
		defer hub.mu.Unlock()
		require.Len(t, hub.requests, 2)
		assert.Equal(t, "Bearer "+canary, hub.requests[0].auth, "the hub is what the token is for")
		assert.Equal(t, cdnURL.Host, hub.requests[1].host, "the second request went to the CDN")
		assert.Empty(t, hub.requests[1].auth, "and took no token there")
	})
	t.Run("a runaway chain of redirects stops", func(t *testing.T) {
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, "/again", http.StatusFound)
			return true
		}

		_, err := hub.fetcher(nil).GetForTest(t.Context(), hub.server.URL+"/again")

		require.ErrorContains(t, err, "stopped after 10 redirects")
	})
	t.Run("a client of the caller keeps its own redirect rule", func(t *testing.T) {
		hub.override = func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return true
		}
		f := hub.fetcher(nil)
		f.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

		resp, err := f.GetForTest(t.Context(), hub.server.URL+"/start")

		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode)
	})
}

// authSent makes a fetcher that was given the address base ask for rawURL, and
// returns the Authorization header that arrived. The hub is a server on a network
// in memory, and its client takes every request to it, over http and over https,
// whatever host and port the URL names: the address does not have to exist.
func authSent(t *testing.T, base, rawURL string) string {
	t.Helper()
	arrived := make(chan string, 1)
	hub := httptest.NewTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		arrived <- r.Header.Get("Authorization")
	}))
	f := &gaia.Fetcher{ServerURL: base, HubURL: base, Token: envcfg.Secret(canary), Client: hub.Client()}

	resp, err := f.GetForTest(t.Context(), rawURL)

	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return <-arrived
}

// The token was given for an address, and the scheme is part of it: the same host
// asked over http would be sent the token in the clear.
func TestFetch_TheSchemeIsPartOfTheAddress(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		asked      string
		want       string
	}{
		{"configured with https, asked over https", "https://hub.example", "https://hub.example/parquet", "Bearer " + canary},
		{"configured with https, asked over http", "https://hub.example", "http://hub.example/parquet", ""},
		{"configured with http, asked over https", "http://hub.example", "https://hub.example/parquet", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authSent(t, tc.configured, tc.asked))
		})
	}
}

// The token goes to an address as it is written. A default port written out names
// the same server, and is another address all the same: it gets no token.
func TestFetch_TheDefaultPortWrittenOutIsAnotherAddress(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		asked      string
		want       string
	}{
		{"https, asked as configured", "https://hub.example", "https://hub.example/parquet", "Bearer " + canary},
		{"https, with 443 written in the request", "https://hub.example", "https://hub.example:443/parquet", ""},
		{"https, with 443 written in the setting", "https://hub.example:443", "https://hub.example/parquet", ""},
		{"https, with 443 written in both", "https://hub.example:443", "https://hub.example:443/parquet", "Bearer " + canary},
		{"http, with 80 written in the request", "http://hub.example", "http://hub.example:80/parquet", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, authSent(t, tc.configured, tc.asked))
		})
	}
}

func TestFetch_SignedLinksStayOutOfErrors(t *testing.T) {
	hub := newFakeHub(t)
	hub.files["c/validation/0000.parquet"] = parquetBytes(t, "x", 1)
	hub.cdn.Close() // the download link now points at nothing
	var logs bytes.Buffer

	_, err := hub.fetcher(&logs).Fetch(t.Context(), "gaia-benchmark/GAIA", []string{"c"}, "validation", t.TempDir())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sig=signed", "the signature of the download link")
	assert.NotContains(t, logs.String(), "sig=signed")
	assert.Contains(t, err.Error(), "/blob/", "but the error still says where it failed")
}
