package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme defines the semantic color palette.
// Colors are theme tokens — never embed literals in components.
type Theme struct {
	Background   color.Color
	Text         color.Color
	Muted        color.Color
	Structure    color.Color
	Stable       color.Color
	Attention    color.Color
	Intervention color.Color
	Monochrome   bool
}

// DefaultTheme returns the default Portico theme.
var DefaultTheme = Theme{
	Background:   lipgloss.Color("#1a1a1a"),
	Text:         lipgloss.Color("#f5e6c8"),
	Muted:        lipgloss.Color("#8a7f70"),
	Structure:    lipgloss.Color("#b87333"),
	Stable:       lipgloss.Color("#4a9e9e"),
	Attention:    lipgloss.Color("#ffb347"),
	Intervention: lipgloss.Color("#c04040"),
}

// MonochromeTheme returns a monochrome-compatible theme.
var MonochromeTheme = Theme{
	Background:   lipgloss.NoColor{},
	Text:         lipgloss.NoColor{},
	Muted:        lipgloss.NoColor{},
	Structure:    lipgloss.NoColor{},
	Stable:       lipgloss.NoColor{},
	Attention:    lipgloss.NoColor{},
	Intervention: lipgloss.NoColor{},
	Monochrome:   true,
}

// Style returns a lipgloss style derived from the theme for the given use.
func (th Theme) Style(use string) lipgloss.Style {
	s := lipgloss.NewStyle()
	if th.Monochrome {
		// No color in monochrome mode
		return s
	}
	switch use {
	case "header":
		return s.
			Background(lipgloss.Color("#b87333")).
			Foreground(lipgloss.Color("#1a1a1a")).
			Bold(true).
			Padding(0, 1)
	case "normal":
		return s.Foreground(lipgloss.Color("#f5e6c8"))
	case "title":
		// A screen's own heading, distinct from the inverse-video header bar.
		return s.Foreground(lipgloss.Color("#f5e6c8")).Bold(true)
	case "muted":
		return s.Foreground(lipgloss.Color("#8a7f70"))
	case "stable":
		return s.Foreground(lipgloss.Color("#4a9e9e"))
	case "attention":
		return s.Foreground(lipgloss.Color("#ffb347"))
	case "intervention":
		return s.Foreground(lipgloss.Color("#c04040"))
	case "help":
		return s.Foreground(lipgloss.Color("#8a7f70"))
	case "selected":
		return s.Foreground(lipgloss.Color("#f5e6c8")).Bold(true)
	default:
		return s
	}
}

// LipGloss styles used throughout the TUI (derived from theme by default).
var (
	HeaderStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#b87333")).
			Foreground(lipgloss.Color("#1a1a1a")).
			Bold(true).
			Padding(0, 1)

	NormalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f5e6c8"))

	MutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8a7f70"))

	StableStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#4a9e9e"))

	AttentionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ffb347"))

	InterventionStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#c04040"))

	HelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8a7f70"))

	SelectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#f5e6c8")).
			Bold(true)
)
