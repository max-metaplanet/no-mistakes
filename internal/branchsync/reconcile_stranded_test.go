package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// strandedFixture reproduces issue #1523: a terminal post-push run whose
// unpublished tail never reached the gate (#1325 discards the run worktree),
// so the only copy of the recorded pipeline head is whatever the operator
// anchored in the invoking worktree.
type strandedFixture struct {
	ctx                               context.Context
	db                                *db.DB
	repo                              *db.Repo
	run                               *db.Run
	service                           *Service
	local, gate, remote               string
	base, submitted, pushed, pipeline string
}

type strandedOptions struct {
	status types.RunStatus
	// callerHead names which head the clean operator branch sits at.
	callerHead string
	// tailInGate publishes the unpublished tail to the gate as well.
	tailInGate bool
	// tailAnchored keeps the unpublished tail alive in the operator worktree.
	tailAnchored bool
	// verifiedHead stamps terminal_head_verified_at.
	verifiedHead bool
	// dirty leaves an uncommitted change in the operator worktree.
	dirty bool
}

func newStrandedFixture(t *testing.T, opts strandedOptions) *strandedFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")

	pipelineClone := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipelineClone)
	mustRun(t, pipelineClone, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipelineClone, "fix.txt"), "pipeline fix\n")
	mustRun(t, pipelineClone, "add", "fix.txt")
	mustRun(t, pipelineClone, "commit", "-m", "no-mistakes(review): fix")
	pushed := mustRun(t, pipelineClone, "rev-parse", "HEAD")
	mustRun(t, pipelineClone, "push", "origin", "HEAD:refs/heads/feature/recover")
	mustRun(t, pipelineClone, "push", remote, "HEAD:refs/heads/feature/recover")
	mustWrite(t, filepath.Join(pipelineClone, "fix.txt"), "pipeline fix 2\n")
	mustRun(t, pipelineClone, "commit", "-am", "no-mistakes(lint): fix")
	pipelineHead := mustRun(t, pipelineClone, "rev-parse", "HEAD")
	mustRun(t, pipelineClone, "branch", "tail", pipelineHead)
	if opts.tailInGate {
		mustRun(t, pipelineClone, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	if opts.tailAnchored {
		mustRun(t, local, "fetch", pipelineClone, "refs/heads/tail:refs/fm-backup/unpublished")
	}
	mustRun(t, local, "fetch", gate, "refs/heads/feature/recover:refs/fm-backup/gate")
	removeScratchClone(t, pipelineClone)

	caller := map[string]string{"submitted": submitted, "pushed": pushed, "pipeline": pipelineHead}[opts.callerHead]
	if caller == "" {
		t.Fatalf("unknown caller head %q", opts.callerHead)
	}
	mustRun(t, local, "reset", "--hard", caller)
	if opts.dirty {
		mustWrite(t, filepath.Join(local, "file.txt"), "uncommitted operator edit\n")
	}

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA: pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(remote), Ref: "refs/heads/feature/recover",
	}); err != nil {
		t.Fatal(err)
	}
	if opts.verifiedHead {
		err = database.UpdateRunStatusWithVerifiedHead(run.ID, opts.status, pipelineHead)
	} else {
		if err := database.UpdateRunHeadSHA(run.ID, pipelineHead); err != nil {
			t.Fatal(err)
		}
		err = database.UpdateRunStatus(run.ID, opts.status)
	}
	if err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &strandedFixture{
		ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base:      base,
		submitted: submitted, pushed: pushed, pipeline: pipelineHead,
	}
}

func (f *strandedFixture) custodyReturned(t *testing.T) bool {
	t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		t.Fatalf("reload run: %#v, %v", run, err)
	}
	return run.CustodyReturnedAt != nil
}

func (f *strandedFixture) offer(t *testing.T) (State, string) {
	t.Helper()
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.NextAction == nil || state.NextAction.Code != "reconcile_custody" {
		t.Fatalf("no reconciliation offer: safety=%s next=%#v", state.Safety, state.NextAction)
	}
	words := strings.Fields(state.NextAction.Command)
	for i, word := range words {
		if word == "--observed" && i+1 < len(words) {
			return state, words[i+1]
		}
	}
	t.Fatalf("no observation in command: %s", state.NextAction.Command)
	return state, ""
}

func (f *strandedFixture) assertPreserved(t *testing.T, heads ...string) {
	t.Helper()
	for _, dir := range []string{f.local, f.gate} {
		for _, head := range heads {
			if got := mustRun(t, dir, "rev-parse", reconciliationRef(f.run.ID, head)); got != head {
				t.Fatalf("%s preserved %s as %s", dir, head, got)
			}
		}
	}
}

