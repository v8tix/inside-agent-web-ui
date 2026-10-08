package gaia

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

// ErrNoToken is returned when asked to download without a Hugging Face token.
var ErrNoToken = errors.New("HF_TOKEN is not set: downloading GAIA needs a Hugging Face token with access to the gated dataset (put it in .env)")

// Fetcher downloads GAIA's parquet files from Hugging Face.
//
// The token goes only to the hosts it was configured for (the datasets server
// and the hub), never to a host a listing or a redirect names, and it is never
// written to a log, an error or a file.
type Fetcher struct {
	// ServerURL and HubURL are HF_DATASETS_SERVER_URL and HF_HUB_URL.
	ServerURL, HubURL string
	Token             envcfg.Secret
	Client            *http.Client
	Logger            *slog.Logger
}

// FetchedFile is one file of a download.
type FetchedFile struct {
	Config, Split, Name string
	Bytes               int64
	// Downloaded is false when a file of the same size was already there.
	Downloaded bool
}

// parquetListing is the datasets server's answer to /parquet.
type parquetListing struct {
	ParquetFiles []struct {
		Dataset  string `json:"dataset"`
		Config   string `json:"config"`
		Split    string `json:"split"`
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"parquet_files"`
	Pending []any `json:"pending"`
	Failed  []any `json:"failed"`
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.parquet$`)

// Fetch downloads the parquet files of dataset for the given configs and split
// into dir/<config>/<split>/, and nothing else. A file that is already there with
// the same size is left alone. Each file is written under a temporary name and
// renamed when complete, so an interrupted download never leaves a half file
// that looks whole.
func (f *Fetcher) Fetch(ctx context.Context, dataset string, configs []string, split, dir string) ([]FetchedFile, error) {
	if f.Token.Reveal() == "" {
		return nil, ErrNoToken
	}
	listing, err := f.list(ctx, dataset)
	if err != nil {
		return nil, err
	}

	var out []FetchedFile
	for _, config := range configs {
		matched := 0
		for _, file := range listing.ParquetFiles {
			if file.Config != config || file.Split != split {
				continue
			}
			matched++
			if !safeName.MatchString(file.Filename) {
				return nil, fmt.Errorf("the listing names a file %q, which is not a plain parquet file name", file.Filename)
			}
			target := filepath.Join(dir, config, split, file.Filename)
			got, err := f.download(ctx, file.URL, target, file.Size)
			if err != nil {
				return out, fmt.Errorf("%s/%s/%s: %w", config, split, file.Filename, err)
			}
			out = append(out, FetchedFile{Config: config, Split: split, Name: file.Filename, Bytes: got.size, Downloaded: got.downloaded})
		}
		if matched == 0 {
			return out, f.noFiles(listing, dataset, config, split)
		}
	}
	return out, nil
}

func (f *Fetcher) noFiles(l parquetListing, dataset, config, split string) error {
	switch {
	case len(l.Pending) > 0:
		return fmt.Errorf("%s is still being converted to parquet by Hugging Face: try again in a few minutes", dataset)
	case len(l.Failed) > 0:
		return fmt.Errorf("part of %s could not be converted to parquet by Hugging Face", dataset)
	}
	return fmt.Errorf("no parquet files for config %s, split %s in %s: check HF_GAIA_LEVELS and GAIA_SPLIT", config, split, dataset)
}

// list asks the datasets server which parquet files a dataset has.
func (f *Fetcher) list(ctx context.Context, dataset string) (parquetListing, error) {
	endpoint := strings.TrimRight(f.ServerURL, "/") + "/parquet?dataset=" + url.QueryEscape(dataset)
	resp, err := f.get(ctx, endpoint)
	if err != nil {
		return parquetListing{}, fmt.Errorf("asking Hugging Face for the file list: %w", f.safe(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if err := f.statusError(resp, "the file list"); err != nil {
		return parquetListing{}, err
	}
	var listing parquetListing
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 16<<20), &listing); err != nil {
		return parquetListing{}, fmt.Errorf("reading the file list: %w", f.safe(err))
	}
	return listing, nil
}

type downloaded struct {
	size       int64
	downloaded bool
}

func (f *Fetcher) download(ctx context.Context, fileURL, target string, wantSize int64) (downloaded, error) {
	if info, err := os.Stat(target); err == nil && wantSize > 0 && info.Size() == wantSize {
		f.log().InfoContext(ctx, "already downloaded", "file", filepath.Base(target), "bytes", info.Size())
		return downloaded{size: info.Size()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return downloaded{}, err
	}

	resp, err := f.get(ctx, fileURL)
	if err != nil {
		return downloaded{}, f.safe(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := f.statusError(resp, "the file"); err != nil {
		return downloaded{}, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".part-*")
	if err != nil {
		return downloaded{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after a rename; cleans up after a failure
	n, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	switch {
	case copyErr != nil:
		return downloaded{}, fmt.Errorf("downloading: %w", f.safe(copyErr))
	case closeErr != nil:
		return downloaded{}, closeErr
	case wantSize > 0 && n != wantSize:
		return downloaded{}, fmt.Errorf("downloaded %d bytes but the listing says %d", n, wantSize)
	}
	if err := checkParquetMagic(tmp.Name()); err != nil {
		return downloaded{}, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return downloaded{}, err
	}
	f.log().InfoContext(ctx, "downloaded", "file", filepath.Base(target), "bytes", n)
	return downloaded{size: n, downloaded: true}, nil
}

// checkParquetMagic makes sure the file starts and ends with "PAR1", so a
// login page or an error body saved as a .parquet file is caught here.
func checkParquetMagic(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 12 {
		return errors.New("what was downloaded is not a parquet file (too short)")
	}
	head, tail := make([]byte, 4), make([]byte, 4)
	if _, err := file.ReadAt(head, 0); err != nil {
		return err
	}
	if _, err := file.ReadAt(tail, info.Size()-4); err != nil {
		return err
	}
	if string(head) != "PAR1" || string(tail) != "PAR1" {
		return errors.New("what was downloaded is not a parquet file (wrong magic bytes)")
	}
	return nil
}

// get makes a GET request. The token is attached only when the URL is on a host
// the fetcher was configured with.
func (f *Fetcher) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if f.trusts(req.URL) {
		req.Header.Set("Authorization", "Bearer "+f.Token.Reveal())
	}
	req.Header.Set("User-Agent", "agent-eval")
	return f.httpClient().Do(req)
}

const maxRedirects = 10

// httpClient is the configured client with a redirect rule added: the token
// follows a redirect only to a host it was configured for. Go's own rule also
// lets it reach another port of the same host name and any subdomain, which is
// more than a signed download link should get.
//
// Pattern: Token Scoped to Two Hosts
func (f *Fetcher) httpClient() *http.Client {
	base := f.Client
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !f.trusts(req.URL) {
			req.Header.Del("Authorization")
		}
		if base.CheckRedirect != nil {
			return base.CheckRedirect(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	return &client
}

// trusts reports whether u is at one of the two addresses the token was given
// for: the same scheme, and the same host and port as they are written, letter
// case aside. Nothing is worked out, so a default port written out is another
// address: with https://host configured, https://host:443 gets no token.
func (f *Fetcher) trusts(u *url.URL) bool {
	for _, base := range []string{f.ServerURL, f.HubURL} {
		if b, err := url.Parse(base); err == nil && b.Host != "" && strings.EqualFold(b.Host, u.Host) && b.Scheme == u.Scheme {
			return true
		}
	}
	return false
}

// statusError turns a bad status into a message that helps and leaks nothing.
func (f *Fetcher) statusError(resp *http.Response, what string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	detail := ""
	var body struct {
		Error string `json:"error"`
	}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil && json.Unmarshal(raw, &body) == nil {
		detail = f.redact(body.Error)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("access to %s was refused (HTTP %d): check HF_TOKEN, and that the account accepted the dataset's terms%s", what, resp.StatusCode, suffix(detail))
	case http.StatusNotFound:
		return fmt.Errorf("%s was not found on Hugging Face (HTTP 404): check HF_GAIA_DATASET%s", what, suffix(detail))
	}
	return fmt.Errorf("unexpected answer from Hugging Face: HTTP %d for %s%s", resp.StatusCode, what, suffix(detail))
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// safe removes what must not be shown from an error: the token, in case
// something echoed it, and the query of a URL, which on a download link is the
// signature that grants access to the file.
func (f *Fetcher) safe(err error) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		if u, parseErr := url.Parse(urlErr.URL); parseErr == nil && (u.RawQuery != "" || u.User != nil) {
			u.RawQuery, u.User = "", nil
			err = &url.Error{Op: urlErr.Op, URL: u.String(), Err: urlErr.Err}
		}
	}
	if redacted := f.redact(err.Error()); redacted != err.Error() {
		return errors.New(redacted)
	}
	return err
}

func (f *Fetcher) redact(text string) string {
	if token := f.Token.Reveal(); token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	return text
}

func (f *Fetcher) log() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
