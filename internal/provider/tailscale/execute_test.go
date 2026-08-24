package tailscale

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/B-A-M-N/portico/internal/core"
)

// Executing and observing a private-network connection.
//
// Observation is what a restarted supervisor depends on: it has no memory of what it
// configured, so it observes from the resources it persisted. These tests reconstruct the
// provider — a fresh adapter — and observe from that inventory.

// stepFor finds a step by its technical type.
func stepFor(t *testing.T, plan *core.OperationPlan, kind string) core.PlanStep {
	t.Helper()
	for _, step := range plan.Steps {
		if step.Technical.Type == kind {
			return step
		}
	}
	t.Fatalf("no %q step in the plan", kind)
	return core.PlanStep{}
}

// TestVerifyingMembershipRecordsAnAdoptedResource pins the ownership the step asserts.
//
// The membership is recorded so the supervisor persists what was confirmed, and it is
// adopted rather than managed so nothing later deletes it.
func TestVerifyingMembershipRecordsAnAdoptedResource(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusRunning, nil)
	provider := New(runner)

	plan, err := provider.Plan(context.Background(),
		core.DesiredConnection{Profile: joinProfile(core.DesiredOpen)})
	if err != nil {
		t.Fatal(err)
	}

	result, err := provider.ExecuteStep(context.Background(), "conn-join",
		stepFor(t, plan, "verify_membership"))
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("the membership check failed: %v", result.Error)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("recorded %d resources, want the membership", len(result.Resources))
	}
	resource := result.Resources[0]
	if resource.Type != core.ResourceTailnetMembership {
		t.Fatalf("recorded a %s", resource.Type)
	}
	if resource.Ownership != core.OwnershipAdopted {
		t.Fatalf("membership recorded as %q; Portico did not create it and must not delete it",
			resource.Ownership)
	}
	if resource.ExternalID != "n1234567890123456" {
		t.Fatalf("membership recorded under %q, want the stable node ID", resource.ExternalID)
	}
}

// TestVerifyingMembershipFailsWhenSignedOut pins that the step does not pass optimistically.
func TestVerifyingMembershipFailsWhenSignedOut(t *testing.T) {
	runner := newFakeRunner().on("status --json", statusNeedsLogin, nil)
	provider := New(runner)

	result, err := provider.ExecuteStep(context.Background(), "conn-join", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{Provider: "tailscale", Type: "verify_membership"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded {
		t.Fatal("the membership check passed on a signed-out machine")
	}
	if !strings.Contains(result.Error.Error(), "tailscale up") {
		t.Errorf("the failure does not say what to do: %v", result.Error)
	}
}

// TestServingPublishesAndRecordsAManagedResource pins the expose execution.
func TestServingPublishesAndRecordsAManagedResource(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve --bg --http=3000 http://127.0.0.1:3000", "", nil).
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)
	provider := New(runner)

	plan, err := provider.Plan(context.Background(),
		core.DesiredConnection{Profile: exposeProfile(core.DesiredOpen, "127.0.0.1:3000")})
	if err != nil {
		t.Fatal(err)
	}

	result, err := provider.ExecuteStep(context.Background(), "conn-serve", stepFor(t, plan, "serve"))
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("the serve failed: %v", result.Error)
	}
	// --bg, or the client would hold the foreground and the step would never return.
	if !runner.called("serve --bg --http=3000 http://127.0.0.1:3000") {
		t.Fatalf("the serve was not run with the correct command: %v", runner.calls())
	}
	if len(result.Resources) != 1 {
		t.Fatalf("recorded %d resources", len(result.Resources))
	}
	resource := result.Resources[0]
	if resource.Type != core.ResourceTailnetServe {
		t.Fatalf("recorded a %s", resource.Type)
	}
	// This one Portico did create, so it is managed and gets withdrawn on close.
	if resource.Ownership != core.OwnershipManaged {
		t.Fatalf("the serve is recorded as %q; Portico created it", resource.Ownership)
	}
	// ExternalID is the frontend identity, not the backend target.
	if resource.ExternalID != "http:3000:/" {
		t.Fatalf("the serve is recorded under %q", resource.ExternalID)
	}
	// Metadata carries the canonical route fields.
	if resource.Metadata["frontend_protocol"] != "http" || resource.Metadata["backend_host"] != "127.0.0.1" {
		t.Fatalf("metadata does not carry the route: %v", resource.Metadata)
	}

	// And the verify step confirms the client actually reports it.
	verify, err := provider.ExecuteStep(context.Background(), "conn-serve",
		stepFor(t, plan, "verify_serve"))
	if err != nil {
		t.Fatal(err)
	}
	if !verify.Succeeded {
		t.Fatalf("the client accepted the serve but verification failed: %v", verify.Error)
	}
}

