# Acceptance matrix — beginner-trust audit

Covers the 42-finding audit of whether Portico can claim a beginner-friendly,
trustworthy multi-connection TUI. Every requirement worked in that remediation
is mapped to the test that holds it, the behaviour that test pins, and the
commit that introduced it.

**This is not the only matrix.** `AUDIT_ACCEPTANCE_MATRIX.md` covers the earlier
production-readiness audit. The two describe different audits and neither
supersedes the other; check both before concluding something is unverified.

Run everything with one command:

```
make validate
```

That runs `gofmt -l`, `go build ./...`, `go vet ./...`, `staticcheck ./...`,
`go test ./... -count=1` and `go test -race ./... -count=1`. Every row below
passes under it as of `3f1a67b`.

Run a single row with the command in its Command column.

---

## 1. Operation state vocabulary (findings 2, 3)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| An operation is never waited on for a state it cannot reach | `TestAnOperationNeverReachesAStepState` (`internal/ipc/operation_state_test.go`) | `completed`/`failed` are operation states; `succeeded` is a step state. The two vocabularies cannot be confused. | `go test ./internal/ipc/ -run AnOperationNeverReaches` | `9dd4aee` |
| Steps are rebuilt from the whole journal | `TestStepsAreRebuiltFromEveryEventNotTheLast` (`internal/supervisor/operation_steps_test.go`) | State, error and timings survive reconstruction instead of being overwritten by the last event. | `go test ./internal/supervisor/ -run StepsAreRebuilt` | `1bc21c5` |
| A rolled-back step is not a successful one | `TestCompensationIsDistinguishedFromSuccessAndFailure` | `compensated` and `compensation_failed` are distinct outcomes. A rollback that failed — something created and not removable — is not an ordinary failure. | `go test ./internal/supervisor/ -run CompensationIsDistinguished` | `1bc21c5` |
| A step with no start event still appears | `TestAStepWithNoStartEventIsStillReported` | Missing events do not erase a step from the record. | `go test ./internal/supervisor/ -run StepWithNoStartEvent` | `1bc21c5` |
| Replay is idempotent | `TestAReplayedEventDoesNotChangeTheOutcome` | Re-reading the journal produces the same result. | `go test ./internal/supervisor/ -run ReplayedEvent` | `1bc21c5` |

## 2. Correlating asynchronous replies (findings 1, 4, 7, 36)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| A plan is never previewed under another connection's name | `TestAPlanNeverAppearsUnderAnotherConnectionsName` (`internal/tui/correlation_test.go`) | The preview takes identity from the plan, not the cursor. | `go test ./internal/tui/ -run PlanNeverAppears` | `8028b31` |
| An abandoned plan is dropped | `TestAPlanTheUserMovedOnFromIsDropped` | A superseded request's reply installs nothing. | `go test ./internal/tui/ -run PlanTheUserMovedOn` | `8028b31` |
| Diagnostics never attach to another connection | `TestDiagnosticsNeverAttachToAnotherConnection` | Findings gathered for one connection are not shown against another. | `go test ./internal/tui/ -run DiagnosticsNeverAttach` | `8028b31` |
| A failed check is not an empty result | `TestAFailedDiagnosticIsNotAnEmptyResult` | A check that could not run is recorded as failed, not as a healthy connection. | `go test ./internal/tui/ -run FailedDiagnosticIsNot` | `8028b31` |
| Repair is verified by identity, not count | `TestRepairIsVerifiedByWhichFindingsChanged` | Resolved, remaining and newly appeared findings are distinguished. Fixing DNS while breaking the connector is not "unchanged". | `go test ./internal/tui/ -run RepairIsVerifiedBy` | `8028b31` |
| A late plan cannot reopen a dismissed screen | `TestLeavingThePreviewWithQAbandonsThePlan` (`internal/tui/scroll_test.go`) | Both exits abandon the request, not just `esc`. | `go test ./internal/tui/ -run LeavingThePreviewWithQ` | `4b4f5a6` |
| A late edit plan opens nothing | `TestALateEditPlanDoesNotOpenAPreview` (`internal/tui/edit_test.go`) | Same rule on the edit path. | `go test ./internal/tui/ -run ALateEditPlan` | `d58736b` |
| A late removal reports against nothing | `TestALateRemovalReplyDoesNotReportAgainstAnotherAccount` (`internal/tui/accounts_test.go`) | Same rule on the account path. | `go test ./internal/tui/ -run ALateRemovalReply` | `115f058` |
| A journal is not shown under another operation | `TestAJournalIsNotShownAgainstAnotherOperation` (`internal/tui/refresh_test.go`) | Same rule on the history path. | `go test ./internal/tui/ -run JournalIsNotShown` | `e95e2a4` |

