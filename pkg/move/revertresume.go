package move

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/block/spirit/pkg/checkpoint"
	"github.com/block/spirit/pkg/dbconn"
	"github.com/block/spirit/pkg/dbconn/sqlescape"
	"github.com/block/spirit/pkg/move/check"
	"github.com/block/spirit/pkg/status"
	"github.com/block/spirit/pkg/table"
	"github.com/block/spirit/pkg/utils"
)

// Crash recovery for an interrupted REVERSE cutover (checkpoint phase
// "reverting"). The reverse cutover's step order (see reverseCutover) is what
// makes a safe decision possible:
//
//	persist "reverting" → un-retire every source shard (<t>_old → <t>) →
//	switch traffic back to the source (caller hook, NOT idempotent) →
//	retire every target (<t> → <t>_revert, under the target locks) →
//	persist "reverse_finalized" → drop marker/checkpoint.
//
// Two steps bracket the non-idempotent traffic switch:
//   - a source table still in its _old form means un-retire did not finish, so
//     the switch never ran and every target must still serve under its real
//     name (target retirement happens strictly after the switch); the rollback
//     can safely be completed: rebuild the reverse feed, re-flush, run the
//     switch once, finish the renames;
//   - every target retired to _revert means the switch definitely ran and only
//     idempotent cleanup remains;
//   - anything in between (all sources un-retired but a target still serving)
//     is a state in which the switch MAY have run — re-invoking it could
//     double-switch traffic, so resume terminates with ErrOwnershipAmbiguous
//     and leaves the checkpoint and revert marker for manual reconciliation.

// tableForm is the observed physical form of one moved table on one shard or
// target. Source shards present formReal/formOld; targets present
// formReal/formRevert; formAbsent/formBoth are never a valid cutover state.
type tableForm int

const (
	formAbsent tableForm = iota // neither the real name nor the retired name exists
	formReal                    // live under the table's real name
	formOld                     // source side: retired to <t>_old by the forward cutover
	formRevert                  // target side: retired to <t>_revert by the reverse cutover
	formBoth                    // both the real and the retired name exist
)

func (f tableForm) String() string {
	switch f {
	case formReal:
		return "real"
	case formOld:
		return "old"
	case formRevert:
		return "revert"
	case formBoth:
		return "both"
	default:
		return "absent"
	}
}

type revertTableSnapshot struct {
	table   string
	sources []tableForm // per source shard
	targets []tableForm // per target
}

type revertSnapshot struct {
	tables []revertTableSnapshot
}

type revertAction int

const (
	revertActionAmbiguous revertAction = iota
	// revertActionCompleteRollback: the traffic switch provably never ran;
	// rebuild the reverse feed and finish the rollback.
	revertActionCompleteRollback
	// revertActionFinalize: ownership is definitively back on the source; only
	// idempotent cleanup (marker/checkpoint drop) can remain.
	revertActionFinalize
)

func (a revertAction) String() string {
	switch a {
	case revertActionCompleteRollback:
		return "complete-rollback"
	case revertActionFinalize:
		return "finalize"
	default:
		return "terminate-ambiguous"
	}
}

type revertDecision struct {
	action revertAction
	// reason is populated for revertActionAmbiguous and surfaced to the
	// operator; it must describe the observed state and what to do manually.
	reason string
}