// TestServingHTTPSPublishesAndRecordsAManagedResource pins the HTTPS expose path.
func TestServingHTTPSPublishesAndRecordsAManagedResource(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve --bg --https=8443 https://127.0.0.1:8443", "", nil).
		on("serve status --json", serveHTTPS("127.0.0.1:8443"), nil)
	provider := New(runner)

	plan, err := provider.Plan(context.Background(),
		core.DesiredConnection{Profile: exposeProfileWithProtocol(core.DesiredOpen, "127.0.0.1:8443", core.ProtocolHTTPS)})
	if err != nil {
		t.Fatal(err)
	}

	result, err := provider.ExecuteStep(context.Background(), "conn-serve", stepFor(t, plan, "serve"))
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("the serve failed: %v", result.Error)
	}
	if !runner.called("serve --bg --https=8443 https://127.0.0.1:8443") {
		t.Fatalf("the serve was not run with the correct command: %v", runner.calls())
	}
	if result.Resources[0].ExternalID != "https:8443:/" {
		t.Fatalf("the serve is recorded under %q", result.Resources[0].ExternalID)
	}
}

// TestServingTCPPublishesAndRecordsAManagedResource pins the TCP expose path.
func TestServingTCPPublishesAndRecordsAManagedResource(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve --bg --tcp=5432 tcp://127.0.0.1:5432", "", nil).
		on("serve status --json", serveTCP("127.0.0.1:5432"), nil)
	provider := New(runner)

	plan, err := provider.Plan(context.Background(),
		core.DesiredConnection{Profile: exposeProfileWithProtocol(core.DesiredOpen, "127.0.0.1:5432", core.ProtocolTCP)})
	if err != nil {
		t.Fatal(err)
	}

	result, err := provider.ExecuteStep(context.Background(), "conn-serve", stepFor(t, plan, "serve"))
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if !result.Succeeded {
		t.Fatalf("the serve failed: %v", result.Error)
	}
	if !runner.called("serve --bg --tcp=5432 tcp://127.0.0.1:5432") {
		t.Fatalf("the serve was not run with the correct command: %v", runner.calls())
	}
	if result.Resources[0].ExternalID != "tcp:5432:/" {
		t.Fatalf("the serve is recorded under %q", result.Resources[0].ExternalID)
	}
}