## 3. Describing a connection as the kind it is (finding 8)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| A port forward is not described as exposed | `TestAPortForwardIsNotDescribedAsAnExposedService` (`internal/supervisor/segments_kind_test.go`) | No "temporary address", no "provider tunnel", no "public endpoint", and never "anyone with the address can reach it" for a loopback forward. | `go test ./internal/supervisor/ -run PortForwardIsNotDescribed` | `95b6f3c` |
| A client tunnel is never described as public | `TestAClientTunnelIsNeverDescribedAsPublic` | The kind whose defining property is having no public address is not given one. | `go test ./internal/supervisor/ -run ClientTunnelIsNever` | `95b6f3c` |
| A private network is not described as unprotected | `TestAPrivateNetworkIsNotDescribedAsUnprotected` | Reachability is membership, and is reported as such. | `go test ./internal/supervisor/ -run PrivateNetworkIsNot` | `95b6f3c` |
| **An exposed service still warns** | `TestAServiceExposureStillDescribesItsPublicRoute`, `TestAnExposedServiceStillWarnsThatAnyoneCanReachIt` | Making the other kinds honest did not soften the warning on the kind that genuinely is open to the internet. | `go test ./internal/supervisor/ -run ServiceExposureStill && go test ./internal/supervisor/ -run ExposedServiceStillWarns` | `95b6f3c` |
| A diagnosed fault still overrides | `TestAnOpenFindingStillOverridesEveryKind` | Per-kind routes did not lose the finding override. | `go test ./internal/supervisor/ -run OpenFindingStill` | `95b6f3c` |
| A kindless profile is read from its spec | `TestAKindlessProfileIsReadFromItsSpec` | The populated spec arm is the authority, matching what the store enforces. | `go test ./internal/supervisor/ -run KindlessProfile` | `95b6f3c` |
| The wire format is a real union | `TestTheSpecCrossesTheWireAsTheKindItIs`, `TestEachKindPopulatesExactlyOneArm` | Exactly one arm is populated and `Kind` says which. | `go test ./internal/supervisor/ -run SpecCrossesTheWire && go test ./internal/supervisor/ -run EachKindPopulates` | `95b6f3c` |
| The list and the detail agree | `TestTheListAndTheDetailAgreeOnWhatAConnectionIs` | One summary builder, so a forward cannot read as a forward in the list and a published service on its own screen. | `go test ./internal/supervisor/ -run ListAndTheDetailAgree` | `95b6f3c` |
| The preview says who can reach each kind | `TestThePreviewSaysWhoCanReachEachKind` | The screen a user approves an open from answers the question for all four kinds. | `go test ./internal/supervisor/ -run PreviewSaysWho` | `95b6f3c` |
| A healthy forward is not diagnosed as broken | `TestAHealthyPortForwardIsNotDiagnosedAsABrokenTunnel` (`internal/diagnostics/engine_kind_test.go`) | "Open with no public address" is the permanent healthy state of three kinds, not a provider-edge failure. **Found by adversarial review after the description was already fixed.** | `go test ./internal/diagnostics/ -run HealthyPortForward` | `95b6f3c` |
| …nor a client tunnel, nor a private-only exposure | `TestAHealthyClientTunnelIsNotDiagnosedAsABrokenTunnel`, `TestAPrivateOnlyExposureIsNotDiagnosedAsABrokenTunnel` | The same defect one level in: mode `private_only` is the right kind and still has no public address. | `go test ./internal/diagnostics/ -run HealthyClientTunnel && go test ./internal/diagnostics/ -run PrivateOnlyExposure` | `95b6f3c` |
| **A published service with no address is still a fault** | `TestAPublishedServiceWithNoAddressIsStillAFault` | Suppression did not extend to the kind that should have an address. Covers empty, temporary and permanent modes. | `go test ./internal/diagnostics/ -run PublishedServiceWithNoAddress` | `95b6f3c` |
| The inspect screen describes a forward as a forward | `TestTheInspectScreenDescribesAForwardAsAForward` (`internal/tui/screens/inspect_kind_test.go`) | The client half: one route, drawn from the supervisor's segments. | `go test ./internal/tui/screens/ -run InspectScreenDescribesAForward` | `95b6f3c` |
| A route is not invented | `TestARouteWithNoSegmentsSaysSoRatherThanDrawingOne` | No segments means "no route is established", not a drawn one. | `go test ./internal/tui/screens/ -run RouteWithNoSegments` | `95b6f3c` |
| Both kind predicates state what they mean | `TestIsProtectedIsPositive`, `TestExpectsPublicAddressIsNegative` (`internal/core/connection_test.go`) | `IsProtected` is positive (unset means none); `ExpectsPublicAddress` is negative (unset means the public default), so a diagnostic fails toward reporting a fault. | `go test ./internal/core/ -run IsProtectedIsPositive && go test ./internal/core/ -run ExpectsPublicAddressIsNegative` | `3f1a67b` |

