# Changelog

All notable changes to **libs** (shared Go libraries for the spore.host tools)
are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **`lifecycle`: `Observe` takes over a closing sentinel a closer that died left** (libs#88). A `CloseTableIfEmpty` that died between writing its sentinel and deleting the table (or dropping the sentinel) used to make every `Observe` in that table return `ErrTableClosing` for good, so a backstop never observed again and no teardown there could start. `Observe` now removes a stale sentinel, as `MarkActive`, `Mark` and `SetIdleIfUnchanged` do, and observes again at once. One that is back is removed once more and observed past after one `Options.PollInterval`; a third is `ErrTableClosing`. So a sentinel adds at most one `PollInterval` to an `Observe`. A live closer's sentinel is left alone, and `Observe` returns `ErrTableClosing` at once, with no write and no back-off; so does a clear that finds a live closer's sentinel has replaced the stale one (`SetIdleIfUnchanged` too, and `MarkActive` then waits a poll for it rather than retrying at once). Callers that cleared a stale sentinel themselves with `CloseTableIfEmpty` on `ErrTableClosing` no longer need to.
- **`lifecycle`: `CloseTableIfEmpty` re-checks its own sentinel just before it deletes the table.** A closer that stalled for longer than `StaleAfter` between its scan and `DeleteTable` could not tell that a writer had taken its sentinel for a dead closer's and written an item since, and deleted the table with that item in it. Now, right before `DeleteTable`, it reads its sentinel again (strongly consistent) and deletes only if the sentinel is still its own (the same token and `since`) and less than `StaleAfter/2` has passed since it wrote it. Otherwise it reports false: a sentinel taken over or replaced is left alone, and a closer that was too slow drops its own. DynamoDB has no conditional `DeleteTable`, so a window remains, from that read to the `DeleteTable` reaching DynamoDB. In it the sentinel is at least `StaleAfter/2` from going stale, so a writer takes it over only if the closer stalls that long there, or if the clocks are that far apart.
- **`lifecycle`: a closing sentinel with no usable time is stale.** A sentinel whose `since` is missing, is not a string, or is not a time (it sorts outside the window from now−`StaleAfter` to now+`StaleAfter`) used to block every writer for good, and `CloseTableIfEmpty` could not replace it either. No live closer writes one, so `Observe`, `MarkActive`, `Mark`, `AddHold`, `SetIdleIfUnchanged` and `CloseTableIfEmpty` now all take it over. The Go side and the conditions apply one rule to the stored string.

## [0.56.0] - 2026-11-25

### Added

- **`lifecycle`: readers judge, writers never idle** (lagotto#340). `InUse(item, now, markBound)` and `IdleSince(item, now, markBound)` (and the same on a `Client`, with its clock and `StaleAfter`) decide from what an item holds and its times whether it is in use and since when it has been idle. For new readers they are authoritative; the state field stays for older ones.
  - In use: a live lease; a hold; a creator mark younger than `markBound` (or with no time); a `MarkActive` younger than `markBound`; or ACTIVE from a write that recorded no times (an older client's: its state or epoch is not what this package last stamped).
  - Idle since: the latest of `LastActivityAt`, each mark's time plus `markBound`, the last `MarkActive` plus `markBound`, and a lease's heartbeat plus `StaleAfter`; and the stored `idleSince` when an older client wrote the state last or the item has none of those times. An item a writer could not finish with (a failed `MarkDone`, a lost reply, a creator that died) ages out by itself.
  - New attributes: `lastActivityAt`, `pulseAt` (the last `MarkActive`), `stampState` and `stampEpoch` (the state and epoch this package's last state write left), and the lease's `client`. `Options.MarkBound` (default 1 h) is how long an idle `Observe` takes a creator mark for a running creator.
- **Conditional writes**, for a backstop or actor that must act on an item only as it judged it. `Expect` (and `ExpectOf(item, exists)`) is an item's epoch, state, lease token and `lastActivityAt`, as read. `lastActivityAt` is the one change detector: every write that changes what keeps an item in use stamps it, and every conditional idle, claim or lock (an idle `Observe`, `Claim`, `SetIdleIfUnchanged`, `ClaimIfUnchanged`, `LockIfUnchanged`) requires it unchanged.
  - `SetIdleIfUnchanged` records a tool idle, or creates the item IDLE along with its table, only if the item is still exactly that. It never removes a live lease. When the write applied but reading it back failed, the error wraps `ErrApplied`.
  - `ClaimIfUnchanged` claims only then: an IDLE item past its grace (with no lease or a stale one), or a TEARDOWN item whose lease went stale. A caller that judged with `IdleSince` passes grace 0.
  - Both refuse (`ErrChanged`, or a `*NotClaimableError` saying "changed since it was judged") when a creator marked, a creator finished, a hold appeared or another claim landed since the read.
  - `LockIfUnchanged` takes a lease without changing the item's state, a short exclusion for a backstop's maintenance; `Lease.Unlock` gives it back.
- **Creator marks.** `Mark` adds a creator mark (`Item.Marks`, stored as a map `marks: {token: time}`, one per running creator) and returns its token, also with an error, so a mark that committed though its answer was lost can still be ended. `MarkDone(token)` removes that mark (one write, no read), stamps `lastActivityAt`, and changes nothing else. `Item.ActiveAt` is the time of the last activation.
  - **`RefreshMark(token)`** re-stamps a mark's time, only while it is still there and no live lease is on the item. A creator that may run longer than its readers' mark bound (`Options.MarkBound`) must refresh well within it (a third, say). It reports false when the mark is gone or a teardown holds the item. One write, no read, so a CLI user's grant needs no `GetItem`.
  - A mark with no readable time is aged from the item's `ActiveAt` or `lastActivityAt`, whichever is later, and is dead outright when the item has neither, so an idle `Observe` removes it once past the bound.
- **`Lease.Suspend`** gives a lease up after a teardown stopped partway. The item goes IDLE, marked `Item.Incomplete`, with no lease, so no creator waits for it, and the next backstop resumes it.
- **`AdoptLease`** returns the live lease this very `Client` took (its owner name and its own random id both match), for a claim whose answer was lost, so the caller can give it back. A `Client` with no owner (`"unknown"`) adopts nothing.
- `MarkDone` and `SetIdleIfUnchanged` retry a throttle or a conflicting transaction.
- `lastActivityAt` is set with each writer's own clock. Writers' clocks are assumed to agree within a few minutes (NTP, on Lambda and CLI hosts); grace and mark bounds are tens of minutes or more, so the skew is immaterial.
- **Leases have a kind.** `Lease.Unlock` refuses a claim's lease, and `Release`, `Suspend` and `Finish` refuse a lock's (`ErrNotALock`); `RefreshMark` goes through a lock, and stops only at a teardown's claim. A lock's lease records `kind: lock`, which `AdoptLease` reads. `LockIfUnchanged` refuses a TEARDOWN item.
- `ErrApplied` wraps the read-back error as well.

### Changed

- **A lease with no heartbeat, or one that is not a string, is stale** (`noLiveLease` is `NOT (heartbeat >= :stale)`), by the same rule as `LeaseStale` and `InUse`, which compare the stored strings. A writer's clock is used at the stored precision (milliseconds).
- **`Claim` and `ClaimIfUnchanged` refuse an item a creator's mark younger than `Options.MarkBound` is on**, and `SetIdleIfUnchanged` one with such a mark or a fresh `MarkActive` pulse (`ErrInUse`, which it also returns for an item still as judged but held or leased).
- **`RemoveHold` of a hold that is not there writes nothing.**
- An active `Observe` whose only change would be a stamp under a minute old writes nothing.
- A mark with no readable time is aged from the item's `ActiveAt` only.
- `InUse` and `IdleSince` with no bound use `DefaultMarkBound` (a `Client`'s methods, its own `MarkBound`); only the `Client` methods use the Client's `StaleAfter`, as its writes do.
- **`MarkActive`** stamps `pulseAt` and `lastActivityAt`, and clears a suspended teardown's `Incomplete` mark.
- **`RemoveHold`** removes the hold and stamps `lastActivityAt`, nothing else. Like `MarkDone`, it succeeds while the table is closing.
- **`Observe`**: an idle `Observe` records nothing while a creator mark younger than `Options.MarkBound` is on the item (a creator it cannot see), and clears older marks along with ACTIVE. An active one stamps `lastActivityAt` (unless the stamp is under a minute old and nothing else would change), and clears `Incomplete`.
- **`Lease.Release`** stamps `lastActivityAt` and clears `Incomplete`. **`Claim`** clears `Incomplete`.
- Every write that sets the state stamps `stampState` and `stampEpoch`.

### Fixed

- `SetIdleIfUnchanged` clears a closing sentinel a closer that died left, as an activation does, and every retry loop counts each retry toward its bound and backs off.
- A `MarkDone` or `RefreshMark` retried after a write that had applied reports it done (a refresh not when a claim now holds the item); an empty token is no request.
- `IdleSince` skips zero times (a lease with no heartbeat no longer ages from year 1).
- Every read of `Options.Rand` is serialised (the Client id is made outside the Client's lock), so a Rand that is not safe for concurrent use is safe here.

## [0.55.0] - 2026-11-20

### Added

- **`lastmile`: the lazy CLI last mile, in report mode** (libs#78, part of leave-no-trace spore-host/spore-host#752). A tool calls `lastmile.Run` after its main command. Run reports what it would do and changes nothing: no `Deleter.Delete` call and no lifecycle write (it never calls `Claim` or `Observe`). Every would-action goes to `Result.WouldDo` and to `Options.Logf`, and `Result.Summary()` gives one line, for example `lastmile REPORT tool=lagotto account=… would=3`. It reports:
  - teardowns whose lease went stale (heartbeat over 20 min old), with what their plan would still delete;
  - the tool's `Options.Residue` (for example lagotto's shared invoke role), once no lifecycle item of the tool remains in any scope;
  - empty `spore-host-lifecycle` tables;
  - for a tool that sets `FullVisibility` (spawn with no backstop), an idle teardown past the 24 h grace. A failed activity read counts as active. Without `FullVisibility` the idle check is skipped, and the note says why.
- `lastmile` runs at most once per 6 h per tool, account and scope. The state is kept in `~/.spore-host/lifecycle.json`, written atomically with mode 0600, and a corrupt file is reset. Each run has a 30 s budget. A scope the budget cuts off is not stamped, so the next invocation resumes it. Run returns no error and recovers panics, including panics in the tool's callbacks, so every problem ends up in `Result.Warnings`. `SPORE_LASTMILE=off|report|reap` (`ModeFromEnv`) switches it, and the default is `report`. `reap` runs as report mode, with a warning, until libs#79.
- **`lifecycle`:** `Client.LeaseStale(item)` reports whether an item's lease is stale on the Client's clock. `Client.ListItems(ctx, region)` lists every tool item in a region's table, and whether the table exists, with a consistent scan and no write.

## [0.54.0] - 2026-11-17

No changes. This tag was cut by mistake before libs#84 had merged, so it is identical to 0.53.0. Its intended contents, `lastmile` and the read-only `lifecycle` helpers, ship in 0.55.0.

## [0.53.0] - 2026-11-16

### Added

- **`lifecycle`: the shared-account lease, so one user's teardown can't race another user's setup** (libs#77, part of leave-no-trace spore-host/spore-host#752). One DynamoDB table per region, `spore-host-lifecycle`, plus the same table in a control region for `global` scope. It is pay-per-request and tagged `managed-by=spore-host` and `component=lifecycle`, with no `role` tag: `role=service` is reserved for the live service, and backstop roles deny access to resources carrying it (lagotto#333). It holds one item per tool and scope: `state`, `epoch`, `idleSince`, a fenced `lease` and `holds`.
  - Creators call `MarkActive` before every create. It creates the table if needed, waiting out a `DELETING` one. It waits (10 s polls, up to 10 min) while a live teardown lease or the table's closing sentinel is in the way, then fails with `ErrWaitTimeout`. A lease whose heartbeat is over 20 min old is taken over.
  - Backstops call `Observe` each tick and `Claim` once the tool has been idle for the grace, then `Lease.Step` before each delete and `Lease.Finish`, or `Lease.Release` if their post-claim re-check finds activity. A lost lease fails with `ErrLeaseLost`. `CloseTableIfEmpty` deletes the table once no tool item is left.
  - `AddHold` and `RemoveHold` let one tool keep another active.
  - The clock, randomness, sleep, poll intervals and timeouts can be injected.
- **`lifecycle/lifecycletest`: an in-memory DynamoDB for unit tests.** It evaluates condition and update expressions with DynamoDB's semantics, applies `TransactWriteItems` atomically with per-item cancellation reasons, models `CREATING`/`DELETING` tables, and returns the SDK's own error types. Expressions it does not support are rejected, not guessed at. A `Hook` lets a test drive a deterministic interleaving. Tools can use it to test their footprint and teardown code without AWS.
- **`footprint`: the shared inventory schema and teardown executor** behind each tool's `footprint -o json` and idle teardown (libs#77).
  - `Report` and `Resource` are the JSON schema, version `1`, with snake_case fields and arrays never `null`. `Decode` rejects an unknown schema version, ownership or class.
  - `Deletable` is the delete guard. It requires `managed-by=<tool>` and ownership `tagged` (a `legacy-name` match is never deleted), a name matching one of the tool's `NamePattern`s, and class `litter`, `residue` or `active`. It refuses `role=service`, `spore-host:permanent=true`, the `spawn-instance-boundary` policy, and an unset or unknown class.
  - `NewPlan` orders resources dependents-first and reports a cycle as a `CycleError`.
  - `Execute` runs a plan in `report` or `reap` mode. Before each delete it re-reads the resource and re-applies the guard to its live tags, then heartbeats the teardown lease. Not-found counts as success. A failure skips what the failed resource depends on, and a lost lease stops the run.
- **`tags`:** `Permanent` (`spore-host:permanent`), `PermanentTrue` and `IsPermanent`, for resources cleanup must never remove. `ToolShared` (`spore-host`) is the `managed-by` value of resources that belong to the suite rather than to one tool; today that is only the lifecycle table.

## [0.52.0] - 2026-11-16

### Added

- **`tags`: the `spore-host:*` resource tags, shared by every tool** (libs#76, part of leave-no-trace spore-host/spore-host#752). Constants for the keys (`ManagedBy`, `Scope`, `Created`, `Schema`, `Owner`, `Role`, `Component`), the values `SchemaVersion` (`"1"`) and `RoleService`, and the tool names `ToolSpawn`, `ToolLagotto`, `ToolCalque`, `ToolTruffle` and `ToolMCP`. `For(tool, scope, now, owner)` returns the standard set, which SQS, Lambda and CloudWatch Logs take as-is. `IsManagedBy`, `IsService` and `CreatedAt` read tags back for cleanup. The package has no dependencies.
- **Per-service tag converters** `tags/ec2tags`, `tags/iamtags`, `tags/dynamodbtags`, `tags/snstags`, `tags/s3tags` and `tags/schedulertags`. Each has `FromMap`/`ToMap` and imports only that service's SDK `types` package. `ec2tags.Specifications` builds one `TagSpecification` per resource type. `s3tags.Tagging` builds the `PutBucketTagging` input and `s3tags.Query` the `PutObject` `Tagging` string.
- **`tags/tagtest`: unit tests fail when a create call goes out without `spore-host:managed-by`.** Install `tagtest.New(t, tags.ToolSpawn).Install(&cfg)` on the test's `aws.Config`. Every `Create*`/`Put*`/`Run*`/`Register*`/`Import*`/`Allocate*`/`CopyImage`/`CopySnapshot`/`RequestSpot*` call is then checked, before input validation and without stopping the call. Operations that cannot carry tags (`PutRolePolicy`, DynamoDB `PutItem`, Scheduler `CreateSchedule`, and others) are on a default allow-list, each with a reason, and callers can add entries. libs now requires the AWS SDK v2 modules for these services, at versions no newer than spawn, lagotto, calque, truffle or spore-host-mcp already use. A tool that imports only the existing libs packages does not compile them.
- `SECURITY.md`: report vulnerabilities privately through GitHub's private vulnerability reporting (the repository's Security tab), not public issues.

## [0.51.5] - 2026-11-11

### Fixed

- **docgen: a wrapped, indented help-text line is escaped as prose** (`escapeAnglesBlock`). A 4-space-indented line was always treated as an indented code block and left raw, but one that directly follows paragraph text is a lazy continuation of that paragraph (CommonMark: an indented code block cannot interrupt a paragraph). markdown-it renders it as prose, so a bare `<placeholder>` in it reached Vue as an unclosed HTML tag. lagotto's `list` help (`"lagotto status\n             <group>"`) broke the spore.host docs build this way and blocked the CLI-reference sync (spore-host#647). spawn's `notify` examples (`--workspace-id <TEAMS_TENANT_ID>`) were rendered as HTML the same way and lost their placeholders. An indented line now starts a code block only after a blank line or inside one.

### Security

- Build and test with Go 1.26.11 / 1.27.4: GO-2026-6859 (net/http: an HTTP/1 server crash from a `MaxBytesReader` header race), published 2026-11-11.

## [0.51.4] - 2026-11-01

### Fixed
- **The "A newer version … is available" notice no longer appears when stderr
  is not a terminal** (libs#69). `update.CheckAsync` documented a non-TTY
  suppression but never checked for one, so a batch harness or a script that
  captured stderr got the notice on every invocation. When stderr is redirected
  to a file or pipe, `CheckAsync` now yields nil at once and makes no request to
  the GitHub releases API and no cache read or write. `SPORE_NO_UPDATE_CHECK`
  (any value) and `CI` still suppress it the same way. `CheckNow`, used by
  explicit `version`/`status` checks, is unchanged: it ignores all three. Users
  get this when spawn and truffle bump their libs requirement.

## [0.51.3] - 2026-10-21

### Changed
- **spawn's help text describes it as ephemeral compute for AWS** in all six
  languages. `spawn.root.short` was "Launch AWS EC2 instances effortlessly" and
  the first line of `spawn.root.long` "Ephemeral AWS EC2 instance launcher";
  both now say "ephemeral compute for AWS", matching `spawn version` and the
  project's description.

### Security
- **Built with Go 1.26.10** for GO-2026-6809 (disclosed 2026-10-21), which
  makes `crypto/x509` distrust compromised intermediate certificates. The CI
  Go pin moves 1.26.9 → 1.26.10.

## [0.51.2] - 2026-10-20

### Security

- **`golang.org/x/text` v0.39.0 → v0.41.0** for CVE-2026-56851 (denial of
  service via an out-of-bounds slice in the Nickname profile). Trivy flagged it
  on main with no code change.

### Fixed

- **`truffle --help` and spawn's help examples use `truffle find`, not the
  deprecated `truffle search`** (spore-host/truffle#255).
  `truffle.root.long` (the root `truffle --help` examples), `spawn.root.long`
  and `spawn.launch.long` (the "from truffle" pipes) invoked
  `truffle search m7i.large`, `"m8g.*"` and `--regions`. They now use
  `truffle find` in en, es, ja and pt. de and fr carry no examples in these
  keys. `truffle.search.long`, the deprecated command's own help, is
  unchanged. Only translated text changed and the message IDs are the same,
  so callers need no code change. A new test fails if any catalog entry
  outside `truffle.search.*` runs `truffle search`.

## [0.51.1] - 2026-10-15

### Changed

- **The `spawn list` TTL column header now says it is time remaining**
  (#54). spawn now shows the time left before an instance's termination
  deadline in that column (`53m`, `none`, `expired`; spore-host/spawn#900)
  rather than the configured TTL, so a bare `TTL` header read as the
  configured value. `spawn.list.header.ttl` is now `TTL Left` (en),
  `Rest-TTL` (de), `TTL restante` (es), `TTL restant` (fr), `TTL Restante`
  (pt) and `残りTTL` (ja). Only the translated text changed; the message ID is
  the same, so callers need no code change. Anything matching the literal
  header text `TTL` in `spawn list` output must allow the new wording.

### Security

- **CI now builds and scans with Go 1.26.9** (was 1.26.8) for the Go
  vulnerabilities disclosed 2026-10-09: GO-2026-6605, 6607, 6608, 6610, 6611,
  6612, 6613 and 6617, across `net/http`, `crypto/tls`, `mime/multipart` and
  `net/textproto`. govulncheck reported this module as affected by 8 of them
  through reachable standard-library calls.
  No dependency change was needed — unlike the other repos in this sweep, `libs`
  does not require `golang.org/x/net` at all (confirmed with `go list -m all`),
  so the toolchain pin was the entire fix.

## [0.51.0] - 2026-10-07

### Added

- **405 real Graviton On-Demand rates across all 8 regions the table covers**
  (truffle#175). Until now `EC2Pricing` had **no** Graviton compute,
  general-purpose or memory family in *any* region — no `c6g`–`c9g`, no
  `m6g`–`m8g`, no `r6g`–`r9g` — so when the Price List API throttled, truffle's
  fail-closed rule refused any Graviton launch carrying a `--cost-limit`. Reported
  from a real eight-way fan-out across four Graviton generations; `c7g.2xlarge` in
  us-west-2 with `--cost-limit 0.15` was the launch that died.
  Rates fetched from the Price List API, not typed from a datasheet, and validated
  three ways before committing: the fetch pipeline reproduces four *pre-existing*
  table entries exactly, all 405 new rates scale linearly within their family, and
  a new test enforces that linearity permanently.
  `PricesAsOf` records when they were fetched, as a constant rather than a comment
  so a caller or a test can reason about the table's age.
- **`scripts/refresh-prices.sh`** re-derives the table from the Price List API —
  the only honest way to maintain a table that exists precisely because that API
  can be unavailable.

- **The changelog policy is now enforced in CI rather than by habit.** A PR that changes
  non-test Go source without touching `CHANGELOG.md` fails, and `changelog_test.go`
  checks `[Unreleased]` for duplicate group headings, unknown group names, entries
  outside a group, and releases missing a compare link.
  This policy has been suite-wide for a while but only `spawn` enforced it — where it
  immediately earned its keep, catching a duplicate `### Fixed` **four times in one
  session** and a PR that had merged with no entry at all (found only at the next
  release, against an empty `[Unreleased]`, with the entries reconstructed from the diff
  at tag time).
  `scripts/changelog-consolidate.py` (and `make changelog-fix` where there's a Makefile)
  merges duplicate groups mechanically, because two PRs each adding their own
  `### Fixed` is a routine conflict that merges cleanly for git and badly for the format
  — not a mistake worth hand-fixing each time.

### Changed

- **The table's doc comment no longer forbids what the table does.** It used to say
  this "must not be grown into" a pricing database because hand-maintained prices
  go stale invisibly. That argument lost, and the comment now records why: truffle
  is the suite's pricing authority, so a fallback that cannot price the
  architecture most of its users run is a defect in truffle rather than a caveat to
  document around. Checked against the live API, four of the pre-existing 2026-01
  entries were still exact nine months later — On-Demand rates move far less than
  the staleness objection assumed.
  The guard that replaces the prohibition is a consistency test rather than a
  policy: EC2 On-Demand scales linearly with size inside a family, so a mistyped
  digit breaks linearity and fails the build. That catches the error class that
  actually matters for a spend cap — a price that is *wrong* rather than merely old.

## [0.50.0] - 2026-09-30

### Changed
- **BREAKING — `pricing.GetEC2HourlyRate` now returns `(float64, error)`** and
  resolves prices by *exact* `(region, instanceType)` match only
  (spore-host/libs#29). It previously returned a bare `float64` and never failed,
  which meant callers could not distinguish a real rate from an invented one.
  Migration: handle the error. There is no replacement for the old guessing
  behaviour by design — if you need a rate for a type outside the static table,
  use truffle's pricer (live AWS Price List), which is the suite's pricing
  authority.
- **BREAKING — `pricing.EstimateSweepCost` takes a `RateResolver`**:
  `EstimateSweepCost(params, rate)`. Pass `nil` for the previous offline
  behaviour (now honest: `StaticRateResolver`, exact match or error), or inject a
  resolver backed by a live pricing source. libs cannot depend on truffle
  (truffle depends on libs), so the pricing authority is supplied by the caller.
- **`CostEstimate` reports what it could not price.** New `PricedRows`,
  `UnpricedRows` (`"type@region"`, deduplicated and sorted) and `Partial()`.
  Rows that cannot be priced are excluded from the totals and named, and both
  `Display()` and `DisplayCompact()` now label a partial total as a **floor**
  rather than presenting it as the cost of the whole sweep.

### Fixed
- **`pricing` no longer fabricates EC2 prices** (spore-host/libs#29). For any
  instance family absent from the static table — which is every GPU family newer
  than `p4d` — the old code fell through to "unknown family $0.10/vCPU-hr ×
  size multiplier" and returned it with `err == nil`. Measured against the AWS
  Price List: `g7e.4xlarge` was quoted at `$0.80` against a real `$3.9982` (5.0×
  low), `p5.4xlarge` at `$0.80` against `$6.88` (8.6× low), `g6e.12xlarge` at
  `$2.40` against `$10.49` (4.4× low), `p6-b200.48xlarge` at `$9.60` against
  `$113.93` (11.9× low), and `p5e.48xlarge` at `$9.60` — a type for which AWS
  publishes no on-demand price at all. An unknown *region* silently borrowed
  us-east-1's prices (`sa-east-1 c5.xlarge` → `$0.17`), and an unknown *size*
  silently used the `xlarge` multiplier. All of these are now errors.
  `estimatePriceByFamily` is removed, along with the tests that asserted its
  output.

### Documentation
- **`i18n`: `lagotto.watch.long` now documents comma-separated instance-type
  patterns** in all six locales — lagotto v0.56.0 added comma-list support
  (spore-host/lagotto#135) but its `watch --help` long text is sourced from this
  catalog, so the CLI help hadn't mentioned it. No key changes.

## [0.49.0] - 2026-09-15

### Added
- **`catalog`: built-in `openrefine` web app** (spore-host spawn#590). OpenRefine
  data-cleaning UI — a `kind: web` entry on `ghcr.io/spore-host/openrefine`, an
  image we build from the official upstream release (OpenRefine ships none) in
  the new `spore-host/app-images` repo; runs auth-less on 0.0.0.0:3333, gated by
  spored's :443 proxy token. Joins code-server + jupyter.

## [0.48.0] - 2026-09-15

### Added
- **`catalog`: built-in `jupyter` web app** (spore-host spawn#590). JupyterLab in
  the browser — a `kind: web` entry on the public `quay.io/jupyter/base-notebook`
  image, running token-less on 0.0.0.0 (access gated by spored's :443 proxy
  token) with `allow_origin`/`trust_xheaders` set for the reverse proxy. Args
  validated against the image. (OpenRefine skipped — no canonical public image.)

## [0.47.0] - 2026-09-15

### Added
- **`catalog`: built-in `code-server` web app** (spore-host spawn#590). VS Code in
  the browser — a `kind: web` entry on the public `codercom/code-server` image,
  running auth-less on 0.0.0.0 inside the container (access is gated by spored's
  :443 proxy token). First shipped web-UI catalog entry; real-instance validated.

## [0.46.0] - 2026-09-15

### Added
- **`catalog`: `AppEntry.Args`** (yaml `args`) — extra arguments appended to a
  containerized app's `docker run` (after the image), e.g. code-server's
  `--bind-addr 0.0.0.0:8080 --auth none`. Lets web apps bind all interfaces and
  run without their own auth (the spored :443 proxy gates access). spore-host
  spawn#590.

## [0.45.0] - 2026-09-14

### Added
- **`catalog`: app launch `kind`** (spore-host spawn#590/#591). `AppEntry` gains
  `KindRaw` (yaml `kind`) with `Kind()`/`UsesDCV()` helpers and constants
  `KindApplication`/`KindDesktop`/`KindWeb`. `application` (the default; `dcv: true`
  is a back-compat alias) is a single GUI app over a DCV session; `desktop` is a
  bare Linux desktop over DCV with no app; `web` is an app that serves its own web
  UI on `Port` (new field, + optional `HealthPath`, default `/`) with no DCV.
  `Validate()` recognizes the three kinds, requires `port` + a runnable
  (image/launch_command) for `web`, and exempts `desktop` from the
  image/recipe/launch_command requirement. Shipped a built-in `desktop` catalog
  entry. No consumer signature breaks (all additive).

## [0.44.0] - 2026-09-11

### Changed
- **`catalog`: a container app's `base_amis` is now an optional pin, not a
  requirement** (spore-host#286/#389). The launcher resolves the AWS-maintained
  GPU DLAMI (NVIDIA driver preinstalled, every region) via an SSM public
  parameter at launch and installs DCV at boot, so no owned/shared base AMI is
  needed. `Validate()` no longer errors on a container app without `base_amis`;
  set `base_amis` only to pin a custom pre-baked image. Removed the broken
  `gpu_base_amis` anchor (dangling/unshared/duplicated per-region IDs — the
  source of #389) from the shipped `catalog.yaml`; paraview/chimerax now carry
  no base-AMI table. The `AppEntry.BaseAMIs` field is unchanged (still an
  optional map), so no consumer signature breaks.

### Fixed
- **`catalog.Validate()` no longer flags a private image bound only in a local
  overlay.** It checked `List()` (the overlay-merged catalog) for the
  shipped-catalog-must-be-public rule (#392), so any machine with a
  `~/.spawn/catalog.yaml` rebinding an app to a private ECR image — the
  overlay's whole purpose — failed `Validate()` as if the SHIPPED catalog were
  broken, when only that machine's own (correctly private) local binding was.
  This broke `go test`/`make check` for any contributor with such an overlay,
  and spawn's `TestCatalogValid` calls `Validate()` directly, so it broke
  spawn's CI-equivalent too (spawn#489). The public-image check now runs
  against the embedded `catalog.yaml` only.

### Changed
- CI moved off the self-hosted orion runner fleet onto `ubuntu-latest`. The
  fleet (colima/Docker on orion.local) is being decommissioned org-wide; no
  behavior change to the tool.

### Security
- **Bumped `golang.org/x/text` v0.21.0 → v0.39.0 — CVE-2026-56852 (HIGH)** (#35).
  A `norm.Iter` can enter an infinite loop on certain input. libs only imports
  `x/text/language`, so the vulnerable symbol isn't called and `govulncheck` reports
  nothing — but the dependency is present and it is the kind of thing that becomes
  reachable one import later.

  Worth stating why it sat here so long: all three CLIs had already been forced to
  v0.39.0 by *their own* security scans. libs, which they all import, had no
  security workflow, so it was the one place nobody was looking — the most-shared
  module in the suite was the least-scanned.

- **Added a security workflow** (#35). libs was the only Go repo in the suite
  without one: govulncheck (pinned `@v1.3.0` — v1.4.0 panics on generics), gitleaks
  over full history, Trivy fs + config, and Semgrep SAST, on PR/push/weekly.

  Semgrep is also what flags mutable action tags, which is why libs was the last
  repo still using floating `@v6` refs until they were pinned above — the rule
  lives in a workflow this repo didn't have.

- **Pinned GitHub Actions to commit SHAs** instead of floating tags
  (`actions/checkout@v6` → `@d23441a… # v6.1.0`, `setup-go`, `codecov-action`).
  A tag is mutable: `@v6` means "whatever v6 points at when the job runs", so the
  code CI executes — with a checkout of this repo and a token — could change with
  no commit here. Not hypothetical: `actions/checkout@v6` had already moved from
  the v6.0.3-era commit the sibling repos pin to a later one, silently.

  A test now enforces the pinning (full 40-hex SHA plus a `# vX.Y.Z` comment, so
  bumps stay reviewable).

- **Added a Dependabot config so those pins actually get bumped.** Pinning to a
  SHA closes the mutable-tag hole but opens a staleness one: a SHA never moves,
  including past a security fix, and unlike `@v6` nothing updates it for you — so
  pinning is only safe if something bumps the pins. This repo had no Dependabot
  config and has no security workflow, so nothing did, and nothing would have said
  so. Weekly, grouped into one PR, with a 7-day cooldown so a freshly-published
  action tag isn't proposed the day it ships. Dependabot rewrites the `# vX.Y.Z`
  comment alongside the SHA, so the pins stay readable and the test keeps passing.

  The group pattern is `*`, not the umbrella repo's `actions/*`: this repo also
  uses `codecov/codecov-action`, which `actions/*` would leave out. A test asserts
  that every action in every workflow is matched by some pattern, so adding a
  third-party action without widening the pattern fails CI rather than quietly
  going unmanaged.

### Changed
- **CI now fails on unformatted code (#30).** Nothing did before: the workflow had
  no formatting step, and this repo has no Makefile, so there wasn't even a local
  `fmt` target anyone might run. Three files in `i18n/` sat unformatted on `main`.

  The gate reports drift instead of fixing it — offenders listed with a diff —
  because `gofmt -w` rewrites files and exits 0, so a check built on it could only
  ever report success. The three drifted files are formatted (struct-field and
  map-literal alignment only; verified whitespace-only with `git diff -w`).
  Internal to the repo; no API change.

## [0.43.2] - 2026-07-19

### Fixed
- **`docgen` now also neutralizes `{{ … }}` Vue mustaches** in prose and flag
  descriptions. VitePress evaluates `{{ config.X }}` as an interpolation
  expression and crashes page rendering; docgen now escapes the braces to HTML
  entities (outside code spans), completing the v0.43.1 angle-bracket fix.

## [0.43.1] - 2026-07-19

### Fixed
- **`docgen` now HTML-escapes `<`/`>` in prose and flag descriptions** so bare
  `<placeholder>` tokens (e.g. `<sweep-id>`, `shell: <cmd>`) don't break the
  VitePress build, which runs markdown through Vue's template compiler and reads
  `<foo>` as an unclosed HTML tag. Angles inside backtick code spans and fenced/
  indented code blocks are left literal, where they render correctly and aren't
  parsed as markup.

## [0.43.0] - 2026-07-19

### Added
- **New `docgen` package** — renders a cobra command tree into deterministic,
  timestamp-free markdown fragments (per-command flag tables + a global-flags
  file), honoring `Hidden` (skipped) and `Deprecated` (flagged). It's the shared
  engine behind each CLI's hidden `gen-docs` command and the docs drift gate, so
  the exhaustive command/flag reference is generated from code and can't silently
  go stale. Adds `github.com/spf13/cobra` as a direct dependency.

## [0.42.0] - 2026-07-17

### Added
- **New `sporeconfig` package** — the shared configuration base for the
  spore.host suite (spawn, truffle, lagotto, spore-host-mcp). `Resolve(Flags)`
  returns the common AWS `Profile`/`Region`/`Account` and default `Output` using
  a single precedence order — **flag > env > file > default** — so every tool
  resolves them the same way instead of each reinventing it. Reads `SPORE_*`
  (and `AWS_PROFILE`/`AWS_REGION`/`AWS_DEFAULT_REGION` as fallbacks) and the
  `[spore]` table of `~/.config/spore/config.toml` (XDG-aware via
  `ConfigDir`/`ConfigPath`). Deliberately **SDK-free**: it resolves strings only;
  each tool turns them into an `aws.Config` itself, and an unset value means
  "use the ambient AWS chain" so an unconfigured suite behaves exactly as before.
  A missing config file is not an error (opt-in); a malformed one is reported but
  the flag/env/default layers still resolve.

## [0.41.1] - 2026-06-28

### Fixed
- **catalog: overlay rebind now field-merges instead of replacing** (spore-host#392).
  An overlay entry that rebinds an existing app (e.g. supplying just an `image`)
  previously REPLACED the whole entry, blanking the app's description, GPU,
  families, etc. It now merges field-by-field: non-zero overlay fields override,
  unset fields inherit from the built-in definition. New apps in the overlay are
  still added as-is.

## [0.41.0] - 2026-06-28

### Added
- **catalog: `recipe` field — public recipe, private cake** (spore-host#392). An
  app can ship a public build-instructions pointer (`recipe:`, e.g.
  `infra/amis/containers/paraview`) without a bound image. Such a "recipe-only"
  entry (`AppEntry.RecipeOnly()`) is a buildable definition — anyone can bake the
  image and bind it via a local overlay or `--image`. `Validate()` accepts
  recipe-only entries as usable.

### Changed
- **catalog: paraview and chimerax are now recipe-only** (spore-host#392).
  Their image bindings were removed from the shipped catalog (they pointed at a
  personal account's public ECR). spore.host ships the recipe; the image is BYO —
  build it (`infra/amis/containers/<app>`) and bind it in `~/.spawn/catalog.yaml`
  or pass `--image`. `base_amis` stays so a bound image launches on the shared
  DCV base AMI.

- **catalog: online public-resolvability gate** (BYO-image model, spore-host#392).
  New `ResolvePublicImages()` anonymously HEAD-checks each shipped container
  image's manifest via the OCI registry v2 API (following the standard Bearer
  challenge — no Docker, no creds), rejecting any image that is private or not
  anonymously pullable. Wired into CI behind the `online` build tag (`go test
  -tags online ./catalog/`) so a dead/private/wrong-tag ref in the global catalog
  fails CI — the gap that let `chimerax:1.8` and dangling AMIs through before.

## [0.40.0] - 2026-06-27

### Added
- **catalog: image `visibility` (public/private) + inference** (BYO-image model,
  spore-host#392). New `AppEntry.Visibility` field and `ImageVisibility()`
  accessor: explicit value wins, else inferred from the registry
  (`public.ecr.aws/*` → public, private-ECR `*.dkr.ecr.*.amazonaws.com/*` →
  private, everything else → public). This underpins the per-account
  list/launch filter — public images surface for everyone, private images only
  for accounts that can pull them. `Validate()` now also rejects any non-public
  image in the shipped catalog (private images belong in a user's local overlay).
- **catalog: local overlay** (BYO-image model, spore-host#392). The embedded
  catalog can now be layered with a user overlay that adds apps or rebinds an
  existing app's image to one the user hosts (the only place private images
  belong). Path precedence: `SetOverlayPath` (e.g. a `--catalog` flag) >
  `$SPAWN_CATALOG` > `~/.spawn/catalog.yaml`; a missing default/env file is fine
  (opt-in), a malformed or explicitly-named-but-missing file is reported via the
  new `LoadError()` and falls back to embedded-only. New `SetOverlayPath`,
  `Reload`, and `LoadError`; overlay entries merge by name (overlay wins,
  case-insensitive) over the embedded baseline.

## [0.39.2] - 2026-06-27

### Changed
- **catalog: chimerax bumped to 1.12** (#290). 1.8 no longer exists on UCSF's
  download site; 1.12 is the current production release. The image must still be
  built/pushed (ChimeraX has a license-gated download — see
  infra/amis/containers/chimerax), so a chimerax launch isn't functional until
  that image is published.

## [0.39.1] - 2026-06-26

### Fixed
- **catalog: point app images at the real ECR Public registry** (#290). The
  paraview/chimerax `image:` prefixes are now `public.ecr.aws/f8g1e7l5/…` (the
  build account's default ECR Public alias) instead of the aspirational
  `public.ecr.aws/spore-host/…`, which does not resolve (a custom alias needs an
  async AWS approval). `paraview:5.13.2` is published and publicly pullable; the
  base AMI note now reflects the real owning account (942542972736).

## [0.39.0] - 2026-06-26

### Added
- **catalog: `Validate()` structural gate** (#290, #389). Returns one error per
  catalog defect with no AWS calls — every app is launchable (image or
  launch_command), no app reuses the deprecated per-app `amis` table, and each
  container app has a `tag_default` within `tags_available` plus a non-empty
  `base_amis`, with a unique image. Run in CI via the existing `go test ./...`
  (`TestValidate_EmbeddedCatalogClean`), so a #389-class bad entry can't merge.
  (ECR/AMI-visibility checks need AWS creds and live in a separate job.)

## [0.38.0] - 2026-06-26

### Added
- **catalog: container-based app model** (#290). `AppEntry` gains `Image`,
  `TagDefault`, `TagsAvailable`, and `BaseAMIs` (region → shared `spore-dcv-base`
  AMI). A containerized app runs `Image:tag` on the shared base AMI instead of a
  baked per-app AMI. New helpers: `AppEntry.ResolveTag(requested)` (validates a
  requested version against `TagsAvailable`, falling back to `TagDefault`) and
  `AppEntry.Containerized()`. paraview and chimerax are now container entries.

### Changed
- **catalog: an app is launchable via a container image OR a `launch_command`**
  (#290). GPU apps (paraview, chimerax) launch from their image CMD and no longer
  set `launch_command`; CPU apps keep it until they are containerized.

### Removed
- **catalog: deleted the per-app, per-region baked AMI tables** (#389). Every ID
  in them was found dangling or unshared from the launch account, and several were
  duplicated across apps (a paraview launch outside us-east-1 would have booted the
  chimerax image). The `amis` field remains on `AppEntry` for one release as a
  deprecated, must-be-empty escape hatch; new entries use `image` + `base_amis`.

## [0.37.1] - 2026-06-12

### Fixed
- i18n: removed stray `{{.Count}}`/`{{.Percent}}` template variables from eight
  `truffle.capacity.summary.*` labels in the es/fr/de/ja/pt translations. The
  truffle capacity command formats counts itself, so these strings supply only
  the label; the leftover variables made `i18n.T` (called with no template data)
  fall through to its error path and render `[truffle.capacity.summary.<key>]`
  in non-English locales. English was already corrected.

## [0.37.0] - 2026-06-12

### Added
- `update.CheckNow(tool, currentVersion) *Result` — a synchronous, ungated
  version check for explicit, user-initiated use (e.g. a `version` subcommand).
  Unlike `CheckAsync` it ignores the CI / `SPORE_NO_UPDATE_CHECK` / non-TTY
  suppressions and bypasses the 24h cache, so the caller always gets a fresh
  answer; returns nil when the GitHub releases API can't be reached.

## [0.36.0] - 2026-06-07

Latest tagged release. See the
[GitHub Releases](https://github.com/spore-host/libs/releases) for the contents
of this and earlier tags (`update`, `i18n`, `catalog`, `pricing` packages).

---

[Unreleased]: https://github.com/spore-host/libs/compare/v0.56.0...HEAD
[0.56.0]: https://github.com/spore-host/libs/compare/v0.55.0...v0.56.0
[0.55.0]: https://github.com/spore-host/libs/compare/v0.54.0...v0.55.0
[0.54.0]: https://github.com/spore-host/libs/compare/v0.53.0...v0.54.0
[0.53.0]: https://github.com/spore-host/libs/compare/v0.52.0...v0.53.0
[0.52.0]: https://github.com/spore-host/libs/compare/v0.51.5...v0.52.0
[0.51.5]: https://github.com/spore-host/libs/compare/v0.51.4...v0.51.5
[0.51.4]: https://github.com/spore-host/libs/compare/v0.51.3...v0.51.4
[0.51.3]: https://github.com/spore-host/libs/compare/v0.51.2...v0.51.3
[0.51.2]: https://github.com/spore-host/libs/compare/v0.51.1...v0.51.2
[0.51.1]: https://github.com/spore-host/libs/compare/v0.51.0...v0.51.1
[0.51.0]: https://github.com/spore-host/libs/compare/v0.50.0...v0.51.0
[0.50.0]: https://github.com/spore-host/libs/compare/v0.49.0...v0.50.0
[0.49.0]: https://github.com/spore-host/libs/compare/v0.48.0...v0.49.0
[0.48.0]: https://github.com/spore-host/libs/compare/v0.47.0...v0.48.0
[0.47.0]: https://github.com/spore-host/libs/compare/v0.46.0...v0.47.0
[0.46.0]: https://github.com/spore-host/libs/compare/v0.45.0...v0.46.0
[0.45.0]: https://github.com/spore-host/libs/compare/v0.44.0...v0.45.0
[0.44.0]: https://github.com/spore-host/libs/compare/v0.43.3...v0.44.0
[0.43.3]: https://github.com/spore-host/libs/compare/v0.43.2...v0.43.3
[0.43.2]: https://github.com/spore-host/libs/compare/v0.43.1...v0.43.2
[0.43.1]: https://github.com/spore-host/libs/compare/v0.43.0...v0.43.1
[0.43.0]: https://github.com/spore-host/libs/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/spore-host/libs/compare/v0.41.1...v0.42.0
[0.41.1]: https://github.com/spore-host/libs/compare/v0.41.0...v0.41.1
[0.41.0]: https://github.com/spore-host/libs/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/spore-host/libs/compare/v0.39.2...v0.40.0
[0.39.2]: https://github.com/spore-host/libs/compare/v0.39.1...v0.39.2
[0.39.1]: https://github.com/spore-host/libs/compare/v0.39.0...v0.39.1
[0.39.0]: https://github.com/spore-host/libs/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/spore-host/libs/compare/v0.37.1...v0.38.0
[0.37.1]: https://github.com/spore-host/libs/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/spore-host/libs/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/spore-host/libs/releases/tag/v0.36.0
