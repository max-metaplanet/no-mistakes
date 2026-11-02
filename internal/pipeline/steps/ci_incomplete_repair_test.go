package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

// TestCIStep_IncompleteVerdictSurvivesAProtectedPathRefusal is the same
// incident with one more obstacle in the way: the unfinished repair also
// touches a protected path, so it first parks at the protected-path gate.
// Approving that path with a fix response must not publish what the agent
// said was not ready; it lands at the incomplete-repair gate instead.
func TestCIStep_IncompleteVerdictSurvivesAProtectedPathRefusal(t *testing.T) {
	t.Parallel()
	const incompleteReason = "the migration is renamed but the snapshot restore and the tests never ran"
	for _, tc := range []struct {
		name          string
		conclusion    string
		wantPublished bool
	}{
		{
			name:          "a_complete_repair_publishes_after_the_retry",
			conclusion:    `{"summary":"repair the failing check","code_change_needed":true,"repair_complete":true}`,
			wantPublished: true,
		},

		{
			name:          "an_incomplete_repair_parks_after_the_retry",
			conclusion:    `{"summary":"` + incompleteReason + `","code_change_needed":true,"repair_complete":false}`,
			wantPublished: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "retained repair\n"} {
					if err := os.WriteFile(filepath.Join(opts.CWD, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(tc.conclusion)}, nil
			}}
			f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			outcome, err := f.run(t)
			if err != nil || outcome == nil || !pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("repair did not refuse: %+v, %v\n%s", outcome, err, f.log())
			}
			if _, carried := ciCarriedIncompleteRepair(outcome.Findings); carried == tc.wantPublished {
				t.Fatalf("protected-path gate carries the incomplete verdict = %v, want %v:\n%s", carried, !tc.wantPublished, outcome.Findings)
			}
			if !tc.wantPublished && !strings.Contains(outcome.Findings, incompleteReason) {
				t.Fatalf("the protected-path gate does not show the agent's reason:\n%s", outcome.Findings)
			}

			persistCIRefusal(t, f, outcome)
			if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
				t.Fatal(err)
			}
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = outcome.Findings
			f.sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"SUCCESS","bucket":"pass"}]`),
				`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`)
			outcome, err = f.run(t)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("retry: %+v, %v\n%s", outcome, err, f.log())
			}
			if calls != 1 {
				t.Fatalf("retry ran another fixer over retained work: %d calls", calls)
			}
			localHead := f.localHead(t)
			if localHead == f.headSHA {
				t.Fatalf("the retry never committed the retained repair\n%s", f.log())
			}
			published := f.remoteHead(t) == localHead
			if published != tc.wantPublished {
				t.Fatalf("published = %v, want %v (local=%s remote=%s)\n%s", published, tc.wantPublished, localHead, f.remoteHead(t), f.log())
			}
			if tc.wantPublished {
				return
			}
			if f.remoteHead(t) != f.headSHA {
				t.Fatalf("remote head = %s, want the pre-repair head %s", f.remoteHead(t), f.headSHA)
			}
			if outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("outcome = %+v, want a park at the incomplete-repair gate", outcome)
			}
			if pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("the resolved protected-path refusal is still parked:\n%s", outcome.Findings)
			}
			parked, carried := ciCarriedIncompleteRepair(outcome.Findings)
			if !carried || parked.Action != types.ActionAskUser || !strings.Contains(parked.Description, incompleteReason) {
				t.Fatalf("findings = %s, want the ask-user incomplete-repair finding with the agent's reason", outcome.Findings)
			}
		})
	}
}

// TestCIStep_AnEarlierRoundsIncompleteVerdictIsNotCarried: a ci-repair-incomplete
// finding left unselected at an earlier gate comes back as a deferred finding.
// A later round whose agent reported its repair complete must not forward it,
// or the protected-path retry would read it back as this round's verdict and
// hold a finished repair, and a revalidation restart would park on it.
func TestCIStep_AnEarlierRoundsIncompleteVerdictIsNotCarried(t *testing.T) {
	t.Parallel()
	const stale = `{"findings":[{"id":"ci-repair-incomplete","severity":"warning","description":"an earlier round's agent called its repair unfinished","action":"ask-user"},{"id":"ci-other","severity":"warning","description":"an unrelated deferred finding","action":"ask-user"}]}`
	for _, tc := range []struct {
		name       string
		revalidate bool
		protected  bool
	}{
		{name: "protected_path_refusal", protected: true},
		{name: "revalidation_restart", revalidate: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.revalidate, nil)
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				for file, content := range map[string]string{"package.lock": "refused\n", "fix.go": "repair\n"} {
					if err := os.WriteFile(filepath.Join(opts.CWD, file), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return &agent.Result{Output: json.RawMessage(`{"summary":"repair the failing check","code_change_needed":true,"repair_complete":true}`)}, nil
			}}
			if tc.protected {
				f.sctx.Config.ProtectedPaths = []string{"*.lock"}
			}
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"the test check failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
			f.sctx.DeferredFindings = stale
			host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}}

			outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
			if err != nil || outcome == nil {
				t.Fatalf("outcome = %+v, err = %v\n%s", outcome, err, f.log())
			}
			if tc.protected != pipeline.HasProtectedPathRefusal(outcome.Findings) {
				t.Fatalf("protected-path refusal = %v, want %v:\n%s", !tc.protected, tc.protected, outcome.Findings)
			}
			if tc.revalidate && outcome.RestartFrom == "" {
				t.Fatalf("outcome = %+v, want a revalidation restart", outcome)
			}
			if _, carried := ciCarriedIncompleteRepair(outcome.Findings); carried {
				t.Fatalf("an earlier round's incomplete verdict was carried into this round:\n%s", outcome.Findings)
			}
			if !strings.Contains(outcome.Findings, "ci-other") {
				t.Fatalf("the unrelated deferred finding was dropped:\n%s", outcome.Findings)
			}
		})
	}
}
