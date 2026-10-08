// Package detachedsave saves a result after the request that asked for it is gone.
package detachedsave

import (
	"context"
	"time"
)

// SaveTimeout is the limit of a save that outlives its request.
const SaveTimeout = 5 * time.Second

// Save calls save on a context that keeps the values of ctx and ignores its
// cancellation, but ends after SaveTimeout, because a detached context has no deadline.
func Save(ctx context.Context, save func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SaveTimeout)
	defer cancel()
	return save(ctx)
}
