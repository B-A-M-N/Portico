package supervisor

import (
	"context"
	"fmt"

	"github.com/B-A-M-N/portico/internal/ipc"
)

// credentialHealthCheck verifies that every stored credential record can
// actually be decrypted, via the store-owned read-only health API (audit
// P1-12). Doctor receives only counts, references and problem
// classifications — never plaintext.
func (h *supervisorHandler) credentialHealthCheck(ctx context.Context) ipc.HealthCheckDTO {
	check := ipc.HealthCheckDTO{ID: "credential_health", Title: "Stored credentials"}

	report, err := h.sup.store.CheckCredentialHealth(ctx)
	if err != nil {
		check.State = string(checkUnknown)
		check.Summary = "Portico could not verify its stored credentials."
		check.Technical = err.Error()
		return check
	}

	if report.Total == 0 {
		check.State = string(checkOK)
		check.Summary = "No credentials are stored yet, so none can be broken."
		return check
	}

	switch {
	case report.Unhealthy > 0:
		check.State = string(checkProblem)
		check.Summary = fmt.Sprintf(
			"%d of %d stored credential(s) cannot be decrypted.", report.Unhealthy, report.Total)
		check.Detail = "A wrong or replaced encryption key, a missing historical key " +
			"version after rotation, or corrupt records each produce this. Affected " +
			"credentials must be set up again; they cannot be recovered without the " +
			"key that made them."
		for _, rec := range report.Records {
			if rec.Problem == "" {
				continue
			}
			check.Detail += fmt.Sprintf("\n- %s %q: %s", rec.Kind, rec.Reference, rec.Problem)
		}
		check.NextAction = "Restore the original key files if you have a backup, or " +
			"set the affected accounts up again."
	default:
		check.State = string(checkOK)
		check.Summary = fmt.Sprintf(
			"All %d stored credential(s) decrypt correctly under the current key.", report.Total)
	}
	return check
}
