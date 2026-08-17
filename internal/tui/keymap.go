package tui

// KeyBinding describes a single key binding.
type KeyBinding struct {
	Key     string
	Label   string
	Enabled bool
}

// KeyMap defines the keyboard shortcuts for the TUI.
type KeyMap struct {
	Quit      KeyBinding
	Up        KeyBinding
	Down      KeyBinding
	Enter     KeyBinding
	Space     KeyBinding
	Help      KeyBinding
	New       KeyBinding
	Providers KeyBinding
	Repair    KeyBinding
	Discover  KeyBinding
	Delete    KeyBinding
	Back      KeyBinding
}

// Bindings returns only enabled bindings as a flat slice.
func (km KeyMap) Bindings() []KeyBinding {
	var b []KeyBinding
	add := func(kb KeyBinding) {
		if kb.Enabled {
			b = append(b, kb)
		}
	}
	add(km.New)
	add(km.Enter)
	add(km.Space)
	add(km.Repair)
	add(km.Discover)
	add(km.Delete)
	add(km.Providers)
	add(km.Help)
	add(km.Back)
	add(km.Quit)
	return b
}

// DefaultKeyMap returns the default key bindings with all enabled.
var DefaultKeyMap = KeyMap{
	Quit:      KeyBinding{Key: "ctrl+c", Label: "Quit", Enabled: true},
	Up:        KeyBinding{Key: "up", Label: "Up", Enabled: true},
	Down:      KeyBinding{Key: "down", Label: "Down", Enabled: true},
	Enter:     KeyBinding{Key: "enter", Label: "Inspect", Enabled: true},
	Space:     KeyBinding{Key: "space", Label: "Open/Close", Enabled: true},
	Help:      KeyBinding{Key: "?", Label: "Help", Enabled: true},
	New:       KeyBinding{Key: "n", Label: "New", Enabled: true},
	Providers: KeyBinding{Key: "p", Label: "Providers", Enabled: true},
	Repair:    KeyBinding{Key: "r", Label: "Repair", Enabled: true},
	Discover:  KeyBinding{Key: "a", Label: "Discover", Enabled: true},
	Delete:    KeyBinding{Key: "d", Label: "Delete", Enabled: true},
	Back:      KeyBinding{Key: "esc", Label: "Back", Enabled: true},
}

// helpRow renders a help text row with context-sensitive bindings.
func helpRow(km KeyMap, th Theme, isHome bool) string {
	quitLabel := "q Home"
	if isHome {
		quitLabel = "q Quit"
	}
	help := "  ↑↓ Navigate  Enter Inspect  Space Open/Close  n New  e Edit  c Copy  " +
		"a Discover  r Repair  d Delete  " + quitLabel + "  ? Help"
	return th.Style("help").Render(help)
}
