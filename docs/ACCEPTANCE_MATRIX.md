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
`go test ./... -count=1` and `go test -race ./... -count=1`. Rows marked
`working tree` were added or corrected after the historical audit commits and
must be run from the current checkout.

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
| A late plan cannot reopen a dismissed screen | `TestLeavingThePreviewAbandonsThePlan` (`internal/tui/scroll_test.go`) | Both exits abandon the request, not just `esc`. | `go test ./internal/tui/ -run LeavingThePreviewAbandons` | `4b4f5a6` |
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
| Hidden content is announced | `TestContentTallerThanTheTerminalCanBeReached` | How much is above and below, and which keys move it — asserted through the real lifecycle. | `go test ./internal/tui/ -run ContentTallerThan` | `ed5450a` |
| The viewport never overflows | `TestPagingDownRepeatedlyStopsAtTheEnd` | Over-paging cannot exceed the terminal height or run into blank space. | `go test ./internal/tui/ -run PagingDownRepeatedly` | `ed5450a` |
| The whole content is reachable | `TestEndReachesTheBottomAndHomeReturns` | End reaches the last line and Home returns; the content is reachable, not merely some of it. | `go test ./internal/tui/ -run EndReachesTheBottom` | `ed5450a` |
| Content that fits is untouched | `TestShortContentIsNotClipped` | No indicator, no reserved line. | `go test ./internal/tui/ -run ShortContentIsNotClipped` | `cfa8b2e` |
| A screen opens at the top | `TestAScreenOpensAtTheTop` | One transition authority, so every navigation path is covered. | `go test ./internal/tui/ -run AScreenOpensAtTheTop` | `ed5450a` |
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

## 8a. Runtime and release corrections

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| A failed Access policy compensates the exact application | `TestAccessPolicyFailureCompensatesExactApplicationID` | The policy uses the application ID returned by creation, and failure removes that exact app. | `go test ./internal/controller/ -run AccessPolicyFailureCompensatesExactApplicationID` | working tree |
| Failed compensation remains actionable | `TestFailedAccessCompensationRetainsCleanupObligation` | If deletion fails, the cleanup obligation is durable rather than silently discarded. | `go test ./internal/controller/ -run FailedAccessCompensationRetainsCleanupObligation` | working tree |
| The OpenAI client uses the Portico gateway | `TestClientUsesGatewayAsMCPOrigin` | The client receives the gateway endpoint, not the raw MCP origin, and stopping closes the gateway. | `go test ./internal/provider/clienttunnel/ -run ClientUsesGatewayAsMCPOrigin` | working tree |
| Protected HTTP responses are reachable | `TestDefaultServiceCheckTreatsProtectedResponsesAsReachable` | 401/403 are evidence of a reachable protected service; unrelated 4xx/5xx remain failures. | `go test ./internal/core/ -run DefaultServiceCheckTreatsProtected` | working tree |
| MCP health performs a protocol probe | `TestMCPServiceCheckPerformsProtocolProbe` | MCP service health uses the transport's request shape rather than a generic GET. | `go test ./internal/core/ -run MCPServiceCheckPerformsProtocolProbe` | working tree |
| Client tunnels have a repair path | `TestAClientTunnelCanBeRepaired` | A crashed client tunnel yields a repair plan replaying the provider's open steps (start + verify ready) without inventing remote resource work. | `go test ./internal/controller/ -run AClientTunnelCanBeRepaired` | working tree |
| Installation-key rotation is operational | `TestRotateSecretKeyIsAnOperationalDurableAction` | Rotation is reachable through the supervisor and leaves an audit event without key material. | `go test ./internal/supervisor/ -run RotateSecretKeyIsAnOperationalDurableAction` | working tree |
| Release verification executes the archive | — | `verify_release_artifact.sh` extracts the produced archive, runs the binary under isolated XDG paths, starts `list`/`doctor`, verifies the database mode, and restarts from a copied install. | `make artifact-check` | working tree |

## 9. Second review: composition, mutation safety, and the real path