## 4. Text entry and screen behaviour (findings 14, 15, 16, 19)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| A typo can be fixed without retyping | `TestATypoInTheMiddleCanBeFixedWithoutRetypingTheRest` (`internal/tui/screens/field_test.go`) | The field has a cursor. | `go test ./internal/tui/screens/ -run TypoInTheMiddle` | `cfa8b2e` |
| Home and end work | `TestHomeAndEndReachBothEndsOfTheValue` | Standard motion. | `go test ./internal/tui/screens/ -run HomeAndEnd` | `cfa8b2e` |
| Paste is accepted, at the cursor | `TestAPastedValueIsAccepted`, `TestAPasteLandsAtTheCursorNotTheEnd` | Paste arrives as its own message and reaches the field. | `go test ./internal/tui/screens/ -run PastedValue && go test ./internal/tui/screens/ -run PasteLands` | `cfa8b2e` |
| A credential can be pasted | `TestACredentialCanBePasted` (`internal/tui/correlation_test.go`) | The field most likely to hold a clipboard value. | `go test ./internal/tui/ -run CredentialCanBePasted` | `cfa8b2e` |
| A secret is masked as typed | `TestASecretFieldDoesNotEchoWhatIsTyped` | The value is not rendered, and masking does not change what is submitted. | `go test ./internal/tui/screens/ -run SecretFieldDoesNotEcho` | `cfa8b2e` |
| **Quitting outranks typing** | `TestQuittingOutranksTyping` | `ctrl+c` quits and clears the credential rather than being typed into it. Caught by an existing test when the routing order was wrong. | `go test ./internal/tui/ -run QuittingOutranksTyping` | `cfa8b2e` |
| The field does not swallow navigation | `TestNavigationKeysAreNotSwallowedByTheField`, `TestAMenuQuestionDoesNotRouteToTheField` | Enter and esc belong to the screen; menus do not collect text. | `go test ./internal/tui/screens/ -run NavigationKeysAreNot && go test ./internal/tui/screens/ -run MenuQuestionDoesNot` | `cfa8b2e` |
| Content taller than the terminal is reachable | `TestContentTallerThanTheTerminalCanBeReached` (`internal/tui/scroll_test.go`) | Clipping happens where content is rendered, so what is cut can be scrolled to. | `go test ./internal/tui/ -run ContentTallerThan` | `cfa8b2e` |
| Hidden content is announced | `TestTheUserIsToldThereIsMore` | How much is above and below, and which keys move it. | `go test ./internal/tui/ -run UserIsToldThereIsMore` | `cfa8b2e` |
| The viewport never overflows | `TestTheViewportNeverExceedsTheTerminalHeight` | Across heights 3…50, including the indicator line. | `go test ./internal/tui/ -run ViewportNeverExceeds` | `cfa8b2e` |
| Scrolling stops at the end | `TestScrollingStopsAtTheEnd` | No blank space past the bottom. | `go test ./internal/tui/ -run ScrollingStopsAtTheEnd` | `cfa8b2e` |
| Content that fits is untouched | `TestShortContentIsNotClipped` | No indicator, no reserved line. | `go test ./internal/tui/ -run ShortContentIsNotClipped` | `cfa8b2e` |
| A screen opens at the top | `TestOpeningAScreenStartsAtTheTop` | Detected at render, so every navigation path is covered. | `go test ./internal/tui/ -run OpeningAScreenStarts` | `cfa8b2e` |
| The list is not scrolled twice | `TestTheConnectionListIsNotScrolledTwice` | The selection-following viewport and the free one do not both act. | `go test ./internal/tui/ -run ConnectionListIsNot` | `cfa8b2e` |
| Scroll keys do not fire while typing | `TestScrollKeysDoNotFireWhileTyping` | The wizard owns the keyboard. | `go test ./internal/tui/ -run ScrollKeysDoNotFire` | `cfa8b2e` |
| A jump key does not fire during a decision | `TestAJumpKeyDoesNotFireDuringADecision` | `s` and `p` do not leave a plan preview, repair, or running operation. | `go test ./internal/tui/ -run JumpKeyDoesNot` | `4b4f5a6` |
| …but still works where it should | `TestAJumpKeyStillWorksWhereItShould` | Guards the over-correction. | `go test ./internal/tui/ -run JumpKeyStillWorks` | `4b4f5a6` |
| Help does not strand the user | `TestHelpDoesNotStrandTheUser` | `?` on the help screen does not record help as the screen to return to. | `go test ./internal/tui/ -run HelpDoesNotStrand` | `4b4f5a6` |

