package port_test

import (
	"testing"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	resmocks "github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource/mocks"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	svcmocks "github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service/mocks"
)

// The mocks are generated. These assertions fail the build when an interface
// changes and the mocks were not regenerated (go generate ./internal/eval/...).
var (
	_ resource.ProblemSource = (*resmocks.ProblemSource)(nil)
	_ resource.UIDriver      = (*resmocks.UIDriver)(nil)
	_ resource.DriverFactory = (*resmocks.DriverFactory)(nil)
	_ resource.UIProbes      = (*resmocks.UIProbes)(nil)
	_ resource.ResultStore   = (*resmocks.ResultStore)(nil)

	_ service.Runner   = (*svcmocks.Runner)(nil)
	_ service.Reporter = (*svcmocks.Reporter)(nil)
	_ service.Prober   = (*svcmocks.Prober)(nil)
)

func TestMocksMatchThePorts(t *testing.T) {
	// Nothing to run: the assertions above are checked when the package compiles.
}
