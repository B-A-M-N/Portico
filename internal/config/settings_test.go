package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func settingsFixture(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestUpdateOperationalSettingsDoesNotDeadlockAndPersistsAllFields(t *testing.T) {
	settingsFixture(t)

	type updateResult struct {
		settings OperationalSettings
		err      error
	}
	result := make(chan updateResult, 1)
	go func() {
		settings, err := UpdateOperationalSettings(SettingsPatch{
			LaunchMode:          stringPtr("manual"),
			DefaultAutoStart:    boolPtr(false),
			DefaultOnDisconnect: stringPtr("close"),
		})
		result <- updateResult{settings: settings, err: err}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("UpdateOperationalSettings: %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("settings update deadlocked")
	}

	got, err := LoadOperationalSettings()
	if err != nil {
		t.Fatalf("LoadOperationalSettings: %v", err)
	}
	want := OperationalSettings{LaunchMode: "manual", DefaultAutoStart: false, DefaultOnDisconnect: "close"}
	if got != want {
		t.Fatalf("settings = %#v, want %#v", got, want)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "portico", "config.toml"))
	if err != nil {
		t.Fatalf("read persisted settings: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("persisted settings file is empty")
	}
}

func TestSaveOperationalSettingsRestoresPreRequestSnapshotOnWriteFailure(t *testing.T) {
	settingsFixture(t)
	prior := OperationalSettings{LaunchMode: "auto", DefaultAutoStart: true, DefaultOnDisconnect: "keep_alive"}
	if err := SaveOperationalSettings(prior); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	configPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "portico", "config.toml")
	priorBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read prior settings: %v", err)
	}
	originalConfigHome := os.Getenv("XDG_CONFIG_HOME")
	badParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badParent, []byte("block"), 0600); err != nil {
		t.Fatalf("create config path blocker: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", badParent)

	if err := SaveOperationalSettings(OperationalSettings{
		LaunchMode: "manual", DefaultAutoStart: false, DefaultOnDisconnect: "close",
	}); err == nil {
		t.Fatal("write failure was accepted")
	}
	t.Setenv("XDG_CONFIG_HOME", originalConfigHome)
	got, err := LoadOperationalSettings()
	if err != nil {
		t.Fatalf("load restored settings: %v", err)
	}
	if got != prior {
		t.Fatalf("in-memory settings after failed write = %#v, want %#v", got, prior)
	}
	afterBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read settings after failed write: %v", err)
	}
	if string(afterBytes) != string(priorBytes) {
		t.Fatalf("disk settings changed after failed write:\nbefore=%q\nafter=%q", priorBytes, afterBytes)
	}
}

func TestLoadOperationalSettingsRejectsCorruptValues(t *testing.T) {
	settingsFixture(t)
	for key, value := range map[string]any{
		KeyLaunchMode:          "automatically",
		KeyDefaultAutoStart:    "sometimes",
		KeyDefaultOnDisconnect: "persist_forever",
	} {
		viper.Reset()
		viper.Set(key, value)
		if _, err := LoadOperationalSettings(); err == nil {
			t.Fatalf("corrupt value for %s was silently normalized", key)
		}
	}
}

func stringPtr(v string) *string { return &v }
func boolPtr(v bool) *bool       { return &v }