// decideRevertAction is the pure resume decision for a "reverting" checkpoint.
// It takes the physical table snapshot and whether every target has a non-empty
// reverse-feed start position in the checkpoint. The decision rules derive
// directly from the reverse cutover's step order (see the file comment):
//
//   - any source still _old, no target retired yet, positions complete →
//     complete the rollback (switch never ran);
//   - all sources un-retired and all targets retired → finalize (switch ran);
//   - anything else — missing/duplicate forms, a retired target alongside an
//     _old source (an ordering the cutover cannot produce), or an incomplete
//     rollback whose positions are missing → terminate ambiguous.
func decideRevertAction(snap revertSnapshot, positionsComplete bool) revertDecision {
	if len(snap.tables) == 0 {
		return revertDecision{action: revertActionAmbiguous,
			reason: "no moved tables found on the source shards; cannot determine where the interrupted reverse cutover stopped"}
	}
	var anyOldSource, anyRealTarget, anyRetiredTarget bool
	var invalid []string
	for _, t := range snap.tables {
		for si, f := range t.sources {
			switch f {
			case formReal:
			case formOld:
				anyOldSource = true
			default:
				invalid = append(invalid, fmt.Sprintf(
					"source shard %d: table %q is neither live (%q) nor retired (%q), observed form=%q",
					si, t.table, t.table, check.CutoverOldName(t.table), f))
			}
		}
		for ti, f := range t.targets {
			switch f {
			case formReal:
				anyRealTarget = true
			case formRevert:
				anyRetiredTarget = true
			default:
				invalid = append(invalid, fmt.Sprintf(
					"target %d: table %q is neither live (%q) nor retired (%q), observed form=%q",
					ti, t.table, t.table, check.RevertRetiredName(t.table), f))
			}
		}
	}
	if len(invalid) > 0 {
		return revertDecision{action: revertActionAmbiguous,
			reason: "the table layout matches no reachable point of the reverse cutover: " + strings.Join(invalid, "; ")}
	}
	if anyOldSource {
		if anyRetiredTarget {
			return revertDecision{action: revertActionAmbiguous,
				reason: "a source table is still retired to _old while a target is already retired to _revert; the reverse cutover never produces this ordering, so the layout may have been changed outside spirit"}
		}
		if !positionsComplete {
			return revertDecision{action: revertActionAmbiguous,
				reason: "the rollback is unfinished (source tables still carry their _old name) but the checkpoint carries no reverse-feed start position for every target; rebuilding the feed from the targets' current head could silently lose writes committed while spirit was down"}
		}
		return revertDecision{action: revertActionCompleteRollback}
	}
	// Every source table is un-retired. The traffic switch sits exactly here in
	// the step order, so target layout decides whether it provably ran.
	switch {
	case anyRetiredTarget && !anyRealTarget:
		return revertDecision{action: revertActionFinalize}
	case anyRetiredTarget && anyRealTarget:
		return revertDecision{action: revertActionAmbiguous,
			reason: "all source tables are un-retired but some targets still serve under their real name while others are already retired to _revert; the reverse traffic switch may or may not have executed"}
	default:
		return revertDecision{action: revertActionAmbiguous,
			reason: "all source tables are un-retired but every target still serves under its real name; the reverse traffic switch may or may not have executed"}
	}
}

// tablePresence reports which of the logical table's two forms exist on db —
// its real name and its retired name (logical + retiredSuffix: "_old" on a
// source shard, "_revert" on a target). The schema is taken from DATABASE(), so
// db must default to the shard's schema.
func tablePresence(ctx context.Context, db *sql.DB, logical, retiredSuffix string) (real, retired bool, err error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (?, ?)",
		logical, logical+retiredSuffix)
	if err != nil {
		return false, false, err
	}
	defer utils.CloseAndLog(rows)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, err
		}
		if name == logical {
			real = true
		} else {
			retired = true
		}
	}
	return real, retired, rows.Err()
}

// presenceForm maps the two presence booleans to a tableForm, with the caller
// supplying which form each boolean denotes (formReal/formOld on sources,
// formReal/formRevert on targets).
func presenceForm(real, retired bool, realForm, retiredForm tableForm) tableForm {
	switch {
	case real && retired:
		return formBoth
	case real:
		return realForm
	case retired:
		return retiredForm
	default:
		return formAbsent
	}
}

// discoverRevertLogicalTables recovers the logical names of the moved tables
// for a reverting-phase resume. Every moved table exists on EITHER side in one
// of two forms — a source shard holds <t> (un-retired) or <t>_old (still
// retired), a target holds <t> (still serving) or <t>_revert (retired) — so
// the names are listed on every source shard AND on targets[0], the known
// suffix is stripped, and the union is taken. Listing both sides (not just the
// sources) means a moved table that vanished from every source shard through
// outside interference is still probed — it then classifies as absent and the
// resume terminates ambiguous rather than skipping it. When SourceTables names
// an explicit list, that whole list is seeded up front for the same reason and
// the union is intersected with it; move-everything moves all tables, so no
// unrelated names can appear.
func (r *Runner) discoverRevertLogicalTables(ctx context.Context) ([]string, error) {
	wanted := make(map[string]bool, len(r.move.SourceTables))
	for _, t := range r.move.SourceTables {
		wanted[t] = true
	}
	found := make(map[string]bool)
	for t := range wanted {
		// Explicit list: probe every wanted name even if it is absent on both
		// sides (absence classifies as formAbsent → terminate ambiguous).
		found[t] = true
	}

	list := func(side string, idx int, db *sql.DB, retiredSuffix string) error {
		rows, err := db.QueryContext(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name NOT LIKE '\\_spirit%'")
		if err != nil {
			return fmt.Errorf("resume reverse cutover: list tables on %s %d: %w", side, idx, err)
		}
		defer utils.CloseAndLog(rows)
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			logical := name
			if strings.HasSuffix(name, retiredSuffix) {
				logical = strings.TrimSuffix(name, retiredSuffix)
			}
			if len(wanted) > 0 && !wanted[logical] {
				continue
			}
			found[logical] = true
		}
		return rows.Err()
	}
	for si := range r.sources {
		if err := list("source shard", si, r.sources[si].db, "_old"); err != nil {
			return nil, fmt.Errorf("resume reverse cutover: %w", err)
		}
	}
	for ti := range r.targets {
		if err := list("target", ti, r.targets[ti].DB, "_revert"); err != nil {
			return nil, fmt.Errorf("resume reverse cutover: %w", err)
		}
	}
	return slices.Sorted(maps.Keys(found)), nil
}

