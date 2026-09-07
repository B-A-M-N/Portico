package tui

import (
	"os/exec"
	"runtime"

	tea "charm.land/bubbletea/v2"
)

// Provider-declared setup help.
//
// The URL is a provider fact — declared on the SetupFlow, never hardcoded
// here. Opening it is best-effort: a machine with a desktop opens the browser,
// and one without (SSH, container, serial console) is told the address so the
// user can open it on whatever device they have. Neither path records the URL
// anywhere durable and neither claims the page was reached.

// browserLauncher opens a URL in the user's browser. A package variable so
// tests can inject a fake instead of actually opening anything.
var browserLauncher = launchBrowser

// launchBrowser is the production launcher. It returns false when no browser
// could be launched, which the caller must surface as a printed URL instead.
func launchBrowser(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// The child's stdout/stderr are dropped: the browser printing to Portico's
	// terminal would corrupt the TUI display.
	return cmd.Start() == nil
}

// openSetupHelpCmd opens the provider's declared help URL. The message only
// records what happened; the TUI displays the URL itself when launch failed.
func openSetupHelpCmd(url string) tea.Cmd {
	return func() tea.Msg {
		opened := url != "" && browserLauncher(url)
		return setupHelpLaunchedMsg{URL: url, Opened: opened}
	}
}

type setupHelpLaunchedMsg struct {
	URL    string
	Opened bool
}
