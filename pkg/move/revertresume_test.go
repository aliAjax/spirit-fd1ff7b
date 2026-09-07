package move

// Pure decision-table tests for the interrupted-reverse-cutover resume. These
// construct the physical-table snapshot directly, so they need no MySQL; the
// probing that builds the snapshot and the resulting actions are covered by
// the end-to-end tests in reversewindow_test.go.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// forms builds a one-table snapshot with the given per-source and per-target
// forms.
func forms(sources, targets []tableForm) revertSnapshot {
	return revertSnapshot{tables: []revertTableSnapshot{{
		table:   "t1",
		sources: sources,
		targets: targets,
	}}}
}

func TestDecideRevertAction(t *testing.T) {
	for _, tt := range []struct {
		name              string
		snap              revertSnapshot
		positionsComplete bool
		want              revertAction
	}{
		{
			name:              "all sources old, targets real, positions complete -> complete rollback",
			snap:              forms([]tableForm{formOld, formOld}, []tableForm{formReal, formReal}),
			positionsComplete: true,
			want:              revertActionCompleteRollback,
		},
		{
			name:              "some sources un-retired already -> still complete rollback",
			snap:              forms([]tableForm{formReal, formOld}, []tableForm{formReal}),
			positionsComplete: true,
			want:              revertActionCompleteRollback,
		},
		{
			name:              "rollback unfinished but positions missing -> terminate",
			snap:              forms([]tableForm{formOld}, []tableForm{formReal}),
			positionsComplete: false,
			want:              revertActionAmbiguous,
		},
		{
			name:              "all sources real, all targets retired -> finalize",
			snap:              forms([]tableForm{formReal, formReal}, []tableForm{formRevert, formRevert}),
			positionsComplete: true,
			want:              revertActionFinalize,
		},
		{
			name:              "finalize-shaped state does not need positions",
			snap:              forms([]tableForm{formReal}, []tableForm{formRevert}),
			positionsComplete: false,
			want:              revertActionFinalize,
		},
		{
			name:              "all sources real, targets still serving -> switch may have run, terminate",
			snap:              forms([]tableForm{formReal, formReal}, []tableForm{formReal, formReal}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "some targets retired while others still serve -> terminate",
			snap:              forms([]tableForm{formReal}, []tableForm{formRevert, formReal}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "a source still old but a target already retired -> ordering violation, terminate",
			snap:              forms([]tableForm{formReal, formOld}, []tableForm{formRevert}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "source table missing (absent) -> terminate",
			snap:              forms([]tableForm{formAbsent}, []tableForm{formReal}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "source has both forms -> terminate",
			snap:              forms([]tableForm{formBoth}, []tableForm{formReal}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "target table missing (absent) -> terminate",
			snap:              forms([]tableForm{formReal}, []tableForm{formAbsent}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "target has both forms -> terminate",
			snap:              forms([]tableForm{formOld}, []tableForm{formBoth}),
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
		{
			name:              "no tables discovered -> terminate",
			snap:              revertSnapshot{},
			positionsComplete: true,
			want:              revertActionAmbiguous,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := decideRevertAction(tt.snap, tt.positionsComplete)
			require.Equal(t, tt.want, got.action, "decision=%s reason=%q", got.action, got.reason)
			if tt.want == revertActionAmbiguous {
				require.NotEmpty(t, got.reason, "an ambiguous decision must carry an operator-facing reason")
			}
		})
	}
}

// TestDecideRevertActionMultiTableGlobalSwitchProof: the switch proof is global
// across tables — one table still mid-un-retire while another is fully
// un-retired still means the switch never ran (targets not yet retired), so the
// rollback completes rather than terminating.
func TestDecideRevertActionMultiTableGlobalSwitchProof(t *testing.T) {
	snap := revertSnapshot{tables: []revertTableSnapshot{
		{table: "a", sources: []tableForm{formReal, formReal}, targets: []tableForm{formReal, formReal}},
		{table: "b", sources: []tableForm{formReal, formOld}, targets: []tableForm{formReal, formReal}},
	}}
	got := decideRevertAction(snap, true)
	require.Equal(t, revertActionCompleteRollback, got.action, "reason=%q", got.reason)
}