func (r *Runner) buildRevertSnapshot(ctx context.Context, logical []string) (revertSnapshot, error) {
	snap := revertSnapshot{tables: make([]revertTableSnapshot, 0, len(logical))}
	for _, name := range logical {
		st := revertTableSnapshot{
			table:   name,
			sources: make([]tableForm, 0, len(r.sources)),
			targets: make([]tableForm, 0, len(r.targets)),
		}
		for si := range r.sources {
			real, old, err := tablePresence(ctx, r.sources[si].db, name, "_old")
			if err != nil {
				return snap, fmt.Errorf("resume reverse cutover: probe source shard %d for table %q: %w", si, name, err)
			}
			st.sources = append(st.sources, presenceForm(real, old, formReal, formOld))
		}
		for ti := range r.targets {
			real, rev, err := tablePresence(ctx, r.targets[ti].DB, name, "_revert")
			if err != nil {
				return snap, fmt.Errorf("resume reverse cutover: probe target %d for table %q: %w", ti, name, err)
			}
			st.targets = append(st.targets, presenceForm(real, rev, formReal, formRevert))
		}
		snap.tables = append(snap.tables, st)
	}
	return snap, nil
}

// summary renders the observed layout for the resume log, one
// "name:src=forms|dst=forms" entry per logical table, e.g.
// "t1:src=old/old,dst=real/real". It is audit evidence for the resume
// decision, not a machine-checked signal.
func (s revertSnapshot) summary() string {
	var b strings.Builder
	for i, t := range s.tables {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:src=", t.table)
		for j, f := range t.sources {
			if j > 0 {
				b.WriteByte('/')
			}
			b.WriteString(f.String())
		}
		b.WriteString(",dst=")
		for j, f := range t.targets {
			if j > 0 {
				b.WriteByte('/')
			}
			b.WriteString(f.String())
		}
	}
	return b.String()
}

// reversePositionsComplete reports whether the checkpoint carries a non-empty
// reverse-feed start position for every target, keyed by targetKey.
func (r *Runner) reversePositionsComplete() bool {
	if len(r.targets) == 0 {
		return false
	}
	for i := range r.targets {
		if r.reversePositions[targetKey(r.targets[i])] == "" {
			return false
		}
	}
	return true
}

// resumeReverting takes over a restart whose checkpoint says the reverse
// cutover was underway (phase "reverting"). It decides, from the physical table
// layout and the checkpoint's reverse positions, whether to finish the rollback,
// run only the idempotent final cleanup, or terminate for manual reconciliation.
// It never re-copies and never re-runs the forward cutover: like the other
// reverse resumes, it is invoked from maybeResumeReverseWindow before discovery
// and locks.
func (r *Runner) resumeReverting(ctx context.Context, rec checkpoint.Record) error {
	r.cutoverAt = rec.CutoverAt
	positions := make(map[string]string)
	if strings.TrimSpace(rec.Position) != "" {
		if err := json.Unmarshal([]byte(rec.Position), &positions); err != nil {
			return fmt.Errorf("%w: resume interrupted reverse cutover: cannot parse checkpoint positions: %v",
				status.ErrOwnershipAmbiguous, err)
		}
	}
	r.reversePositions = positions

	logical, err := r.discoverRevertLogicalTables(ctx)
	if err != nil {
		return err
	}
	snap, err := r.buildRevertSnapshot(ctx, logical)
	if err != nil {
		return err
	}
	decision := decideRevertAction(snap, r.reversePositionsComplete())

	r.logger.Warn("interrupted reverse cutover detected; deciding the resume action from the physical table state",
		"cutover_at", rec.CutoverAt,
		"decision", decision.action.String(),
		"tables", logical,
		"forms", snap.summary())

	switch decision.action {
	case revertActionFinalize:
		// Every former target is retired, so the traffic switch definitely ran
		// and ownership is definitively back on the source. Only the idempotent
		// cleanup that follows the last rename can be outstanding.
		return newReverseWindow(r).finalizeReverse(ctx)
	case revertActionCompleteRollback:
		return r.completeRevertRollback(ctx, logical)
	default:
		return fmt.Errorf("%w: refusing to auto-resume the interrupted reverse cutover: %s. Reconcile table ownership manually, then either finish the rollback by hand and remove %s and %s, or re-run once the layout is consistent",
			status.ErrOwnershipAmbiguous, decision.reason, revertMarkerName, checkpointTableName)
	}
}