## 5. Account lifecycle (findings 9, 10)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| An account can be removed | `TestAnAccountCanBeRemoved` (`internal/ipc/account_removal_test.go`) | The route exists and reaches the handler. | `go test ./internal/ipc/ -run AnAccountCanBeRemoved` | `115f058` |
| A refusal names the dependents | `TestARefusalNamesTheConnectionsThatDependOnTheAccount`, `TestARefusalNamesWhatMustChangeFirst` | Which connections are in the way, not just how many. | `go test ./internal/ipc/ ./internal/tui/ -run ARefusalNames` | `115f058` |
| Removal requires DELETE | `TestRemovalRequiresDelete` | GET/POST/PUT do not remove an account. | `go test ./internal/ipc/ -run RemovalRequiresDelete` | `115f058` |
| A malformed path is refused | `TestAMalformedAccountPathIsRefused` | The widened parser does not accept what it should not. | `go test ./internal/ipc/ -run MalformedAccountPath` | `115f058` |
| Configuring an account still works | `TestConfiguringAnAccountStillWorks` | The widened parser did not break the two-segment routes. | `go test ./internal/ipc/ -run ConfiguringAnAccountStill` | `115f058` |
| An account is selectable and removable | `TestAnAccountCanBeSelectedAndRemoved` (`internal/tui/accounts_test.go`) | A cursor exists, so there is something to act on. | `go test ./internal/tui/ -run AnAccountCanBeSelected` | `115f058` |
| An unverified account is selectable | `TestAnUnverifiedAccountCanBeRemoved` | The accounts most likely to need removing can be acted on. | `go test ./internal/tui/ -run AnUnverifiedAccount` | `115f058` |
| A refusal is not re-sent | `TestARefusedRemovalIsNotResent` | Enter on a refusal does not repeat a request whose answer will not change. | `go test ./internal/tui/ -run ARefusedRemovalIsNot` | `115f058` |
| The cursor stays in range | `TestTheCursorStaysInsideTheList` | Removing the last account does not strand the cursor. | `go test ./internal/tui/ -run CursorStaysInside` | `115f058` |
| The action is discoverable | `TestTheProvidersScreenSaysHowToRemoveAnAccount` | An action nobody can find is not reachable. | `go test ./internal/tui/ -run ProvidersScreenSaysHow` | `115f058` |
| The CLI can remove an account | `TestRemoveAccountIsReachable` (`internal/cli/handler_test.go`) | Not TUI-only. | `go test ./internal/cli/ -run RemoveAccountIsReachable` | `14ed62e` |

