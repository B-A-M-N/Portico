# Audit acceptance matrix

Every finding in the production-readiness audit, mapped to the test that pins
it, the behaviour that test asserts, and the commit that introduced it.

Verification commands, all clean on this branch:

```bash
gofmt -l .            # no output
go build ./...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
```

125 tests were added across 21 commits.

## P0 — release blockers

| # | Finding | Test | Asserts | Commit |
|---|---------|------|---------|--------|
| 1 | Test suite does not compile | whole suite | `go vet ./...` and `go test ./...` compile and pass; fixtures rebuilt on the tagged union | `96d4f43` |
| 2 | SQLite persistence incompatible with tagged model | `TestSaveProfileRoundTripPreservesTaggedUnion`, `TestProfileKindIsDurableNotInferred`, `TestLoadProfileRejectsUnknownSpecVersion`, `TestLoadProfileRejectsAmbiguousSpecArms`, `TestMigration18ConvertsLegacyProfileAndPreservesDependents`, `TestMigration18AbortsOnUndecodableLegacyRow`, `TestMigration18TakesRecoveryBackupAndIsIdempotent` | `SaveProfile` writes a declared column; load allocates the union arm and restores `Kind` from storage; unknown spec versions and ambiguous arms are refused; migration 18 converts legacy rows, preserves FK dependents, aborts atomically on corrupt JSON, takes a recovery backup, and is idempotent | `96d4f43`, `f2b1a50` |
| 3 | Kinds advertised but not executable | `TestSaveProfilePersistsNonServiceExposureKinds`, `TestClientTunnelProfileRoundTrips` | storage represents every kind and never panics; `client_tunnel` is executable end to end | `f2b1a50`, `45b0616` |
| 4 | Ngrok advertised as implemented | `TestAgentProcessSpecNeverPlacesTokenInArgv`, `TestNgrokIsNotDescribedAsUsable`, `TestReadmeProviderTableMatchesAdapterDescriptors` | auth token never in argv but still delivered via env; README advertises no protection the adapter omits; documented status matches the descriptor | `96d4f43`, `51f8906` |
| 5 | ChatGPT Secure MCP Tunnel absent | `TestTunnelNeverPlansAPublicAddress`, `TestCapabilitiesForbidPublicExposure`, `TestCredentialNeverEntersArgv`, `TestUnreachableMCPServerFailsBeforeAnythingStarts`, `TestTunnelResourceIsAdoptedNotManaged`, `TestPrivateAndPublicProvidersAreNotInterchangeable`, `TestUnavailableOutcomeIsRefusedRatherThanSubstituted` | private connection kind that never plans a public address; capabilities forbid public exposure; credential env-only; origin verified before any start; tunnel adopted not managed; public and private providers never substitute for each other | `45b0616`, `515926b` |
| 6 | Fake provider recommendation | `TestRecommendationHonoursStatedRequirements`, `TestRecommendationReturnsNothingWhenNoProviderQualifies`, `TestUnusableProvidersAreNeverRecommended`, `TestRecommendationRejectsSSEOverTemporaryAddress`, `TestRecommendationPrefersAuthenticatedAndSelectedAccount`, `TestRecommendationStatesTradeoffs` | requirements drive the result; ineligible providers returned with reasons; no recommendation when nothing qualifies; unusable providers never offered; tradeoffs stated | `01fe6b1` |
| 7 | Operation history returns empty success | `TestListRecentOperationsJoinsPlanIdentity`, `TestListRecentOperationsRespectsLimit`, `TestOperationHistoryReadsDurableJournal`, `TestOperationHistoryEmptyIsMarkedAvailable`, `TestOperationsScreenDistinguishesEmptyFromUnavailable`, `TestOperationsScreenShowsIntentNotPlanID`, `TestOperationsScreenStylesCompletedAsSuccess` | history read from the journal with plan identity joined; empty is distinguished from unavailable; intent shown rather than plan ID; completed styled as success | `8505c09` |
| 8 | Connection detail not wired into inspect | `TestEnterOnConnectionLoadsAuthoritativeDetail`, `TestStaleConnectionDetailIsIgnored`, `TestConnectionDetailErrorKeepsPreviousDetail`, `TestConnectionDetailRouteIsServed`, `TestConnectionDetailRouteMapsTypedErrors`, `TestConnectionDetailReportsDurableResourcesWithoutRuntime` | detail fetched on entry and rendered with exact external IDs; stale replies ignored; failed refresh preserves state; route served and maps typed errors; durable resources reported when the runtime projection has none | `c9a9335` |
| 9 | Placeholder activity and logs | `TestActivityTabDoesNotFabricateMetrics` | the `-- / -- / --` metrics table cannot return; telemetry is stated as not collected | `c9a9335` |
| 10 | Cloudflare setup contradiction | `TestCloudflareSetupAcceptsAccountWithoutZone`, `TestCloudflareSetupDoesNotPersistUnvalidatedCredentials`, `TestCloudflareSetupRejectsZoneNotVisibleToToken`, `TestConfigureCloudflareAccountPersistsEncryptedAccountForRestart` | zone optional as the UI states; credential validated before persistence and never stored unvalidated; invisible zone refused by name; validation runs exactly once | `6946ff9` |
| 11 | Unavailable providers silently omitted | `TestCatalogEntriesRemainVisibleWithoutAnAdapter`, `TestRegisteredAdapterSupersedesCatalogEntry`, `TestCapabilityErrorIsReportedNotDiscarded`, `TestSnapshotDoesNotHoldTheLockDuringCapabilityQueries`, `TestSnapshotIsConcurrencySafe` | catalogued providers stay visible with reason and setup actions; adapters supersede entries; capability errors reported as degraded; registry lock not held during capability queries | `4243585` |
| 12 | Verification sequencing | `TestOpenPlanVerifiesOriginBeforeAnyProviderMutation`, `TestVerifyOriginStepFailsWhenOriginUnreachable`, `TestVerifyOriginStepRefusesMissingOriginURL`, `TestStartOriginPrecedesOriginVerification`, `TestStartOriginStillAnchorsToConnectorWithoutVerifyStep` | no tunnel, DNS or Access step precedes origin verification in any exposure mode; `verify_endpoint` carries `origin_url`; missing origin URL fails rather than passing vacuously; owned origins start before the probe | `b307fba` |
| 13 | Duplicate execution architectures | `TestAdaptersDoNotOwnOperationOrchestration`, `TestAdaptersDoNotPersistState` | no adapter defines `Apply`/`Repair`/`Remove`; no adapter imports the store. 1211 lines removed | `0082c71` |

