// Package lastmile is the lazy CLI last mile of leave-no-trace
// (spore-host/spore-host#752, libs#78): any spore.host CLI, after its main
// command, spends a few seconds checking whether something it created has been
// left behind, and resumes or removes it.
//
// The backstops (the lagotto poller, the spawn reaper, calque's reaper module)
// tear a tool down once it has been idle for the 24 h grace. Three things fall
// through them, and Run covers each:
//
//   - A teardown whose holder died. Its lifecycle lease goes stale once its
//     heartbeat is 20 min old (package lifecycle decides; see
//     lifecycle.Client.LeaseStale), and nothing resumes it until the next
//     backstop tick, if the backstop itself was not what died.
//   - Residue that outlives every scope's teardown: resources the tool shares
//     across scopes, such as lagotto's shared invoke role, and an empty
//     spore-host-lifecycle table. These may go only once no lifecycle item of
//     the tool remains in any scope. The tool lists them in Options.Residue.
//   - A tool with no backstop at all (spawn in an account without the
//     reaper). Run checks it for idleness itself, but only when the tool says
//     it can read every one of its activity signals (Options.FullVisibility);
//     a read that fails counts as activity.
//
// # Report mode only, for now
//
// This release only reports. Run computes what it would do, records each
// would-action in Result.WouldDo and logs it through Options.Logf, and changes
// nothing: it makes no Deleter.Delete call and no lifecycle write. In
// particular it never calls lifecycle.Client.Claim (which writes a lease) or
// Observe (which writes state and idleSince); it reads with
// lifecycle.Client.Get and ListItems, and plans with footprint.Execute in
// footprint.ModeReport, which calls only Deleter.Describe. ModeReap is
// accepted, so that SPORE_LASTMILE=reap is not an error, but runs as report
// mode with a warning until libs#79 flips it. Result.Did is always empty.
//
// Because report mode never calls Observe, it never starts the idle clock: a
// tool with no backstop reports "would claim" only once something else has
// recorded it idle for the grace. libs#79 has to settle two things report
// mode leaves open. lifecycle.Client.Claim refuses an item in state TEARDOWN,
// so a stale teardown cannot be claimed directly; it is returned to IDLE (with
// its first idleSince) by an idle Observe, which only a caller that sees every
// activity signal may make. And reap mode has to call Observe for a tool with
// no backstop, so it needs the same visibility.
//
// # Never in the way
//
// Run is background hygiene, never part of the user's command:
//
//   - It returns a Result, never an error, and recovers from panics,
//     including panics in the tool's own callbacks. Every problem becomes a
//     line in Result.Warnings.
//   - It runs at most once per Options.MinInterval (6 h) for each tool,
//     account and scope, so most invocations do nothing but read a small
//     local file.
//   - It stops after Options.Budget (30 s), even if a callback ignores its
//     context. A scope it did not finish is not stamped, so the next
//     invocation picks it up where this one stopped.
//
// # The state file
//
// The rate limit lives in ~/.spore-host/lifecycle.json (Options.StatePath),
// shared by every tool. It is written atomically (a temporary file in the
// same directory, synced, then renamed over the old one) with mode 0600, in a
// directory created 0700. A file that cannot be read or parsed, or carries an
// unknown version, is reset with a warning; losing it costs at most one extra
// run. The format:
//
//	{
//	  "version": 1,
//	  "runs": {
//	    "lagotto/123456789012/us-east-1": {
//	      "last_run": "2026-11-16T12:00:00Z",
//	      "last_attempt": "2026-11-16T12:00:00Z"
//	    },
//	    "lagotto/123456789012/global": {
//	      "last_attempt": "2026-11-16T12:00:30Z",
//	      "incomplete": "budget exceeded"
//	    }
//	  }
//	}
//
// A key is tool/account/scope, where scope is a region or "global". The tool
// is part of the key because each tool's run checks only that tool's items:
// spawn's run must not suppress lagotto's. last_run is when the scope was last
// checked to the end; a scope is due when it has none, or it is at least
// MinInterval old (or in the future, after a clock change). last_attempt is
// when a run last started on it, and incomplete says why that run did not
// finish.
//
// # Scopes
//
// The scopes are Options.Regions, plus "global" if the lifecycle Client has a
// ControlRegion. Each due scope is checked for a stale lease (and, with
// FullVisibility, for idleness), and the lifecycle table in its region is
// checked for emptiness. The residue is judged in the global scope, after
// reading the tool's item in every scope, due or not: one item anywhere keeps
// it. So the residue check needs a ControlRegion, and Regions must list every
// region the tool can have used, not just the one this command touched.
//
// # Integrating it (spawn, lagotto, calque)
//
// Call Run once per CLI invocation, after the main command has finished (or
// in a goroutine alongside it, waiting for it before exit), from the root
// command's post-run hook rather than from each subcommand. Skip it for
// commands that must stay silent or fast and for which a cleanup check makes
// no sense: completion, version, help, and the tool's own footprint and
// teardown commands. Gate it on SPORE_LASTMILE (EnvVar), read with
// ModeFromEnv: off disables it, report (the default, also used for an unset
// or unknown value) reports, and reap is reserved for libs#79. A tool may
// also expose the same switch in its config file through libs/sporeconfig;
// the env var wins.
//
// Pass:
//
//   - Tool, Account (from sts:GetCallerIdentity, which the command has
//     usually already made) and Regions: every region the tool has
//     resources in or is configured for, not only this command's region.
//   - Lifecycle: the tool's lifecycle.Client, with ControlRegion set.
//   - Footprint: the function behind `<tool> footprint -o json`, and its
//     Deleter and Patterns. Run uses them to say what a resumed teardown
//     would delete; Deleter.Describe is called, Delete never is.
//   - Residue: lagotto passes its shared invoke role (and anything else that
//     outlives its regional teardowns); spawn and calque pass what they share
//     across scopes, or nothing.
//   - FullVisibility and Activity: spawn sets them when it runs without a
//     backstop and can read every activity signal (instances, spot requests,
//     pools, sweeps); lagotto and calque, whose backstops own idleness, leave
//     FullVisibility false.
//   - Logf: the tool's debug or verbose logger. The default discards, so a
//     user sees nothing unless they ask.
//
// Then ignore the Result, or print Result.Summary() under --verbose. The call
// must never change the command's exit code or its stdout: never return an
// error because of it, never print to stdout, and if the main command failed,
// still exit with the main command's status. Something like:
//
//	func lastMile(ctx context.Context, cfg aws.Config, account string) {
//		mode := lastmile.ModeFromEnv()
//		if mode == lastmile.ModeOff {
//			return
//		}
//		res := lastmile.Run(ctx, lastmile.Options{
//			Tool:      tags.ToolLagotto,
//			Account:   account,
//			Regions:   configuredRegions(cfg),
//			Lifecycle: lifecycleClient(cfg),
//			Footprint: func(ctx context.Context) (*footprint.Report, error) { return inventory(ctx, cfg) },
//			Deleter:   deleter(cfg),
//			Patterns:  namePatterns,
//			Residue:   sharedResidue(account),
//			Mode:      mode,
//			Logf:      debugLog.Printf,
//		})
//		debugLog.Print(res.Summary())
//	}
package lastmile