An independent review of the completed remediation found defects that unit
tests on each side could not see. These rows are the corrections.

| Requirement | Test | Behaviour pinned | Command | Commit |
|---|---|---|---|---|
| **Scrolling works through the real lifecycle** | `TestContentTallerThanTheTerminalCanBeReached` (`internal/tui/scroll_test.go`) | Size the window, open a screen, press Page Down, read the view. The previous tests set the height by hand — setup production cannot perform — and the feature did not work at all. | `go test ./internal/tui/ -run ContentTallerThan` | `ed5450a` |
| **Rendering does not change the program** | `TestViewIsPure` | Two identical renders produce identical output and leave the model unchanged, across five screens. Both the scroll state and the inspect model were being mutated from inside View. | `go test ./internal/tui/ -run ViewIsPure` | `ed5450a` |
| ctrl+d is not a scroll key | `TestCtrlDIsNotAScrollKey` | In a terminal it means end of input. | `go test ./internal/tui/ -run CtrlDIsNot` | `ed5450a` |
| A late operation refresh cannot replace the one on screen | `TestALateOperationRefreshDoesNotReplaceTheOneOnScreen` (`internal/tui/correlation_more_test.go`) | Watching one operation is not interrupted by a refresh started for another. | `go test ./internal/tui/ -run ALateOperationRefresh` | `b3146e1` |
| Leaving progress abandons its refresh | `TestLeavingProgressAbandonsItsRefresh` | A reply in flight cannot install an operation after the screen is gone. | `go test ./internal/tui/ -run LeavingProgressAbandons` | `b3146e1` |
| A copy cannot be submitted twice | `TestACopyCannotBeSubmittedTwice` | A second Enter does not create a second connection. | `go test ./internal/tui/ -run ACopyCannotBeSubmittedTwice` | `b3146e1` |
| A late copy reply cannot clear another copy | `TestALateCopyReplyDoesNotClearAnotherCopy` | Submit A, leave, begin B, A replies. | `go test ./internal/tui/ -run ALateCopyReply` | `b3146e1` |
| **An older snapshot cannot replace a newer one** | `TestAnOlderSnapshotDoesNotReplaceANewerOne` | Otherwise stale state sits beside a newer event cursor, and the corrections have already been skipped. | `go test ./internal/tui/ -run AnOlderSnapshot` | `b3146e1` |
| A smaller history page cannot undo "show more" | `TestASmallerHistoryPageDoesNotReplaceALargerOne` | Ordered by request token. | `go test ./internal/tui/ -run ASmallerHistoryPage` | `b3146e1` |
| Applying carries an idempotency key | `TestApplyingAPlanCarriesAnIdempotencyKey` | The machinery existed end to end and nothing used it. | `go test ./internal/tui/ -run IdempotencyKey` | `b3146e1` |
| A retry reuses the same key | `TestRetryingAnApplyReusesTheSameKey`, `TestANewPreviewIsANewAttempt` | One key per approved preview; a different plan is a different attempt. | `go test ./internal/tui/ -run RetryingAnApply && go test ./internal/tui/ -run ANewPreviewIsANewAttempt` | `b3146e1` |
| **A timed-out apply is not reported as failed** | `TestATimedOutApplyIsNotReportedAsFailed` | The supervisor continues after the client's context is cancelled, so the outcome is unknown, not failed. | `go test ./internal/tui/ -run ATimedOutApply` | `b3146e1` |
| **Account removal decides and deletes together** | `TestRemovingAnAccountDecidesAndDeletesTogether` (`internal/store/migration_chain_test.go`) | One write transaction, so a connection bound in between cannot be stranded. | `go test ./internal/store/ -run RemovingAnAccountDecides` | `eff55c9` |
| **A cleanup obligation blocks removing its account** | `TestACleanupObligationBlocksRemovingTheAccountThatCanDischargeIt` | Otherwise the only credential able to remove a created resource is taken away. | `go test ./internal/store/ -run ACleanupObligationBlocks` | `eff55c9` |
| **A refusal reaches the screen with names, through the real transport** | `TestAccountRemovalRefusalReachesTheScreenWithNames` (`internal/tui/vertical_test.go`) | Real server, real socket, real client, real model. Both sides' unit tests passed while this path was broken. | `go test ./internal/tui/ -run AccountRemovalRefusalReaches` | `eff55c9` |
| Turning protection on asks who can sign in | `TestTurningProtectionOnAsksWhoCanSignIn` (`internal/tui/edit_test.go`) | A policy naming nobody is refused where it is typed, not after a round trip. | `go test ./internal/tui/ -run TurningProtectionOn` | `9e50e7b` |
| Identities are parsed by the wizard's own parser | `TestNamedIdentitiesReachTheRequest`, `TestAMalformedIdentityIsRefusedWhereItIsTyped` | One parser, so the screen and core cannot disagree about a valid identity. | `go test ./internal/tui/ -run NamedIdentitiesReach && go test ./internal/tui/ -run AMalformedIdentity` | `9e50e7b` |
| The account is chosen, not typed | `TestTheAccountIsChosenNotTyped` | Only accounts the provider reports as usable. | `go test ./internal/tui/ -run TheAccountIsChosen` | `9e50e7b` |
| The CLI does not require a zone the provider calls optional | `TestProviderLoginDoesNotRequireAZone` (`internal/cli/handler_test.go`) | One provider contract, declared by the provider, read by both interfaces. | `go test ./internal/cli/ -run ProviderLoginDoesNotRequireAZone` | `67bfc49` |
| **A secret is never taken from an argument** | `TestASecretIsNeverTakenFromAnArgument` | Arguments are in shell history and the process list. | `go test ./internal/cli/ -run ASecretIsNeverTaken` | `67bfc49` |
| Adding an account uses the selected provider | `TestAddingAnAccountUsesTheSelectedProvider` (`internal/tui/accounts_test.go`) | No Cloudflare default in a provider-neutral mechanism. | `go test ./internal/tui/ -run AddingAnAccountUsesTheSelected` | `67bfc49` |
| The selected account is visible | `TestTheSelectedAccountIsVisible` | The screen offered "↑↓ select account" and drew no marker. | `go test ./internal/tui/ -run TheSelectedAccountIsVisible` | `67bfc49` |
| A no-op plan says which intent it answers | `TestANoOpPlanSaysWhichIntentItAnswers` (`internal/tui/refresh_test.go`) | Every no-op said "No repair needed" and pushed the repair screen. | `go test ./internal/tui/ -run ANoOpPlanSays` | `be5f524` |
| The route names what carries the traffic | `TestTheRouteStripNamesWhatCarriesTheTraffic` | A forward has no provider gateway. | `go test ./internal/tui/ -run TheRouteStripNames` | `be5f524` |
| **Origin ownership says what closing stops** | `TestOriginOwnershipSaysWhatClosingDoes`, `TestAForwardSaysOnlyForwardingStops` (`internal/supervisor/describe_spec_test.go`) | Whether Portico started the local service, or connected to one already running. | `go test ./internal/supervisor/ -run OriginOwnershipSays && go test ./internal/supervisor/ -run AForwardSaysOnly` | `be5f524` |
| The Access contract is pinned | `TestCreatingAnAppSendsTheHostname`, `TestThePolicyCarriesTheAllowedIdentities`, `TestAProtectedPolicyIsNeverCreatedWithoutIdentities` (`internal/access/contract_test.go`) | The application and its allow policy are separate requests, with fail-closed identity validation before policy creation. | `go test ./internal/access/ -run CreatingAnAppSends && go test ./internal/access/ -run PolicyCarries && go test ./internal/access/ -run ProtectedPolicyIsNever` | `959ec52` |
| A missing Access resource is an absence | `TestAMissingApplicationIsAnAbsenceNotAFailure`, `TestAMissingPolicyIsAnAbsenceNotAFailure`, `TestARejectedTokenIsNotAMissingApplication` | Same rule as the tunnel and DNS managers, both directions. | `go test ./internal/access/ -run AMissingApplicationIs && go test ./internal/access/ -run AMissingPolicyIs && go test ./internal/access/ -run ARejectedTokenIsNotAMissingApp` | `959ec52` |
| **Overwriting a permissive file still ends private** | `TestOverwritingAPermissiveFileStillEndsPrivate` (`internal/cli/handler_test.go`) | os.WriteFile's mode applies only on create; the code claimed 0600 and produced 0644. Verified empirically before fixing. | `go test ./internal/cli/ -run OverwritingAPermissiveFile` | `959ec52` |
| An existing report is not silently replaced | `TestAnExistingReportIsNotSilentlyReplaced`, `TestAFailedWriteLeavesNoPartialReport` | --force required; no half-report left behind. | `go test ./internal/cli/ -run AnExistingReportIsNot && go test ./internal/cli/ -run AFailedWriteLeaves` | `959ec52` |
| The report is reachable from the interface | `TestTheSupportReportIsReachableFromTheInterface` (`internal/tui/refresh_test.go`) | A CLI-only report is not where a stuck user looks. | `go test ./internal/tui/ -run TheSupportReportIsReachable` | pending |
| **Provider validation details reach the screen** | `TestProviderValidationDetailsReachTheScreen` (`internal/tui/vertical_test.go`) | Real server, real socket, real client, real model. The same defect as account removal: a rejection is a non-2xx, so the client returns an error with a nil response, and the screen was reading the response. Verified to fail against the previous code. | `go test ./internal/tui/ -run ProviderValidationDetailsReach` | `c3eaa5e` |
| A rejected credential is not retained | `TestARejectedCredentialIsNotRetained` | It does not stay in memory while the user retypes it, and does not appear in the error text. | `go test ./internal/tui/ -run ARejectedCredentialIsNotRetained` | `c3eaa5e` |
| **An edit plan can actually be applied** | `TestAnEditPlanCanActuallyBeApplied` (`internal/controller/edit_test.go`) | ApplyPlan's intent dispatch omitted IntentEdit, so every edit plan returned "unknown plan intent: edit". The whole feature could be planned, previewed, approved and never applied — and every test covering it asserted on the plan without applying one. | `go test ./internal/controller/ -run TestAnEditPlanCanActuallyBeApplied` | `77b66a7` |
| A removal without a preview is refused | `TestARemovalWithoutAPreviewIsRefused` (`internal/store/migration_chain_test.go`) | The fingerprint is required, not optional: treating an empty one as "skip the check" makes every forgetful caller silently unchecked. | `go test ./internal/store/ -run ARemovalWithoutAPreview` | `885f447` |
| **A preview binds the apply** | `TestARemovalPreviewedBeforeTheCredentialChangedIsRefusedAsStale` | An account removed and re-added under the same key is a different subject; the old preview no longer authorises removing it. | `go test ./internal/store/ -run ARemovalPreviewedBeforeTheCredential` | `885f447` |
| A dependency appearing after the preview blocks it | `TestARemovalPreviewedBeforeADependencyAppearedIsRefused` | Dependencies are reported before staleness: what blocks it is more use than the fact a preview aged. | `go test ./internal/store/ -run ARemovalPreviewedBeforeADependency` | `885f447` |
| **A removal is recorded, a refusal is not** | `TestASuccessfulRemovalIsRecorded`, `TestARefusedRemovalIsNotRecorded` | The record is written in the transaction that deletes, so it cannot claim a removal that did not happen. | `go test ./internal/store/ -run ASuccessfulRemovalIsRecorded && go test ./internal/store/ -run ARefusedRemovalIsNotRecorded` | `885f447` |
| **One dependency query serves preview and apply** | `TestThePreviewAndTheRefusalAgree` | Two would drift, and the drift would be a preview describing a removal that does something else. | `go test ./internal/store/ -run ThePreviewAndTheRefusalAgree` | `885f447` |
| No client invents what a removal does | `TestTheRemovalScreenShowsWhatTheSupervisorSaid` (`internal/tui/vertical_test.go`) | The screen promised to forget a credential Portico might not hold; the sentence now comes from the supervisor. | `go test ./internal/tui/ -run TheRemovalScreenShowsWhat` | `885f447` |
| The removal carries the preview's fingerprint | `TestARemovalCarriesThePreviewsFingerprint`, `TestAnUnremovableAccountSendsNoRemoval` | What was confirmed is what is applied, and nothing is sent that the preview said could not be. | `go test ./internal/tui/ -run ARemovalCarriesThePreviews && go test ./internal/tui/ -run AnUnremovableAccountSendsNo` | `885f447` |
| Only one fourth path segment is accepted | `TestTheRemovalPreviewIsReachable`, `TestOnlyTheRemovalPreviewIsAcceptedAsAFourthSegment` | The parser widened by one literal; every other four-segment path is still refused. | `go test ./internal/ipc/ -run TheRemovalPreviewIsReachable && go test ./internal/ipc/ -run OnlyTheRemovalPreviewIsAccepted` | `885f447` |
| **The matrix is verified, not trusted** | — | `make acceptance` runs every cited command and checks every cited test name exists. Its first run found four renamed tests and a wrong exit code in itself. | `make acceptance` | pending |