// TestAServeTheClientDoesNotReportFailsVerification pins that acceptance is not proof.
//
// The client exiting zero means the request was accepted. Whether the tailnet is serving
// the address is a separate question, and reporting an unserved address as open would
// leave the user with a connection Portico calls healthy and nothing can reach.
func TestAServeTheClientDoesNotReportFailsVerification(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve --bg --http=3000 http://127.0.0.1:3000", "", nil).
		on("serve status --json", serveNothing, nil)

	result, err := New(runner).ExecuteStep(context.Background(), "conn-serve", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{
			Provider: "tailscale", Type: "verify_serve",
			Parameters: map[string]string{
				"frontend_protocol": "http", "frontend_port": "3000", "frontend_path": "/",
				"backend_protocol": "http", "backend_host": "127.0.0.1", "backend_port": "3000",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded {
		t.Fatal("verification passed while the client reported no serve")
	}
}

// TestVerifyingTheOriginRefusesAnAddressNothingListensOn pins the check that saves a
// confusing failure later.
func TestVerifyingTheOriginRefusesAnAddressNothingListensOn(t *testing.T) {
	// Port 1 on loopback: reserved and not listening.
	result, err := New(newFakeRunner()).ExecuteStep(context.Background(), "conn-serve",
		core.PlanStep{
			ID: "s", Technical: core.TechnicalOperation{
				Provider: "tailscale", Type: "verify_origin",
				Parameters: map[string]string{
					"frontend_protocol": "tcp", "frontend_port": "1", "frontend_path": "/",
					"backend_protocol": "tcp", "backend_host": "127.0.0.1", "backend_port": "1",
				},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded {
		t.Fatal("an address nothing is listening on was accepted for publishing")
	}
	if !strings.Contains(result.Error.Error(), "listening") {
		t.Errorf("the failure does not say what is wrong: %v", result.Error)
	}
}

// TestClosingWithdrawsExactlyWhatWasServed pins the close path.
func TestClosingWithdrawsExactlyWhatWasServed(t *testing.T) {
	runner := newFakeRunner().
		on("status --json", statusRunning, nil).
		on("serve --bg --http=3000 off", "", nil)

	result, err := New(runner).ExecuteStep(context.Background(), "conn-serve", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{
			Provider: "tailscale", Type: "unserve",
			Parameters: map[string]string{
				"frontend_protocol": "http", "frontend_port": "3000", "frontend_path": "/",
				"backend_protocol": "http", "backend_host": "127.0.0.1", "backend_port": "3000",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		t.Fatalf("the withdrawal failed: %v", result.Error)
	}
	if !runner.called("serve --bg --http=3000 off") {
		t.Fatalf("the exact served frontend was not withdrawn: %v", runner.calls())
	}
}

// TestClosingTwiceSucceeds pins that a repeated close is not an error.
//
// The desired state is that the address is not served. If it already is not, that state
// has been reached, and failing the close would leave a connection that cannot be closed.
func TestClosingTwiceSucceeds(t *testing.T) {
	runner := newFakeRunner().
		on("serve --bg --http=3000 off", "serve config does not exist", errors.New("exit status 1")).
		on("serve status --json", serveNothing, nil)

	result, err := New(runner).ExecuteStep(context.Background(), "conn-serve", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{
			Provider: "tailscale", Type: "unserve",
			Parameters: map[string]string{
				"frontend_protocol": "http", "frontend_port": "3000", "frontend_path": "/",
				"backend_protocol": "http", "backend_host": "127.0.0.1", "backend_port": "3000",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		t.Fatalf("closing an already-closed serve failed: %v", result.Error)
	}
}

// TestAFailedWithdrawalIsReported pins the opposite case.
//
// A serve that is still running after the user asked to stop keeps the service reachable.
// Reporting success would tell them it is closed when it is not.
func TestAFailedWithdrawalIsReported(t *testing.T) {
	runner := newFakeRunner().
		on("serve --bg --http=3000 off", "permission denied", errors.New("exit status 1")).
		// Still serving, so the failure was real.
		on("serve status --json", serveHTTP("127.0.0.1:3000"), nil)

	result, err := New(runner).ExecuteStep(context.Background(), "conn-serve", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{
			Provider: "tailscale", Type: "unserve",
			Parameters: map[string]string{
				"frontend_protocol": "http", "frontend_port": "3000", "frontend_path": "/",
				"backend_protocol": "http", "backend_host": "127.0.0.1", "backend_port": "3000",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded {
		t.Fatal("a serve that is still running was reported as withdrawn")
	}
}

// TestReleasingAJoinTouchesNothing pins that closing a join runs no client command.
func TestReleasingAJoinTouchesNothing(t *testing.T) {
	runner := newFakeRunner()
	result, err := New(runner).ExecuteStep(context.Background(), "conn-join", core.PlanStep{
		ID: "s", Technical: core.TechnicalOperation{Provider: "tailscale", Type: "release"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded {
		t.Fatalf("releasing a join failed: %v", result.Error)
	}
	if len(runner.calls()) != 0 {
		t.Fatalf("releasing a join ran %v; it must not change the machine", runner.calls())
	}
}
