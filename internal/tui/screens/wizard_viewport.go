package screens

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// wizardMenuViewport owns cursor movement for wizard menus. The root TUI has
// a similar primitive for full-screen lists, but the wizard is also embedded
// directly by tests and other callers, so its menu window belongs here.
type wizardMenuViewport struct {
	Cursor int
	Offset int
	Rows   int
}

func (v *wizardMenuViewport) normalize(total int) {
	if total <= 0 {
		v.Cursor, v.Offset = 0, 0
		return
	}
	if v.Cursor < 0 {
		v.Cursor = 0
	}
	if v.Cursor >= total {
		v.Cursor = total - 1
	}
	if v.Rows < 1 {
		v.Rows = 1
	}
	maxOffset := total - v.Rows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if v.Offset < 0 {
		v.Offset = 0
	}
	if v.Offset > maxOffset {
		v.Offset = maxOffset
	}
}

func (v *wizardMenuViewport) ensureVisible(total int) {
	v.normalize(total)
	if total <= 0 {
		return
	}
	if v.Cursor < v.Offset {
		v.Offset = v.Cursor
	}
	if v.Cursor >= v.Offset+v.Rows {
		v.Offset = v.Cursor - v.Rows + 1
	}
	v.normalize(total)
}

func (v *wizardMenuViewport) move(delta, total int) {
	v.Cursor += delta
	v.ensureVisible(total)
}

func (v *wizardMenuViewport) page(delta, total int) {
	step := v.Rows - 1
	if step < 1 {
		step = 1
	}
	v.move(delta*step, total)
}

func (v *wizardMenuViewport) first(total int) {
	v.Cursor = 0
	v.Offset = 0
	v.normalize(total)
}

func (v *wizardMenuViewport) last(total int) {
	v.Cursor = total - 1
	v.ensureVisible(total)
}

func (v wizardMenuViewport) rangeFor(total int) (int, int) {
	v.ensureVisible(total)
	start := v.Offset
	end := start + v.Rows
	if end > total {
		end = total
	}
	if start > end {
		start = end
	}
	return start, end
}

// Move, Page, First, Last are the public API for wizard menu navigation.
// They wrap the lowercase helpers so wizard_discovery.go can control the viewport.

// Move scrolls by a delta in a wizard menu.
func (v *wizardMenuViewport) Move(delta, total int) {
	v.move(delta, total)
}

// Page scrolls one page in a wizard menu.
func (v *wizardMenuViewport) Page(delta, total int) {
	v.page(delta, total)
}

// First jumps to the first item.
func (v *wizardMenuViewport) First(total int) {
	v.first(total)
}

// Last jumps to the last item.
func (v *wizardMenuViewport) Last(total int) {
	v.last(total)
}

func maxWizardDetailWidth(width int) int {
	const maxDetailWidth = 80
	if width-4 > maxDetailWidth {
		return maxDetailWidth
	}
	return width - 4
}

func wizardMenuRows(height int) int {
	// The outer shell reserves the title, progress, status, and action footer.
	// Keep a small usable window even before the first WindowSizeMsg arrives.
	rows := height - 14
	if rows < 2 {
		rows = 2
	}
	return rows
}

// wizardFrameIndent is the fixed indentation the wizard's own rows reserve at
// their deepest: an expanded choice's detail hangs four cells in. The
// container's inner width is the terminal minus that reservation, and every
// prose renderer in the wizard consumes this one answer — a row that wrapped
// to the full terminal width and was then drawn under an indent is the defect
// that amputated explanation text at narrow sizes.
const wizardFrameIndent = 4

// contentWidth returns the inner width wizard content may occupy. It is the
// one width calculation for the wizard: menus, titles, field prompts and
// detail prose all wrap or truncate against it, so no produced line exceeds
// it in terminal cells.
func (m *WizardModel) contentWidth() int {
	if m.width < 1 {
		return 80
	}
	return InnerWidth(m.width, wizardFrameIndent)
}

func (m *WizardModel) MenuStep() bool {
	if m == nil {
		return false
	}
	if m.state.Step == WizardStepHostname && m.hostnameManual {
		return false
	}
	switch m.state.Step {
	case WizardStepOutcome, WizardStepIntent, WizardStepMCPMode,
		WizardStepDiscovery, WizardStepProtocol, WizardStepPortForwardProtocol,
		WizardStepHealth, WizardStepAdvancedSettings, WizardStepDirectoryMode,
		WizardStepDirectorySPA, WizardStepMCPTransport, WizardStepExposure,
		WizardStepHostname, WizardStepProtection, WizardStepProvider,
		WizardStepAccount, WizardStepPrivateNetworkMode:
		return true
	default:
		return false
	}
}

func (m *WizardModel) syncMenuViewport() {
	if m == nil || !m.MenuStep() {
		return
	}
	choices := m.menuChoiceCount()
	if m.menuViewport.Rows < 1 {
		m.menuViewport.Rows = wizardMenuRows(m.height)
	}
	// selected remains the compatibility field used by the existing step
	// handlers; the viewport is the authority for movement and visibility.
	m.menuViewport.Cursor = m.selected
	m.menuViewport.ensureVisible(choices)
	m.selected = m.menuViewport.Cursor
}

