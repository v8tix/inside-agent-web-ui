package logging_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/logging"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.App
		logInfo   bool
		wantInOut []string
		wantJSON  bool
	}{
		{
			name:      "json at info level",
			cfg:       config.App{LogLevel: "info", LogFormat: "json"},
			logInfo:   true,
			wantInOut: []string{`"level":"INFO"`, `"msg":"hello"`, `"app":"agent-web-ui"`},
			wantJSON:  true,
		},
		{
			name:      "text at info level",
			cfg:       config.App{LogLevel: "info", LogFormat: "text"},
			logInfo:   true,
			wantInOut: []string{"level=INFO", "msg=hello", "app=agent-web-ui"},
		},
		{
			name:    "warn level drops info records",
			cfg:     config.App{LogLevel: "warn", LogFormat: "json"},
			logInfo: true,
		},
		{
			name:      "debug level keeps debug records",
			cfg:       config.App{LogLevel: "debug", LogFormat: "json"},
			wantInOut: []string{`"level":"DEBUG"`},
			wantJSON:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := logging.New(tc.cfg, &buf)

			if tc.logInfo {
				log.Info("hello")
			} else {
				log.Debug("hello")
			}

			out := buf.String()
			if len(tc.wantInOut) == 0 {
				assert.Empty(t, out)
				return
			}
			for _, want := range tc.wantInOut {
				assert.Contains(t, out, want)
			}
			if tc.wantJSON {
				var record map[string]any
				require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
			}
		})
	}
}
