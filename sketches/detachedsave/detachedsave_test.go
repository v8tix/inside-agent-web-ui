package detachedsave

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestTheSaveSurvivesTheRequest(t *testing.T) {
	request, leave := context.WithCancel(t.Context())
	leave() // the browser has gone
	err := Save(request, func(ctx context.Context) error { return ctx.Err() })
	if err != nil {
		t.Fatalf("the save saw the cancellation of the request: %v", err)
	}
}

func TestTheSaveHasALimitOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := Save(t.Context(), func(ctx context.Context) error {
			<-ctx.Done() // a database that never answers
			return ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != SaveTimeout {
			t.Fatalf("err = %v after %v, want a deadline after %v", err, time.Since(start), SaveTimeout)
		}
	})
}
