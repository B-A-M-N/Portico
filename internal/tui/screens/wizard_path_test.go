package screens

import (
	"testing"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// walkForward drives the wizard through real key handling and records every
// question it actually visits.
//
// This is the difference from walking applicableSteps: that asks the table what
// it believes, and the table was derived by reading the forward handlers. If
// the reading was wrong, the table and a test built on it are wrong together.
// Driving the handlers records what the wizard does.
func walkForward(t *testing.T, m *WizardModel, answer func(step int) []string) []int {
	t.Helper()
	visited := []int{m.Step()}
	for i := 0; i < 40; i++ {
		step := m.Step()
		if step == WizardStepReview || step == WizardStepCreating {
			break
		}
		for _, key := range answer(step) {
			m.HandleKey(key)
		}
		if m.Step() == step {
			t.Fatalf("the wizard did not advance from step %d", step)
		}
		visited = append(visited, m.Step())
	}
	return visited
}

// walkBack drives escape from wherever the wizard is and records the return.
func walkBack(t *testing.T, m *WizardModel) []int {
	t.Helper()
	visited := []int{m.Step()}
	for i := 0; i < 40; i++ {
		step := m.Step()
		m.HandleKey("esc")
		if m.Step() == step {
			break
		}
		visited = append(visited, m.Step())
	}
	return visited
}

// TestTheStepsVisitedGoingBackAreTheStepsVisitedGoingForward drives the real
// handlers in both directions and compares the paths.
//
// The previous test built applicableSteps and walked that same list backward,
// so it proved the table was self-consistent, not that it matched the wizard.
func TestTheStepsVisitedGoingBackAreTheStepsVisitedGoingForward(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers func(step int) []string
	}{
		{
			name: "service at an address",
			answers: func(step int) []string {
				switch step {
				case WizardStepOutcome:
					return []string{"enter"} // first prepared outcome
				case WizardStepName:
					return []string{"d", "e", "m", "o", "enter"}
				case WizardStepSource:
					return []string{"1", "2", "7", ".", "0", ".", "0", ".", "1", "enter"}
				case WizardStepPort:
					return []string{"8", "0", "8", "0", "enter"}
				default:
					return []string{"enter"}
				}
			},
		},
		{
			name: "directory served read-only",
			answers: func(step int) []string {
				switch step {
				case WizardStepOutcome:
					return []string{"down", "down", "enter"} // the directory outcome
				case WizardStepName:
					return []string{"f", "i", "l", "e", "s", "enter"}
				case WizardStepSource:
					return []string{"/", "s", "r", "v", "enter"}
				default:
					return []string{"enter"}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
			forward := walkForward(t, m, tc.answers)
			if len(forward) < 4 {
				t.Fatalf("path too short to be meaningful: %v", forward)
			}

			back := walkBack(t, m)

			// Reversing the outbound path must give the return path.
			reversed := make([]int, 0, len(forward))
			for i := len(forward) - 1; i >= 0; i-- {
				reversed = append(reversed, forward[i])
			}
			if len(back) != len(reversed) {
				t.Fatalf("forward visited %v, back visited %v", forward, back)
			}
			for i := range back {
				if back[i] != reversed[i] {
					t.Fatalf("step %d of the return was %d, want %d\nforward %v\nback %v",
						i, back[i], reversed[i], forward, back)
				}
			}
		})
	}
}

// TestEveryConditionalStepIsBothVisitedAndSkipped is the coverage the path test
// needs to be meaningful.
//
// A predicate that is never exercised in both directions is a predicate no test
// can contradict. Every conditional question must appear on at least one real
// path and be absent from at least one, driven through the actual handlers.
func TestEveryConditionalStepIsBothVisitedAndSkipped(t *testing.T) {
	paths := []struct {
		name    string
		setup   func(*WizardModel)
		answers func(step int) []string
	}{
		{
			name: "permanent address, protected, two accounts",
			setup: func(m *WizardModel) {
				snapshot := fullCloudflareSnapshot()
				snapshot[0].Accounts = []ipc.ProviderAccountDTO{
					{ID: "acct-a", Status: "authenticated"},
					{ID: "acct-b", Status: "authenticated"},
				}
				m.caps = providerCapabilities{providers: snapshot}
			},
			answers: func(step int) []string {
				switch step {
				case WizardStepOutcome:
					return []string{"enter"}
				case WizardStepName:
					return []string{"a", "enter"}
				case WizardStepSource:
					return []string{"1", "2", "7", ".", "0", ".", "0", ".", "1", "enter"}
				case WizardStepPort:
					return []string{"9", "0", "enter"}
				case WizardStepExposure:
					return []string{"down", "enter"} // a permanent address
				case WizardStepHostname:
					return []string{"a", ".", "e", "x", "a", "m", "p", "l", "e", ".", "c", "o", "m", "enter"}
				case WizardStepProtection:
					return []string{"down", "enter"} // email passcode
				case WizardStepProtectionRules:
					return []string{"p", "@", "e", ".", "c", "o", "m", "enter"}
				default:
					return []string{"enter"}
				}
			},
		},
		{
			name: "directory, read-only",
			answers: func(step int) []string {
				switch step {
				case WizardStepOutcome:
					return []string{"down", "down", "enter"} // the directory outcome
				case WizardStepName:
					return []string{"d", "enter"}
				case WizardStepSource:
					return []string{"/", "s", "r", "v", "enter"}
				default:
					return []string{"enter"}
				}
			},
		},
		{
			name: "command source",
			answers: func(step int) []string {
				switch step {
				case WizardStepOutcome:
					return []string{"down", "down", "down", "enter"} // the command outcome
				case WizardStepName:
					return []string{"c", "enter"}
				case WizardStepSource:
					return []string{"s", "r", "v", "enter"}
				case WizardStepPort:
					return []string{"7", "0", "enter"}
				case WizardStepCommandArgs:
					return []string{"-", "v", "enter"}
				default:
					return []string{"enter"}
				}
			},
		},
	}

	visitedAnywhere := map[int]bool{}
	skippedAnywhere := map[int]bool{}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWizard(&fakeWizardClient{}, fullCloudflareSnapshot())
			if tc.setup != nil {
				tc.setup(m)
			}
			forward := walkForward(t, m, tc.answers)

			onPath := map[int]bool{}
			for _, step := range forward {
				onPath[step] = true
				visitedAnywhere[step] = true
			}
			for _, rule := range wizardStepOrder {
				if !onPath[rule.id] {
					skippedAnywhere[rule.id] = true
				}
			}

			// The return path must retrace this one exactly.
			back := walkBack(t, m)
			if len(back) != len(forward) {
				t.Fatalf("forward %v, back %v", forward, back)
			}
			for i := range back {
				if back[i] != forward[len(forward)-1-i] {
					t.Fatalf("return diverged at %d\nforward %v\nback %v", i, forward, back)
				}
			}
		})
	}

	// Conditional questions only. The unconditional ones are on every path by
	// definition and cannot be skipped.
	conditional := []struct {
		id   int
		name string
	}{
		{WizardStepPort, "port"},
		{WizardStepProtocol, "protocol"},
		{WizardStepCommandArgs, "command arguments"},
		{WizardStepCommandWorkingDir, "working directory"},
		{WizardStepHostname, "hostname"},
		{WizardStepProtectionRules, "protection rules"},
		{WizardStepAccount, "account"},
	}
	for _, step := range conditional {
		if !visitedAnywhere[step.id] {
			t.Errorf("the %s question is never asked on any tested path", step.name)
		}
		if !skippedAnywhere[step.id] {
			t.Errorf("the %s question is never skipped on any tested path", step.name)
		}
	}
}
