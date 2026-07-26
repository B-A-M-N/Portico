package tunnel

import (
	"path/filepath"
	"testing"
)

func TestPorticoConnectorLogDirUsesXDGState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got, want := porticoConnectorLogDir(), filepath.Join(state, "portico", "logs"); got != want {
		t.Fatalf("porticoConnectorLogDir() = %q, want %q", got, want)
	}
}
