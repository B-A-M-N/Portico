package supervisor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/app"
	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider"
	"github.com/B-A-M-N/portico/internal/store"
)

func TestRotateSecretKeyIsAnOperationalDurableAction(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "portico.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := &supervisorHandler{sup: &Supervisor{
		store:    st,
		paths:    app.Paths{DatabasePath: filepath.Join(dir, "portico.db")},
		registry: provider.NewRegistry(),
	}}
	result, err := h.HandleRotateSecretKey()
	if err != nil {
		t.Fatalf("HandleRotateSecretKey: %v", err)
	}
	if result.Version < 2 {
		t.Fatalf("rotated version = %d, want a new key version", result.Version)
	}
	if result.RotatedAt == "" {
		t.Fatal("rotation result has no timestamp")
	}

	events, err := st.GetDurableEventsSince(context.Background(), 0, 20)
	if err != nil {
		t.Fatalf("GetDurableEventsSince: %v", err)
	}
	found := map[core.EventType]bool{}
	for _, event := range events {
		switch event.Event.Type {
		case core.EventSecretKeyRotationStarted, core.EventSecretKeyRotationCompleted, core.EventSecretKeyRotationFailed:
			found[event.Event.Type] = true
		default:
			continue
		}
		if event.ConnectionID != "" || event.OperationID != "" {
			t.Fatalf("rotation event has connection/operation identity: %+v", event)
		}
		data, _ := json.Marshal(event.Event.Data)
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "key_material") {
			t.Fatalf("rotation event contains sensitive material: %s", data)
		}
	}
	for _, eventType := range []core.EventType{core.EventSecretKeyRotationStarted, core.EventSecretKeyRotationCompleted} {
		if !found[eventType] {
			t.Fatalf("key rotation did not leave durable %q event", eventType)
		}
	}
}
