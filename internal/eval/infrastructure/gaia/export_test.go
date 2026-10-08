package gaia

import (
	"context"
	"net/http"
)

// GetForTest lets the black-box tests make a request the way the fetcher does.
func (f *Fetcher) GetForTest(ctx context.Context, rawURL string) (*http.Response, error) {
	return f.get(ctx, rawURL)
}

// ReadBatchForTest is how many rows the reader asks of a row group at a time, so
// that a test can write more than one batch.
const ReadBatchForTest = readBatch
