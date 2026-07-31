package supervisor

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/B-A-M-N/portico/internal/core"
	"github.com/B-A-M-N/portico/internal/ipc"
)

// secretIndicators name the substrings that mark a value as sensitive. A value
// whose key matches is never exported, regardless of what it contains.
var secretIndicators = []string{
	"token", "secret", "password", "credential", "key", "authorization",
	"cookie", "session", "signature", "private",
}

// isSecretKey reports whether a metadata key should be withheld from an export.
//
// The export is meant to be safe to attach to a bug report. It therefore
// operates as an allowlist of known-safe facts plus a denylist over
// free-form metadata, rather than dumping state and hoping nothing sensitive is
// present.
func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, indicator := range secretIndicators {
		if strings.Contains(lower, indicator) {
			return true
		}
	}
	return false
}

// redactMetadata copies metadata, replacing sensitive values with a marker so
// the reader can see that a field existed without learning its value.
func redactMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			out[k] = "[redacted]"
			continue
		}
		out[k] = v
	}
	return out
}

// HandleSupportExport produces a redacted diagnostic report for a bug report.
//
// It deliberately carries no API tokens, authorization headers, cookies, private
// keys, command environments or full process environments. Connection profiles
// are exported without their secrets, and provider accounts appear as
// identifiers and status only — never with their credential reference resolved.
func (h *supervisorHandler) HandleSupportExport() (*ipc.SupportExportDTO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	export := &ipc.SupportExportDTO{
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		GoVersion:       runtime.Version(),
		SocketPath:      h.sup.paths.SocketPath,
		DatabasePath:    h.sup.paths.DatabasePath,
		SupervisorReady: h.sup.ready,
		Reviewed:        false,
	}

	if version, err := h.sup.store.SchemaVersion(ctx); err == nil {
		export.SchemaVersion = version
	}

	for _, snap := range h.sup.registry.Snapshot() {
		entry := ipc.SupportProviderDTO{
			ID:           string(snap.ID),
			Availability: string(snap.Availability),
			Reason:       snap.Reason,
			Accounts:     len(snap.Accounts),
		}
		export.Providers = append(export.Providers, entry)
	}

	for _, profile := range h.sup.controller.ListProfiles() {
		entry := ipc.SupportConnectionDTO{
			ID:           string(profile.ID),
			Kind:         string(profile.EffectiveKind()),
			Revision:     profile.Revision,
			DesiredState: string(profile.Desired),
			ProviderID:   string(profile.Driver.ProviderID),
			AccountID:    string(profile.Driver.AccountID),
		}
		if spec := profile.Spec.ServiceExposure; spec != nil {
			entry.SourceKind = string(spec.Source.Kind)
			entry.ExposureMode = string(spec.Exposure.Mode)
			entry.ProtectionKind = string(spec.Protection.Kind)
			// The hostname is part of the configuration under investigation and
			// is not itself a secret, but allowed identities are personal data
			// and are reported only as a count.
			entry.RequestedAddress = spec.Exposure.RequestedAddress
			entry.AllowedIdentityCount = len(spec.Protection.AllowedEmails) + len(spec.Protection.AllowedDomains)
		}

		if rt, ok := h.sup.controller.GetRuntime(profile.ID); ok && rt != nil {
			entry.RuntimeState = string(rt.State)
			entry.ConnectorState = string(rt.Connector.Status)
			entry.ConnectorPID = rt.Connector.PID
			entry.Restarts = rt.Connector.Restarts
			if rt.Error != nil {
				entry.LastError = rt.Error.Message
			}
			for _, r := range rt.Provider.Resources {
				entry.Resources = append(entry.Resources, ipc.SupportResourceDTO{
					Type:       string(r.Type),
					ExternalID: r.ExternalID,
					Ownership:  string(r.Ownership),
					Lifecycle:  string(r.Lifecycle),
					Metadata:   redactMetadata(r.Metadata),
				})
			}
			for _, f := range rt.Diagnostics {
				if f.ResolvedAt != nil {
					continue
				}
				entry.Findings = append(entry.Findings, fmt.Sprintf("[%s] %s: %s", f.Severity, f.Segment, f.Summary))
			}
		}
		export.Connections = append(export.Connections, entry)
	}

	if summaries, err := h.sup.store.ListRecentOperations(ctx, 25); err == nil {
		for _, s := range summaries {
			export.RecentOperations = append(export.RecentOperations, ipc.SupportOperationDTO{
				ID:           string(s.ID),
				ConnectionID: string(s.ConnectionID),
				Intent:       s.Intent,
				State:        s.State,
				StartedAt:    s.StartedAt,
				CompletedAt:  s.CompletedAt,
				Error:        s.Error,
			})
		}
	} else {
		export.Notes = append(export.Notes, "operation history could not be read: "+err.Error())
	}

	export.Notes = append(export.Notes,
		"This report excludes API tokens, credentials, command environments and allowed identities.",
		"Review it before sharing.")
	return export, nil
}

// accountStatusUnknown documents that an account row existing is not evidence
// of a working credential.
var _ = core.AccountPending