// TestStrandedPostPushCallerAtPushedHeadReconciles is the regression test for
// the cancelled post_push report on issue #1523: the clean branch sits exactly
// at the run's own pushed head, the remote matches it, and the unpublished
// recorded head survives only as an object in the invoking worktree. That
// state used to offer nothing but `no-mistakes axi status`.
func TestStrandedPostPushCallerAtPushedHeadReconciles(t *testing.T) {
	t.Parallel()
	for _, keep := range []string{"local", "pipeline", "pushed"} {
		t.Run(keep, func(t *testing.T) {
			t.Parallel()
			f := newStrandedFixture(t, strandedOptions{
				status: types.RunCancelled, callerHead: "pushed", tailAnchored: true, verifiedHead: true,
			})
			state, token := f.offer(t)
			if !strings.Contains(state.NextAction.Command, "--keep '<local|pipeline|pushed>'") {
				t.Fatalf("offer did not include every keep choice: %s", state.NextAction.Command)
			}
			chosen := map[string]string{"local": f.pushed, "pipeline": f.pipeline, "pushed": f.pushed}[keep]
			result := f.service.Reconcile(f.ctx, keep, token)
			if !result.Recovered || !f.custodyReturned(t) {
				t.Fatalf("reconciliation failed: %#v", result)
			}
			for _, dir := range []string{f.local, f.gate} {
				if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/recover"); got != chosen {
					t.Fatalf("%s branch = %s, want %s", dir, got, chosen)
				}
			}
			f.assertPreserved(t, f.submitted, f.pushed, f.pipeline)
			if got := mustRun(t, f.local, "status", "--porcelain"); got != "" {
				t.Fatalf("worktree was not updated cleanly: %s", got)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != f.pushed {
				t.Fatalf("remote changed to %s", got)
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil || run.HeadSHA != f.pipeline || ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != 1 {
				t.Fatalf("reconciliation changed publication history: %#v, %v", run, err)
			}
		})
	}
}

// TestStrandedPostPushCallerAtSubmittedHeadReconciles covers the #1523 body
// shape whose recorded head still exists: the caller never moved off the
// submitted head and the unpublished tail is only in the invoking worktree.
func TestStrandedPostPushCallerAtSubmittedHeadReconciles(t *testing.T) {
	t.Parallel()
	f := newStrandedFixture(t, strandedOptions{
		status: types.RunFailed, callerHead: "submitted", tailAnchored: true, verifiedHead: true,
	})
	_, token := f.offer(t)
	result := f.service.Reconcile(f.ctx, "pipeline", token)
	if !result.Recovered || !f.custodyReturned(t) {
		t.Fatalf("reconciliation failed: %#v", result)
	}
	for _, dir := range []string{f.local, f.gate} {
		if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/recover"); got != f.pipeline {
			t.Fatalf("%s branch = %s, want %s", dir, got, f.pipeline)
		}
	}
	f.assertPreserved(t, f.submitted, f.pushed, f.pipeline)
	if result.NextAction == nil || result.NextAction.Code != "run_pipeline" {
		t.Fatalf("recovered state does not point at a fresh run: %#v", result.NextAction)
	}
}

// TestStrandedUnmaterializedHeadOffersAnExplicitRelease covers the #1523 body
// case itself: the recorded head was never verified and is no Git object
// anywhere, so its content cannot be recovered. The operator must still get a
// named release rather than a read-only next_action, and choosing the
// unrecoverable head must refuse.
func TestStrandedUnmaterializedHeadOffersAnExplicitRelease(t *testing.T) {
	t.Parallel()
	f := newStrandedFixture(t, strandedOptions{status: types.RunFailed, callerHead: "submitted"})
	state, token := f.offer(t)
	if !strings.Contains(state.NextAction.Command, "--keep '<local|pushed>'") {
		t.Fatalf("release offer should exclude the unrecoverable head: %s", state.NextAction.Command)
	}
	if !strings.Contains(state.Error, f.pipeline) || !strings.Contains(state.Error, "cannot be recovered") {
		t.Fatalf("release offer does not name the unrecoverable head: %s", state.Error)
	}
	refused := f.service.Reconcile(f.ctx, "pipeline", token)
	if refused.Recovered || f.custodyReturned(t) {
		t.Fatalf("keeping an unrecoverable head was accepted: %#v", refused)
	}
	result := f.service.Reconcile(f.ctx, "pushed", token)
	if !result.Recovered || !f.custodyReturned(t) {
		t.Fatalf("release failed: %#v", result)
	}
	for _, dir := range []string{f.local, f.gate} {
		if got := mustRun(t, dir, "rev-parse", "refs/heads/feature/recover"); got != f.pushed {
			t.Fatalf("%s branch = %s, want %s", dir, got, f.pushed)
		}
	}
	f.assertPreserved(t, f.submitted, f.pushed)
	if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != f.pushed {
		t.Fatalf("remote changed to %s", got)
	}
}

// TestStrandedReconciliationKeepsItsProofs pins the guards that a wider
// caller and a worktree-only tail must not weaken.
func TestStrandedReconciliationKeepsItsProofs(t *testing.T) {
	t.Parallel()
	cases := map[string]strandedOptions{
		"present-but-unverified-head": {status: types.RunCancelled, callerHead: "pushed", tailAnchored: true},
		"dirty-caller":                {status: types.RunCancelled, callerHead: "pushed", tailAnchored: true, verifiedHead: true, dirty: true},
		"active-run":                  {status: types.RunRunning, callerHead: "pushed", tailAnchored: true},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newStrandedFixture(t, opts)
			state := f.service.InspectCached(f.ctx)
			if state.NextAction != nil && state.NextAction.Code == "reconcile_custody" {
				t.Fatalf("unproven evidence was offered a reconciliation: %#v", state)
			}
			before := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)")
			got := f.service.Reconcile(f.ctx, "pushed", "any-token")
			if got.Recovered || f.custodyReturned(t) {
				t.Fatalf("unproven evidence was reconciled: %#v", got)
			}
			if after := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
				t.Fatal("refusal changed local refs")
			}
		})
	}
}