## P1 — UI and UX

| # | Finding | Test | Asserts | Commit |
|---|---------|------|---------|--------|
| 14 | Provider-first technical wizard | `TestWizardOpensOnAnOutcomeQuestion`, `TestChoosingAnOutcomeSkipsTheQuestionsItAnswers`, `TestAdvancedOutcomeFallsBackToTheSourceQuestion` | wizard opens on "What are you trying to do?"; outcomes preset what they determine; advanced path presets nothing | `515926b` |
| 15 | Progressive disclosure | `TestWizardOpensOnAnOutcomeQuestion` | the consequence of the highlighted outcome is shown before it is chosen, in plain language | `515926b` |
| 16 | Plan preview | `TestPlanPreviewStatesOutcomeAndConsequences`, `TestPlanPreviewDescribesUnrestrictedAccessPlainly`, `TestDeletePreviewStatesIrreversibility`, `TestPlanPreviewWithoutProfileInventsNothing` | preview states outcome, access, local and provider changes and reversibility while retaining the technical steps; a missing profile invents nothing | `89f8d67` |
| 17 | Editing and cloning | `TestUpdateConnectionRefusesSilentlyIgnoredChanges`, `TestUpdateConnectionHonoursExpectedRevision`, `TestCloneConnectionProducesAnIndependentCopy`, `TestCloneRequiresANewHostnameForPermanentConnections` | spec/driver/lifecycle changes refused rather than silently dropped; optimistic concurrency honoured; clone is independent and closed; permanent clones need a new hostname | `446a20f` |
| 18 | TUI credential handling | `TestCredentialIsClearedOnEveryExitPath`, `TestCredentialNeverAppearsInAnyRenderedView` | credential cleared on back, cancel, failure, quit and success; never rendered in any view, status line or error | `bb4efba` |
| 19 | Technical errors not actionable | `TestStructuredSupervisorErrorsBecomeInterventions`, `TestCommonTransportFailuresGetSpecificRemedies`, `TestUnrecognisedErrorsStillCarryTheirText`, `TestStatusLineIsSingleLine`, `TestDescribeNilError` | structured errors become interventions with next actions; transport failures get specific remedies; text never lost; status line single-line | `ce22133` |
| 20 | Polling instead of event stream | `TestWizardUsesEventStreamRatherThanTightPolling`, `TestWizardExposesItsOperationForEventRouting` | event-driven refresh waits on no timer; polling demoted to a fallback | `25b34c4` |
| 21 | Terminal text handling | `TestEditStringIsRuneAware`, `TestEditStringBackspaceRemovesWholeRunes`, `TestEditStringIgnoresNamedAndControlKeys`, `TestWizardEditInputIsRuneAware` | accented Latin, Arabic, CJK, emoji and combining marks reach the field; backspace never yields invalid UTF-8 | `6b50327` |
| 22 | Comma-separated command arguments | `TestCommandArgsSupportValuesContainingCommas`, `TestCommandArgsRoundTripThroughTheInputLine`, `TestCommandArgsRejectUnbalancedQuoting`, `TestArgvPreviewShowsExactArguments` | an argument containing a comma round-trips; unbalanced quoting rejected; exact argv previewed | `6b50327` |

