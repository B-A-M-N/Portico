// Package docs_test validates that the capability claims in project
// documentation match what the adapters actually declare.
//
// Audit item 28: the README, SPEC.md and docs/REMAINING_WORK.md each carried
// independent capability claims, and they disagreed. The README described ngrok
// as implemented with IP restriction, basic auth and OAuth protection while the
// adapter declared every capability experimental and applied no protection at
// all. Hand-maintained claims in several files drift; this test makes the
// adapter descriptors the single source of truth and fails the build when a
// document contradicts them.
package docs_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/provider/cloudflare"
	"github.com/B-A-M-N/portico/internal/provider/ngrok"
)

// releaseStatus is the vocabulary documentation may use for a provider.
type releaseStatus string

const (
	statusImplemented    releaseStatus = "implemented"
	statusExperimental   releaseStatus = "experimental"
	statusNotImplemented releaseStatus = "not implemented"
)

// derivedStatus computes a provider's release status from its declared
// capabilities. A provider whose every supported capability is experimental is
// experimental, however the documentation describes it.
func derivedStatus(caps core.Capabilities) releaseStatus {
	supported := 0
	experimental := 0

	consider := func(c core.CapabilitySupport) {
		if !c.Supported {
			return
		}
		supported++
		if c.Stability == core.StabilityExperimental {
			experimental++
		}
	}
	consider(caps.TemporaryAddresses)
	consider(caps.CustomHostnames)
	consider(caps.PrivateExposure)
	consider(caps.ManagedDNS)

	for _, p := range caps.BuiltInProtection {
		if !p.Supported || p.Kind == core.ProtectionNone {
			continue
		}
		supported++
		if p.Stability == core.StabilityExperimental {
			experimental++
		}
	}

	if supported == 0 {
		return statusNotImplemented
	}
	if experimental == supported {
		return statusExperimental
	}
	return statusImplemented
}

// providerDescriptors returns the live capability descriptors. Providers with
// no adapter are absent, which is itself the claim being verified.
func providerDescriptors(t *testing.T) map[string]releaseStatus {
	t.Helper()
	ctx := context.Background()

	cfCaps, err := (&cloudflare.Provider{}).Capabilities(ctx)
	if err != nil {
		t.Fatalf("cloudflare capabilities: %v", err)
	}
	ngrokCaps, err := (&ngrok.Provider{}).Capabilities(ctx)
	if err != nil {
		t.Fatalf("ngrok capabilities: %v", err)
	}

	return map[string]releaseStatus{
		"cloudflare": derivedStatus(cfCaps),
		"ngrok":      derivedStatus(ngrokCaps),
		// No adapter package exists for these.
		"tailscale": statusNotImplemented,
		"zrok":      statusNotImplemented,
	}
}

// documentedStatus maps the README's status column onto the vocabulary.
func documentedStatus(cell string) (releaseStatus, bool) {
	lower := strings.ToLower(cell)
	switch {
	case strings.Contains(lower, "not implemented"):
		return statusNotImplemented, true
	case strings.Contains(lower, "experimental"):
		return statusExperimental, true
	case strings.Contains(lower, "implemented"):
		return statusImplemented, true
	}
	return "", false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// This package lives at internal/docs.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

var providerRow = regexp.MustCompile(`^\|\s*([A-Za-z][A-Za-z0-9 ]*?)\s*\|\s*([^|]+?)\s*\|`)

// TestReadmeProviderTableMatchesAdapterDescriptors fails when the README claims
// a capability level the adapter does not declare.
func TestReadmeProviderTableMatchesAdapterDescriptors(t *testing.T) {
	descriptors := providerDescriptors(t)

	readme, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	checked := map[string]bool{}
	for _, line := range strings.Split(string(readme), "\n") {
		m := providerRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(m[1]))
		want, known := descriptors[name]
		if !known {
			continue
		}
		got, ok := documentedStatus(m[2])
		if !ok {
			t.Errorf("README row for %q has an unrecognised status %q; use implemented, experimental or not implemented",
				name, strings.TrimSpace(m[2]))
			continue
		}
		if got != want {
			t.Errorf("README claims %q is %q but its adapter declares %q", name, got, want)
		}
		checked[name] = true
	}

	// A provider that vanishes from the table is as much a drift as a wrong
	// claim, so every known provider must appear.
	for name := range descriptors {
		if !checked[name] {
			t.Errorf("provider %q is missing from the README provider table", name)
		}
	}
}

// TestNgrokIsNotDescribedAsUsable guards the specific claim the audit found:
// the README advertised ngrok protection the adapter never applies.
func TestNgrokIsNotDescribedAsUsable(t *testing.T) {
	caps, err := (&ngrok.Provider{}).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("ngrok capabilities: %v", err)
	}
	for _, p := range caps.BuiltInProtection {
		if p.Kind == core.ProtectionNone {
			continue
		}
		if p.Supported {
			t.Fatalf("ngrok now declares protection %q supported; the README must be updated to match", p.Kind)
		}
	}

	readme, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(readme)
	// The adapter applies no protection, so the README must not advertise any.
	for _, claim := range []string{"IP restrictions", "basic auth", "OAuth"} {
		if strings.Contains(text, claim) {
			t.Errorf("README advertises ngrok %q, which the adapter does not apply", claim)
		}
	}
	if !strings.Contains(text, "PORTICO_ENABLE_EXPERIMENTAL_NGROK") {
		t.Error("README does not state that ngrok is disabled by default")
	}
}

// TestRemainingWorkAgreesWithTheReadme ensures the two documents do not
// contradict each other, which is how the original inconsistency arose.
func TestRemainingWorkAgreesWithTheReadme(t *testing.T) {
	root := repoRoot(t)
	remaining, err := os.ReadFile(filepath.Join(root, "docs", "REMAINING_WORK.md"))
	if err != nil {
		t.Skipf("docs/REMAINING_WORK.md not present: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	// Both must agree that Cloudflare is the only usable provider.
	remainingText := strings.ToLower(string(remaining))
	readmeText := strings.ToLower(string(readme))
	if strings.Contains(remainingText, "only cloudflare is a real provider") &&
		!strings.Contains(readmeText, "cloudflare is the only provider suitable for use") {
		t.Error("docs/REMAINING_WORK.md says only Cloudflare is real but the README does not say so")
	}
}
