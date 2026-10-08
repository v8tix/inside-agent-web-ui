package resource

import (
	"context"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// UIDriver uses the Web UI the way a person does: it speaks in intentions, not
// clicks. One driver is one browser session and is not safe for concurrent use.
//
//go:generate mockery --name UIDriver --filename ui_driver.go
type UIDriver interface {
	// Login signs in. A page speaking another UI contract version is
	// domain.ErrContractVersion; refused credentials are domain.ErrLoginFailed.
	Login(ctx context.Context, email, password string) error

	// StartChat opens a new, empty conversation, so the agent starts with no
	// memory of earlier tasks. Observation of the attempt starts here, as the
	// page opens.
	StartChat(ctx context.Context) error

	// Submit puts text in the composer and presses Enter. The times of the
	// attempt are counted from that key press.
	Submit(ctx context.Context, text string) error

	// AwaitReply waits until the assistant message has a final status. When ctx
	// ends first it returns what it knows and an error wrapping
	// domain.ErrReplyTimeout.
	AwaitReply(ctx context.Context) (domain.Reply, error)

	// Stop presses the Stop button.
	Stop(ctx context.Context) error

	// ReadRaw returns the reply as stored (Markdown), for answer extraction.
	ReadRaw(ctx context.Context, reply domain.Reply) (string, error)

	// Collect returns what the browser observed since StartChat opened the page.
	Collect(ctx context.Context) (domain.UIObservations, error)

	// Screenshot saves the page as a PNG at path.
	Screenshot(ctx context.Context, path string) error

	// DeleteChat deletes the current conversation through the UI.
	DeleteChat(ctx context.Context) error

	// Close ends the browser session.
	Close(ctx context.Context) error
}

// DriverFactory makes drivers, one for each worker. A driver that died is
// replaced by asking for another.
//
//go:generate mockery --name DriverFactory --filename driver_factory.go
type DriverFactory interface {
	NewDriver(ctx context.Context) (UIDriver, error)
}