## 6. Editing and copying (finding 11)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| A connection can be edited | `TestAConnectionCanBeEdited` (`internal/tui/edit_test.go`) | End to end: change a value, preview it, reach the plan screen. | `go test ./internal/tui/ -run AConnectionCanBeEdited` | `d58736b` |
| An edit carries its revision | `TestAnEditCarriesTheRevisionItWasBuiltFrom` | The request states what it was built from. | `go test ./internal/tui/ -run AnEditCarriesThe` | `d58736b` |
| **A stale edit is refused** | `TestPlanEditRefusesAStaleRevision` (`internal/supervisor/recovery_test.go`) | The plan path honours the revision. Enforcement existed only on the PATCH path, which this flow never uses. | `go test ./internal/supervisor/ -run PlanEditRefusesAStale` | `d58736b` |
| Only changed fields are sent | `TestAnUntouchedFieldIsNotResubmitted` | An edit is not a full overwrite. | `go test ./internal/tui/ -run AnUntouchedField` | `d58736b` |
| **An edit does not rebuild the source** | `TestAnEditOfProtectionDoesNotRebuildTheSource` (`internal/supervisor/describe_spec_test.go`) | Changing protection does not destroy the origin scheme via a lossy DTO round-trip. | `go test ./internal/supervisor/ -run AnEditOfProtection` | `d58736b` |
| The detail reports the whole source | `TestTheDetailReportsTheWholeExistingSource` | Network and protocol are projected, not just the address. | `go test ./internal/supervisor/ -run TheDetailReportsTheWhole` | `d58736b` |
| The edit screen is kind-aware | `TestAForwardIsNotOfferedFieldsItCannotHave`, `TestATemporaryAddressIsNotPresentedAsEditable` | A forward is told it has no hostname; an assigned address is not offered as editable. | `go test ./internal/tui/ -run AForwardIsNotOffered && go test ./internal/tui/ -run ATemporaryAddressIsNot` | `d58736b` |
| A refused edit keeps its values | `TestARefusedEditStaysOnTheScreenThatMadeIt` | The values the supervisor objected to stay visible and editable. | `go test ./internal/tui/ -run ARefusedEditStays` | `d58736b` |
| Abandoning the preview returns to the edit | `TestAbandoningThePreviewReturnsToTheEdit` | Escape does not land on an empty screen with the changes gone. | `go test ./internal/tui/ -run AbandoningThePreview` | `d58736b` |
| An empty edit is not sent | `TestAnEditThatChangesNothingIsNotSent` | No request to plan a no-op. | `go test ./internal/tui/ -run AnEditThatChangesNothing` | `d58736b` |
| The edit screen takes typed text | `TestTheEditScreenTakesTypedText` | `p` typed into a field is text, not "preview". | `go test ./internal/tui/ -run TheEditScreenTakes` | `d58736b` |
| A connection can be copied | `TestAConnectionCanBeCopied` | End to end. | `go test ./internal/tui/ -run AConnectionCanBeCopied` | `d58736b` |
| **The copy is deep-copied, not rebuilt** | `TestTheCopyIsMadeBySupervisorNotRebuiltFromTheDetail` | Rebuilding from the detail DTO would drop a command's environment and a health check. | `go test ./internal/tui/ -run TheCopyIsMadeBySupervisor` | `d58736b` |
| **A protected connection can be copied** | `TestACopyOfAProtectedConnectionIsPossible` | The first version downgraded the address while copying the policy — a combination core validation rejects, making every protected connection uncopyable. | `go test ./internal/tui/ -run ACopyOfAProtectedConnection` | `d58736b` |
| The copy is asked for its own hostname | `TestACopyIsAskedForItsOwnHostname`, `TestATemporaryConnectionIsNotAskedForAHostname` | Two connections cannot share a hostname; the question is asked only when it applies. | `go test ./internal/tui/ -run ACopyIsAskedFor && go test ./internal/tui/ -run ATemporaryConnectionIsNot` | `d58736b` |
| A forward can be copied | `TestAForwardCanBeCopied` | Copying is not limited to published services. | `go test ./internal/tui/ -run AForwardCanBeCopied` | `d58736b` |
| The copy is created closed | `TestACopyIsCreatedClosed` | Copying an open connection does not start a second one. | `go test ./internal/tui/ -run ACopyIsCreatedClosed` | `d58736b` |
| Both actions are discoverable | `TestEditAndCopyAreDiscoverable` | Present in the key hints and in help. | `go test ./internal/tui/ -run EditAndCopyAreDiscoverable` | `d58736b` |

