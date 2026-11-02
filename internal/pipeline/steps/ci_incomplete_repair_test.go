package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCIStep_IncompleteRepairIsHeldForADecision is run
// 01M6D2GZFJWF6RWN0AT4JWTNMX in miniature: one failing check, one repair that
// really does change the worktree, and the agent's own verdict on whether it
// finished as the only difference between the cases.
func TestCIStep_IncompleteRepairIsHeldForADecision(t *testing.T) {
	const incompleteReason = "the merge is resolved but the snapshot restore, the guards, the type-check and the tests never ran"
	for _, tc := range []struct {
		name          string
		conclusion    string
		wantPublished bool
	}{
		{
			name:          "a_complete_repair_publishes",
			conclusion:    `{"summary":"repair the failing check","code_change_needed":true,"repair_complete":true}`,
			wantPublished: true,
		},
		{
			name:          "an_unreported_verdict_publishes",
			conclusion:    `{"summary":"repair the failing check","code_change_needed":true}`,
			wantPublished: true,
		},
		{
			name:          `an_incomplete_repair_parks`,
			conclusion:    `{"summary":"` + incompleteReason + `","code_change_needed":true,"repair_complete":false}`,
			wantPublished: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				writeCIFix(opts.CWD)
				return &agent.Result{Output: json.RawMessage(tc.conclusion)}, nil
			}}
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"the test check failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
			host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}}

			outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
			if err != nil {
				t.Fatalf("repair returned error: %v\nlog:\n%s", err, f.log())
			}

			localHead := f.localHead(t)
			if localHead == f.headSHA {
				t.Fatalf("the repair commit was never created\nlog:\n%s", f.log())
			}
			published := f.remoteHead(t) != f.headSHA
			if published != tc.wantPublished {
				t.Fatalf("remote advanced = %v, want %v (remote=%s repair=%s)\nlog:\n%s",
					published, tc.wantPublished, f.remoteHead(t), localHead, f.log())
			}

			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.HeadSHA != localHead {
				t.Fatalf("recorded head = %s, want the repair commit %s", run.HeadSHA, localHead)
			}

			if tc.wantPublished {
				if outcome != nil {
					t.Fatalf("outcome = %#v, want the monitor to resume after a published repair", outcome)
				}
				if f.remoteHead(t) != localHead {
					t.Fatalf("remote head = %s, want the repair commit %s", f.remoteHead(t), localHead)
				}
				return
			}

			if outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome = %#v, want an ask-user park that spends no further round", outcome)
			}
			if outcome.RestartFrom != "" {
				t.Fatalf("RestartFrom = %q, want a decision rather than validation restarted on a head the agent called unfinished", outcome.RestartFrom)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			var parked *Finding
			for i := range findings.Items {
				if findings.Items[i].ID == "ci-repair-incomplete" {
					parked = &findings.Items[i]
				}
			}
			if parked == nil {
				t.Fatalf("findings = %+v, want a ci-repair-incomplete finding", findings.Items)
			}
			if parked.Action != types.ActionAskUser {
				t.Fatalf("action = %q, want ask-user", parked.Action)
			}
			if !strings.Contains(parked.Description, incompleteReason) {
				t.Fatalf("the gate does not show the agent's reason:\n%s", parked.Description)
			}
			if run.ReviewApprovedHeadSHA != nil && strings.TrimSpace(*run.ReviewApprovedHeadSHA) != "" {
				t.Fatalf("review approval = %q, want it revoked so Push cannot publish the held repair either", *run.ReviewApprovedHeadSHA)
			}
		})
	}
}
