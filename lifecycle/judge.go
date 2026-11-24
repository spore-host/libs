package lifecycle

import "time"

// The reader's judgement (lagotto#340). Writers only add and remove what
// keeps an item in use (a creator's mark, a hold, a lease, a MarkActive
// pulse) and stamp when they did; none of them sets an item IDLE as a side
// effect. Whether an item is in use, and since when it has been idle,
// follows from what it holds and those times alone, so an item a writer
// could not finish with (a failed MarkDone, a lost reply, a creator that
// died) ages out by itself. The state field is kept for older readers; for
// new ones these two are authoritative. A backstop or an actor judges an
// item with them, then acts only as judged: SetIdleIfUnchanged and
// ClaimIfUnchanged with ExpectOf the item it judged.

// InUse reports whether the item, as read, is in use at now. It is, while
// any of these holds:
//
//   - a live lease is on it: a heartbeat younger than DefaultStaleAfter;
//   - a hold is on it;
//   - a creator mark on it is younger than markBound, or has no time (an
//     older mark is a creator that died);
//   - its last MarkActive (PulseAt) is younger than markBound;
//   - it is ACTIVE from a write with no times: its State or Epoch is not
//     what the last write of this package stamped (StampState, StampEpoch),
//     so an older client activated it since, and nothing can age that. A
//     backstop's idle Observe, which sees every activity signal, ends it.
//
// An absent item (the zero Item) is not in use.
func InUse(it Item, now time.Time, markBound time.Duration) bool {
	return inUse(it, now, markBound, DefaultStaleAfter)
}

// IdleSince is when the item, not in use at now, stopped being in use: the
// latest of
//
//   - LastActivityAt (a mark added or removed, a hold added or removed, a
//     MarkActive, an active Observe, a Release);
//   - each mark's time plus markBound, and PulseAt plus markBound;
//   - the lease's heartbeat plus DefaultStaleAfter;
//   - the stored IdleSince, when an older client wrote the state last (the
//     stamps differ) or the item has none of the times above.
//
// It is the zero time for an item in use, and for one with no time at all
// (an absent item): callers compare now.Sub(IdleSince(...)) with their grace
// only after InUse is false, and a zero time is idle for any grace.
func IdleSince(it Item, now time.Time, markBound time.Duration) time.Time {
	return idleSince(it, now, markBound, DefaultStaleAfter)
}

// InUse is the package's InUse with the Client's own clock and StaleAfter.
func (c *Client) InUse(it Item, markBound time.Duration) bool {
	return inUse(it, c.now(), markBound, c.o.StaleAfter)
}

// IdleSince is the package's IdleSince with the Client's own clock and
// StaleAfter.
func (c *Client) IdleSince(it Item, markBound time.Duration) time.Time {
	return idleSince(it, c.now(), markBound, c.o.StaleAfter)
}

// untimed reports whether the item's state was last written by a client that
// stamps nothing (one older than the stamps).
func untimed(it Item) bool {
	return it.State != "" && (it.StampState != it.State || it.StampEpoch != it.Epoch)
}

// liveMark reports whether a creator mark on the item is younger than
// markBound, or has no time.
func liveMark(it Item, now time.Time, markBound time.Duration) bool {
	for _, m := range it.Marks {
		if m.At.IsZero() || now.Sub(m.At) < markBound {
			return true
		}
	}
	return false
}

func inUse(it Item, now time.Time, markBound, staleAfter time.Duration) bool {
	switch {
	case it.Lease != nil && now.Sub(it.Lease.Heartbeat) < staleAfter:
		return true
	case len(it.Holds) > 0, liveMark(it, now, markBound):
		return true
	case !it.PulseAt.IsZero() && now.Sub(it.PulseAt) < markBound:
		return true
	}
	return it.State == StateActive && untimed(it)
}

func idleSince(it Item, now time.Time, markBound, staleAfter time.Duration) time.Time {
	if inUse(it, now, markBound, staleAfter) {
		return time.Time{}
	}
	latest := it.LastActivityAt
	later := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	for _, m := range it.Marks {
		later(m.At.Add(markBound))
	}
	if !it.PulseAt.IsZero() {
		later(it.PulseAt.Add(markBound))
	}
	if it.Lease != nil {
		later(it.Lease.Heartbeat.Add(staleAfter))
	}
	if latest.IsZero() || untimed(it) {
		later(it.IdleSince)
	}
	return latest
}