// completeRevertRollback finishes an interrupted rollback when the reverse
// traffic switch provably never ran (at least one source table still _old and no
// target retired). Traffic is still on the targets, so writes that landed while
// spirit was down are captured by rebuilding the reverse feed from the
// checkpointed positions and flushing them before the switch.
func (r *Runner) completeRevertRollback(ctx context.Context, logical []string) error {
	// 1. Finish un-retiring the sources (<t>_old → <t>). Nothing writes the
	//    _old tables at this point — the previous run's reverse feed is dead
	//    and application traffic is still on the targets — so the renames need
	//    no table lock.
	for si := range r.sources {
		s := &r.sources[si]
		for _, name := range logical {
			if err := renameOldToRealIfPresent(ctx, s.db, name); err != nil {
				return fmt.Errorf("resume reverse cutover: un-retire source shard %d table %q: %w", si, name, err)
			}
		}
		// Rebuild this shard's table list (names only; buildFeed derives the
		// real-name TableInfos itself), mirroring resumeReverseWindow.
		s.tables = make([]*table.TableInfo, 0, len(logical))
		for _, name := range logical {
			s.tables = append(s.tables, table.NewTableInfo(s.db, s.config.DBName, name))
		}
	}
	r.sourceTables = r.sources[0].tables

	return r.status.Do(status.ReverseWindow, func() error {
		w := newReverseWindow(r)
		// resuming: the source tables were just un-retired, so the feed applies
		// to their real names, the stale-_revert cleanup is skipped (a _revert
		// table here would be an inconsistency the decision already rejected),
		// and every rename in reverseCutover is idempotent.
		w.resuming = true
		if err := w.buildFeed(ctx); err != nil {
			return err
		}
		defer w.feed.Close()
		if err := w.feed.Start(ctx); err != nil {
			return fmt.Errorf("resume reverse cutover: start reverse feed: %w", err)
		}
		// Drain the downtime backlog before taking the target table locks.
		if err := w.waitCaughtUp(ctx); err != nil {
			return err
		}
		// Lock the targets, final flush, run the traffic switch (it never
		// executed before the crash), retire the targets, finalize.
		return w.reverseCutover(ctx)
	})
}

// renameOldToRealIfPresent un-retires one source table (<t>_old → <t>). It is a
// no-op when the real table already exists (the shard was un-retired before
// the crash); any other combination is an error the decision excluded.
func renameOldToRealIfPresent(ctx context.Context, db *sql.DB, name string) error {
	real, old, err := tablePresence(ctx, db, name, "_old")
	if err != nil {
		return err
	}
	switch {
	case real && !old:
		return nil // already un-retired
	case old && !real:
		return dbconn.Exec(ctx, db, "RENAME TABLE %n TO %n", check.CutoverOldName(name), name)
	default:
		// Neither/both: the layout is inconsistent with any reachable cutover
		// state — ownership can no longer be reasoned about, so mark it
		// ambiguous rather than returning a plain failure.
		return fmt.Errorf("%w: table %q: expected exactly one of %q (retired) or %q (live), found real=%v retired=%v",
			status.ErrOwnershipAmbiguous, name, check.CutoverOldName(name), name, real, old)
	}
}

// renameRealToRevertIfPresent retires one target table (<t> → <t>_revert) via
// the target's held table lock. It is a no-op when the _revert form already
// exists (the table was retired before the crash); any other combination is an
// error the decision excluded.
func renameRealToRevertIfPresent(ctx context.Context, db *sql.DB, lock *dbconn.TableLock, name string) error {
	real, rev, err := tablePresence(ctx, db, name, "_revert")
	if err != nil {
		return err
	}
	switch {
	case rev && !real:
		return nil // already retired
	case real && !rev:
		stmt := sqlescape.MustEscapeSQL("RENAME TABLE %n TO %n", name, check.RevertRetiredName(name))
		return lock.ExecUnderLock(ctx, stmt)
	default:
		// Neither/both. The traffic switch may already have run (this rename
		// follows it), so an inconsistent layout is an ambiguous ownership
		// state, not a retriable failure.
		return fmt.Errorf("%w: table %q: expected exactly one of %q (live) or %q (retired), found real=%v retired=%v",
			status.ErrOwnershipAmbiguous, name, name, check.RevertRetiredName(name), real, rev)
	}
}
