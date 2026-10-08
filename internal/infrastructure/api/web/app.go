package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/api/web/render"
)

// Services are the use cases the handlers call.
type Services struct {
	Auth          service.Auth
	Chat          service.Chat
	Conversations service.Conversation
}

// Options are the settings the handlers need.
type Options struct {
	// SecureCookies adds the Secure attribute to the session cookie (https).
	SecureCookies bool
	// FlushInterval is the shortest time between streamed updates.
	FlushInterval time.Duration
	// RateLimitRPS and RateLimitBurst set the token bucket for sending messages
	// (per user) and signing in (per client address). Zero in either turns
	// limiting off. Only code that builds Options itself can pass one:
	// config.Load refuses zero for both settings, so a server started from the
	// settings always limits.
	RateLimitRPS   float64
	RateLimitBurst int
	// ReadinessChecks are what GET /readyz looks at, in the order they are
	// reported.
	ReadinessChecks []ReadinessCheck
}

// App owns the shared dependencies of the handlers and builds the route table.
// It is the only place that knows about both HTTP and the use cases; wiring the
// use cases to their adapters happens outside, in internal/app.
type App struct {
	log      *slog.Logger
	renderer *Renderer
	static   *Static
	svc      Services
	opts     Options
	md       *render.Markdown

	sends  *rateLimiter // nil when limiting is off
	logins *rateLimiter
	ready  readyCache

	// streams is done once the server starts shutting down; see EndStreams.
	streams    context.Context
	endStreams context.CancelFunc
}

// NewApp parses templates and loads static files from files, which must hold
// templates/ and static/ (see package assets).
func NewApp(log *slog.Logger, files fs.FS, svc Services, opts Options) (*App, error) {
	renderer, err := NewRenderer(files)
	if err != nil {
		return nil, err
	}
	static, err := NewStatic(files)
	if err != nil {
		return nil, err
	}
	streams, endStreams := context.WithCancel(context.Background())
	return &App{
		log: log, renderer: renderer, static: static, svc: svc, opts: opts, md: render.NewMarkdown(),
		sends:   newRateLimiter(opts.RateLimitRPS, opts.RateLimitBurst),
		logins:  newRateLimiter(opts.RateLimitRPS, opts.RateLimitBurst),
		streams: streams, endStreams: endStreams,
	}, nil
}

// EndStreams ends every running reply: each is saved as stopped with the text
// so far, and its browser is told. Streams that start afterwards are sent away
// to try again, so a reply is never stopped before it began. Call it when the
// server starts shutting down.
func (a *App) EndStreams() { a.endStreams() }

// serverError logs err with the request context and answers 500. The client
// never sees the error text.
func (a *App) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.ErrorContext(r.Context(), "server error",
		"request_id", RequestIDFrom(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// fail answers a request whose use case returned err. Something that is not
// there, or belongs to someone else, is a 404; anything else is a bug or an
// outage, and is logged and answered with a 500.
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	a.serverError(w, r, err)
}

// fromHTMX reports whether htmx sent the request, and so wants a piece of page to
// swap in. Anything else wants a whole page, or a redirect to one.
func fromHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}

// parseID reads the {id} of the path.
func parseID(r *http.Request) (uuid.UUID, bool) { return parseUUID(r.PathValue("id")) }

// messageRef names a message of the signed-in user, as the path of its routes does.
type messageRef struct{ userID, convID, msgID uuid.UUID }

// messageRefOf reads it from the request, answering 404 itself when the path
// holds something other than ids.
func messageRefOf(w http.ResponseWriter, r *http.Request) (messageRef, bool) {
	convID, ok1 := parseUUID(r.PathValue("id"))
	msgID, ok2 := parseUUID(r.PathValue("mid"))
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return messageRef{}, false
	}
	user, _ := UserFrom(r.Context())
	return messageRef{userID: user.ID, convID: convID, msgID: msgID}, true
}

// answerWithReply answers a request that put a new reply in the place of one:
// htmx gets the reply to swap in, a plain form post goes back to the conversation.
func (a *App) answerWithReply(w http.ResponseWriter, r *http.Request, convID uuid.UUID, reply domain.Message) {
	if !fromHTMX(r) {
		redirect(w, r, "/c/"+convID.String())
		return
	}
	a.block(w, r, http.StatusOK, "partial:reply", a.messageView(reply))
}

// basePage is embedded by every page's data: it carries what the layout needs.
type basePage struct {
	User *domain.User
}

func (a *App) base(r *http.Request) basePage {
	if u, ok := UserFrom(r.Context()); ok {
		return basePage{User: &u}
	}
	return basePage{}
}
