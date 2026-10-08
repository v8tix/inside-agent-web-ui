package testagent

import (
	"net/http"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/server/adkrest"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// AppName is the ADK app name clients put in URLs. It matches the default of
// the Web UI's AGENT_APP_NAME.
const AppName = "ui_agent"

// LookupArgs is the input of the lookup tool.
type LookupArgs struct {
	Query string `json:"query" jsonschema:"what to look up"`
}

// LookupResult is what the lookup tool returns.
type LookupResult struct {
	Result string `json:"result"`
}

func lookup(_ agent.Context, _ LookupArgs) (LookupResult, error) {
	return LookupResult{Result: "forty-two"}, nil
}

// BuildAgent wraps llm in an ADK agent named AppName with one tool, "lookup".
func BuildAgent(llm model.LLM) (agent.Agent, error) {
	fnTool, err := functiontool.New(functiontool.Config{
		Name:        lookupTool,
		Description: "Looks something up and returns the result.",
	}, lookup)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:        AppName,
		Model:       llm,
		Description: "A scripted agent for tests.",
		Instruction: "You are a scripted test agent.",
		Tools:       []tool.Tool{fnTool},
	})
}

// DefaultSSEWriteTimeout is how long the ADK server lets one event stream run,
// as its launcher does (the flag is --sse-write-timeout).
const DefaultSSEWriteTimeout = 2 * time.Minute

// HandlerOption changes how NewHandler serves.
type HandlerOption func(*handlerSettings)

type handlerSettings struct{ sseWriteTimeout time.Duration }

// WithSSEWriteTimeout sets how long one reply may take to stream before the
// server cuts it off, as an agent whose answer outlasts that limit is.
func WithSSEWriteTimeout(d time.Duration) HandlerOption {
	return func(s *handlerSettings) { s.sseWriteTimeout = d }
}

// NewHandler serves the real ADK REST API under /api for llm, with in-memory
// sessions, the same routes the ADK launcher exposes.
func NewHandler(llm model.LLM, opts ...HandlerOption) (http.Handler, error) {
	settings := handlerSettings{sseWriteTimeout: DefaultSSEWriteTimeout}
	for _, opt := range opts {
		opt(&settings)
	}
	root, err := BuildAgent(llm)
	if err != nil {
		return nil, err
	}
	srv, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  session.InMemoryService(),
		MemoryService:   memory.InMemoryService(),
		ArtifactService: artifact.InMemoryService(),
		AgentLoader:     agent.NewSingleLoader(root),
		SSEWriteTimeout: settings.sseWriteTimeout,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", srv))
	return mux, nil
}