---

## How this matrix is verified

`make acceptance` runs every command in the tables above and checks that every
test name they cite exists. It fails if a row points at nothing.

This is not decoration. The first draft of this document cited ten commands
whose `-run` patterns matched no test — pipes escaped for the markdown table
became literal `\|` in Go's regexp — and later cited four tests that had been
renamed. Those rows read as authoritative and verified nothing. The checker's
own first version reported success while listing eighteen failures, because a
counter was incremented in what turned out to be a subshell; it now counts from
the report it writes.

The check runs in CI and uploads its report, which records the commit and the Go
version it ran against.

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
- **Private-network lifecycle coverage is narrower than service exposure.**
  Tailscale and client-mediated private connections are implemented and
  described as membership-based, but not every private-network provider or
  reconciliation path is claimed by this matrix.
- **`AuthenticateProvider` and `UpdateConnection` have no client method.** Both
  are reachable over the transport. The TUI deliberately routes edits through
  plans instead of the PATCH path, so `UpdateConnection` is an API affordance
  rather than a gap.
- **The `-race` suite is run with `-count=1` locally and `-count=3` in CI.**
  One flaky test was found and fixed: `TestController_ConcurrentOperationLimit`
  asserted that exceeding the concurrency limit is refused, while its guard only
  skipped when *no* operation was still running. A run where some had finished
  left a free slot, so the next apply was correctly accepted and the test
  correctly failed. It surfaced under load — two race suites competing — which
  is the condition a developer machine does not reproduce. An earlier version of
  this document claimed no flaky test had been observed; that claim was made
  before anything had looked hard enough.
- **Account removal is deliberately not a plan**, and that is a decision rather
  than an omission. An architect pass declined the machinery: account creation
  writes the same durable credential state with no plan, so if removal violated
  the invariant, creation would violate it equally. The plan boundary governs
  mutations with provider-visible state, ordered steps that can stop partway,
  and therefore compensation — removal has none of the three. What a preview
  actually buys is implemented instead: the preview is computed from the same
  evidence the removal decides on, a fingerprint binds preview to apply, and the
  removal is recorded in the transaction that deletes. The reasoning is in
  `ACCOUNT_REMOVAL_DESIGN.md` so the next non-connection mutation does not have
  to re-litigate it.
- **Vertical coverage is partial.** There are now real-server, real-socket,
  real-model tests for account-removal refusals and for provider validation
  details — the two places this defect was found — and the package exposes a
  no-op handler to make more of them cheap. Plan apply and the event stream are
  still proven on each side separately. Two instances of one defect were found
  by crossing the transport, so the remaining uncrossed paths should be read as
  unverified rather than as working.