## 7. Live refresh, history and export (findings 24, 26, 27, 30)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| An open detail view refreshes | `TestAnOpenDetailViewRefreshesWhenItsConnectionChanges` (`internal/tui/refresh_test.go`) | A connection event refreshes the detail, not only the list. | `go test ./internal/tui/ -run OpenDetailView` | `14ed62e` |
| Refresh is scoped | `TestAnotherConnectionsEventDoesNotRefetchThisDetail`, `TestNoDetailIsFetchedWhenNoDetailIsOpen` | An event storm does not become a fetch storm. | `go test ./internal/tui/ -run AnotherConnectionsEvent && go test ./internal/tui/ -run NoDetailIsFetched` | `14ed62e` |
| A capped history says so | `TestACappedHistorySaysItIsCapped` | And offers the older ones. | `go test ./internal/tui/ -run CappedHistory` | `14ed62e` |
| A complete history says so | `TestACompleteHistorySaysItIsComplete` | The ambiguity is removed in both directions. | `go test ./internal/tui/ -run CompleteHistory` | `14ed62e` |
| Asking for more asks for more | `TestAskingForMoreAsksForMore`, `TestAskingForMoreDoesNothingWhenThereIsNoMore` | The key requests a larger page, and is inert when there is nothing older. | `go test ./internal/tui/ -run AskingForMore` | `14ed62e` |
| A past operation shows what happened | `TestAPastOperationShowsWhatHappened` | The journal, not just the steps: "the zone is not delegated" rather than "the DNS step failed". | `go test ./internal/tui/ -run PastOperationShows` | `e95e2a4` |
| An unreadable journal says so | `TestAnUnreadableJournalSaysSoRatherThanShowingNothing` | Distinguishable from an operation that recorded nothing. | `go test ./internal/tui/ -run UnreadableJournal` | `e95e2a4` |
| The support export is reachable | `TestSupportExportIsReachable` (`internal/cli/handler_test.go`) | `portico support export`, with a documented redaction claim. | `go test ./internal/cli/ -run SupportExportIsReachable` | `14ed62e` |
| The export carries no secrets | `TestSupportExportCarriesNoSecrets` (`internal/supervisor/recovery_test.go`) | Pre-existing; still passing. | `go test ./internal/supervisor/ -run SupportExportCarriesNoSecrets` | pre-existing |
| A command's environment is not shipped | `TestACommandsEnvironmentIsNotShippedInTheDetail` (`internal/supervisor/describe_spec_test.go`) | Operator-supplied env routinely holds tokens; the detail view is rendered, logged and exported. | `go test ./internal/supervisor/ -run ACommandsEnvironment` | `95b6f3c` |
| A tunnel secret is not returned | `TestATunnelSecretIsNeverReturnedToTheCaller` (`internal/tunnel/contract_test.go`) | The generated secret stays inside the manager. | `go test ./internal/tunnel/ -run TunnelSecretIsNever` | `a8a57c7` |