## Provider, account and observability

| # | Finding | Test | Asserts | Commit |
|---|---------|------|---------|--------|
| 23 | Provider setup not provider-neutral | `TestProviderSetupIsDeclarative`, `TestUnknownProviderSetupIsRefusedByCapabilityNotByName` | providers declare fields, secrecy and requirements; configurability decided by capability, not provider ID | `db70847` |
| 24 | Account lifecycle | `TestRemovingAnAccountReportsDependentConnections`, `TestRemovingAnUnusedAccountDeletesItsCredential` | removal refused while connections depend on it, naming them; credential deleted with the account | `db70847` |
| 25 | Health not segment-based | `TestSegmentsLocateTheFaultRatherThanReportingOneVerdict`, `TestSegmentsDoNotClaimHealthWithoutEvidence`, `TestSegmentsMarkInapplicableHopsExplicitly`, `TestOpenFindingsOverrideInferredSegmentState`, `TestClosedConnectionSegmentsAreNotFailures` | per-segment state locates the fault; unknown never upgraded to ok; inapplicable hops marked; findings override inference | `4475134` |
| 26 | Route and resource identity | `TestEnterOnConnectionLoadsAuthoritativeDetail`, `TestSegmentsLocateTheFaultRatherThanReportingOneVerdict` | exact external IDs and process identity rendered; segments explain them | `c9a9335`, `4475134` |
| 27 | Support-safe diagnostic export | `TestSupportExportCarriesNoSecrets`, `TestSupportExportRedactsBySubstringNotExactKey` | credentials and allowed identities absent from the serialised export while resource identity and schema version are retained; redaction matches realistic key names | `4475134` |
| 28 | Documentation contradictions | `TestReadmeProviderTableMatchesAdapterDescriptors`, `TestNgrokIsNotDescribedAsUsable`, `TestRemainingWorkAgreesWithTheReadme` | documented status derived from adapter descriptors; drift fails the build | `51f8906` |

## Security gates

| Requirement | Test | Commit |
|-------------|------|--------|
| No secrets in argv | `TestAgentProcessSpecNeverPlacesTokenInArgv`, `TestCredentialNeverEntersArgv` | `96d4f43`, `45b0616` |
| No secrets in diagnostics exports | `TestSupportExportCarriesNoSecrets` | `4475134` |
| No secrets in rendered views | `TestCredentialNeverAppearsInAnyRenderedView` | `bb4efba` |
| Credentials stored only after validation | `TestCloudflareSetupDoesNotPersistUnvalidatedCredentials` | `6946ff9` |
| Public exposure never substituted for a private connection | `TestPrivateAndPublicProvidersAreNotInterchangeable`, `TestUnavailableOutcomeIsRefusedRatherThanSubstituted` | `45b0616`, `515926b` |
| Provider resources deleted only when ownership established | `TestTunnelResourceIsAdoptedNotManaged` | `45b0616` |

## Known limitations, deliberately not closed

These are stated rather than silently left open.

- **ngrok** remains scaffolding behind `PORTICO_ENABLE_EXPERIMENTAL_NGROK=1`. It
  is not rebuilt; it is gated, and the documentation says so.
- **OpenAI Secure MCP Tunnel** has not been exercised against a live tunnel.
  Tunnel creation, MCP tool enumeration, permission review and ChatGPT app
  registration detection are absent, not stubbed.
- **Port forward and private network** kinds persist and validate but have no
  provider that executes them.
- **Editing a connection's runtime-affecting fields** is refused rather than
  implemented; a delta change plan does not exist. Clone is the safe path.
- **Per-connection log capture** is not implemented; the inspect screen says so
  rather than showing an empty list.
- **Provider adapter rebuild** still requires a supervisor restart.