func (m *WizardModel) menuChoiceCount() int {
	if m == nil {
		return 0
	}
	switch m.state.Step {
	case WizardStepOutcome:
		return m.outcomeMenuLength()
	case WizardStepIntent:
		return 4
	case WizardStepMCPMode:
		return 2
	case WizardStepDiscovery:
		return len(m.discoveryChoiceList())
	case WizardStepProtocol:
		return 2
	case WizardStepPortForwardProtocol:
		return len(portForwardProtocolChoices())
	case WizardStepHealth:
		return 2
	case WizardStepAdvancedSettings:
		return 2
	case WizardStepDirectoryMode:
		return len(m.directoryModeChoices())
	case WizardStepDirectorySPA:
		return 2
	case WizardStepMCPTransport:
		return len(m.mcpTransports())
	case WizardStepExposure:
		return len(m.exposureChoices())
	case WizardStepHostname:
		return len(m.hostnameChoiceRows())
	case WizardStepProtection:
		return len(m.protectionChoices())
	case WizardStepProvider:
		return len(m.providerChoices())
	case WizardStepAccount:
		return len(m.accountsFor(m.state.Provider))
	case WizardStepPrivateNetworkMode:
		return len(privateNetworkModeChoices())
	default:
		return 0
	}
}

// menuWindow returns the visible [start, end) window for a menu of total items
// with the given selection. Direct wizard callers have no terminal geometry;
// the full semantic view is preserved there, while the compiled TUI always
// supplies a real height before a user can interact.
func (m *WizardModel) menuWindow(total, selected int) (int, int) {
	v := m.menuViewport
	v.Cursor = selected
	if m.height <= 0 {
		v.Rows = total
	} else if v.Rows < 1 {
		v.Rows = wizardMenuRows(m.height)
	}
	return v.rangeFor(total)
}

// wizardOneLine collapses a label onto exactly one physical row of at most
// width cells. Collapsing internal whitespace keeps a line a line even when a
// detail string was assembled with newlines; truncation is the honest answer
// for a label that cannot fit, and the full text stays reachable in Detail.
func wizardOneLine(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if width < 1 {
		return ""
	}
	return ansi.Truncate(text, width, "…")
}

// renderChoiceWindow is renderMenuWindow for choices carrying availability,
// sections and expandable detail.
func (m *WizardModel) renderChoiceWindow(title string, choices []wizardChoice, selected int) string {
	width := m.contentWidth()
	start, end := m.menuWindow(len(choices), selected)
	lines := append(WrapText(title, width), "")
	lastSection := ""
	for i := start; i < end; i++ {
		choice := choices[i]
		if choice.Section != "" && choice.Section != lastSection {
			lines = append(lines, wizardOneLine(choice.Section, width))
			lastSection = choice.Section
		}
		prefix := "  "
		if i == selected {
			prefix = "▸ "
			if m.useASCII {
				prefix = "> "
			}
		}
		label := prefix + choice.Label
		if !choice.Available && choice.Reason != "" {
			mark := " — " + choice.Reason
			if m.useASCII {
				mark = " - " + choice.Reason
			}
			label += mark
		}
		lines = append(lines, wizardOneLine(label, width))
	}
	if marker := wizardMenuIndicator(start, end, len(choices)); marker != "" {
		lines = append(lines, wizardOneLine(marker, width))
	}

	if choice, ok := choiceAt(choices, selected); ok {
		detail := make([]string, 0, len(choice.Detail)+1)
		for _, line := range choice.Detail {
			detail = append(detail, WrapText(line, maxWizardDetailWidth(width))...)
		}
		if len(choice.Providers) > 0 {
			detail = append(detail, WrapText("Provided by: "+strings.Join(choice.Providers, ", "), maxWizardDetailWidth(width))...)
		}
		if len(detail) > 0 {
			lines = append(lines, "")
			budget := 4
			if m.height > 0 {
				budget = m.height - len(lines) - 5
				if budget < 2 {
					budget = 2
				}
			}
			for i, line := range detail {
				if i >= budget {
					lines = append(lines, "    … more detail in Help")
					break
				}
				lines = append(lines, wizardOneLine("    "+line, width))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func wizardMenuIndicator(start, end, total int) string {
	if start == 0 && end >= total {
		return ""
	}
	parts := make([]string, 0, 2)
	if start > 0 {
		parts = append(parts, "↑ "+strconv.Itoa(start)+" above")
	}
	if end < total {
		parts = append(parts, "↓ "+strconv.Itoa(total-end)+" below")
	}
	return strings.Join(parts, " · ")
}

// SetSize applies terminal dimensions to the wizard and its active field.
func (m *WizardModel) SetSize(width, height int) *WizardModel {
	if m == nil {
		return m
	}
	m.width, m.height = width, height
	fieldWidth := width - 6
	if fieldWidth < 20 {
		fieldWidth = 20
	}
	if fieldWidth > 120 {
		fieldWidth = 120
	}
	m.field.SetWidth(fieldWidth)
	m.discoveryFilterField.SetWidth(fieldWidth)
	m.menuViewport.Rows = wizardMenuRows(height)
	m.syncMenuViewport()
	return m
}