## 8. Release confidence (findings 37–42)

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| Tunnel creation is remotely managed | `TestCreatingATunnelSendsARemotelyManagedTunnel` (`internal/tunnel/contract_test.go`) | `config_src: cloudflare`, without which the tunnel ignores its ingress. | `go test ./internal/tunnel/ -run CreatingATunnel` | `a8a57c7` |
| **A deleted tunnel is an absence, not a failure** | `TestAMissingTunnelIsNotAFailure` | A tunnel deleted in the dashboard can be recreated instead of failing every repair on the lookup. **This test found a production bug on its first run.** | `go test ./internal/tunnel/ -run MissingTunnelIsNot` | `a8a57c7` |
| **A rejected token is not a missing tunnel** | `TestAnAuthenticationFailureIsReportedNotSwallowed` | Otherwise the repair path loops recreating what it cannot see. | `go test ./internal/tunnel/ -run AuthenticationFailure` | `a8a57c7` |
| Ingress carries origin and hostname | `TestConfiguringIngressSendsTheOriginAndHostname` | Including the catch-all rule Cloudflare requires. | `go test ./internal/tunnel/ -run ConfiguringIngress` | `a8a57c7` |
| A DNS record is proxied and points at the tunnel | `TestARecordIsCreatedProxiedAndPointedAtTheTunnel` (`internal/dns/contract_test.go`) | An unproxied CNAME does not route; the failure looks like a hostname that resolves but does not answer. | `go test ./internal/dns/ -run ARecordIsCreatedProxied` | `a8a57c7` |
| A missing record is an absence | `TestAMissingRecordIsAnAbsenceNotAFailure`, `TestARejectedTokenIsNotReportedAsAMissingRecord` | Same defect, same package, both directions. | `go test ./internal/dns/ -run MissingRecordIsAnAbsence && go test ./internal/dns/ -run RejectedTokenIsNot` | `a8a57c7` |
| Retargeting updates rather than replaces | `TestRetargetingUpdatesRatherThanReplaces` | No delete-and-recreate, which would drop the record and lose its durable identity. | `go test ./internal/dns/ -run RetargetingUpdates` | `a8a57c7` |
| The migration chain is well formed | `TestTheMigrationChainIsWellFormed` (`internal/store/migration_chain_test.go`) | Strictly increasing, contiguous, no duplicate version — a duplicate silently skips a migration. | `go test ./internal/store/ -run MigrationChainIsWellFormed` | `a8a57c7` |
| A fresh database applies everything once | `TestAFreshDatabaseAppliesEveryMigrationExactlyOnce` | The path every first-time user takes. | `go test ./internal/store/ -run FreshDatabaseApplies` | `a8a57c7` |
| Reopening applies nothing further | `TestReopeningADatabaseAppliesNothingFurther` | A data-transforming migration does not reapply on every start. | `go test ./internal/store/ -run ReopeningADatabase` | `a8a57c7` |
| A fresh database is usable | `TestAFreshDatabaseIsImmediatelyUsable` | A chain that applies cleanly and leaves an unusable schema is still a broken release. | `go test ./internal/store/ -run FreshDatabaseIsImmediately` | `a8a57c7` |
| The release gate is one command | — | `make validate` runs format, build, vet, staticcheck, tests and race tests. | `make validate` | `a8a57c7` |

---

## What is not claimed

Stating the limits, because a matrix that overstates its coverage is worse than
none.

- **The provider contract tests pin our half only.** They assert the requests
  Portico sends and how it handles the response shapes Cloudflare documents.
  They are served by a local fake. They are not evidence that Cloudflare behaves
  as modelled, and they would not catch an undocumented change in its API.
- **Traffic telemetry is not implemented.** The activity view says Portico does
  not collect it. That is an honest report of an unbuilt feature, not a
  completed requirement.
- **Private network connections are not implemented.** They are described
  correctly wherever they appear and refused at creation with a reason. No
  adapter can join or expose through one.
- **`AuthenticateProvider` and `UpdateConnection` have no client method.** Both
  are reachable over the transport. The TUI deliberately routes edits through
  plans instead of the PATCH path, so `UpdateConnection` is an API affordance
  rather than a gap.
- **The `-race` suite is run with `-count=1` locally and `-count=3` in CI.** No
  flaky test was observed across the runs in this remediation; one apparent
  failure was traced to a shell timeout cutting a run short, not to a test.
