package devtools

import (
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewSessionForTest wraps a client session that talks to a server in this process.
func NewSessionForTest(cs *mcp.ClientSession, opts Options) *Session { return newSession(cs, opts) }

// Isolate and KillTree expose the process helpers.
func Isolate(cmd *exec.Cmd)  { isolate(cmd) }
func KillTree(cmd *exec.Cmd) { killTree(cmd) }

// TailBuffer exposes the buffer that keeps the end of the server's output.
type TailBuffer = tailBuffer

// NewTailBuffer makes one that keeps max bytes.
func NewTailBuffer(max int) *TailBuffer { return &tailBuffer{max: max} }

// OutputOf exposes what an error says of the output the server left behind.
func OutputOf(t *TailBuffer) string { return outputOf(t) }

// ScriptForTest and ScriptMarkerForTest expose the page scripts.
func ScriptForTest(name string) string           { return script(name) }
func ScriptMarkerForTest(function string) string { return scriptMarker(function) }

// Session exposes the driver's connection to the MCP server, to call a tool the
// driver has no method for.
func (d *Driver) Session() *Session { return d.s }

// SetReportForTest sets the hook that is told the versions of the tools.
func (d *Driver) SetReportForTest(f func(name, version string)) { d.report = f }

// NewProbesForTest returns probes that work in the driver's tab, which is
// already signed in, instead of opening a browser of their own.
func NewProbesForTest(d *Driver) *Probes { return &Probes{d: d} }
