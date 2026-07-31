package screens

import (
	"testing"
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
