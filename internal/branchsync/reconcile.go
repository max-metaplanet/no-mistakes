package branchsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// reconciliationObservation is a read-only lease, not authority to choose a
// head. The operator supplies that choice separately. Ref values include
// absence so even a newly created recovery anchor invalidates an old lease.
type reconciliationObservation struct {
	RunID, Branch, Local, Pipeline, Pushed, Gate, Submitted string
	Repository, Worktree                                    string
	Target, Kind                                            string
	// PipelineRecoverable reports whether the recorded pipeline head is a
	// Git object in either repository. When it is false the head's content
	// can never be recovered and the branch can only be released at a head
	// that still exists. Reconciliation copies an observed commit into both
	// repositories, so this records presence rather than which one holds it.
	PipelineRecoverable    bool
	Generation, VerifiedAt int64
	BranchRuns             int
	Refs                   []string
}

func (o reconciliationObservation) token() string {
	b, _ := json.Marshal(o)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func reconciliationRef(run, head string) string {
	return "refs/no-mistakes/recover-reconcile/" + run + "/" + head
}

// observedHeads lists every observed head that is a Git object, so each one
// is anchored before the branch moves. An unrecoverable pipeline head is no
// object at all, so it can be neither anchored nor chosen.
func (o reconciliationObservation) observedHeads() []string {
	heads := []string{o.Local}
	if o.PipelineRecoverable {
		heads = append(heads, o.Pipeline)
	}
	return append(heads, o.Pushed, o.Gate, o.Submitted)
}

func (o reconciliationObservation) anchors() map[string]string {
	refs := map[string]string{}
	if o.PipelineRecoverable {
		refs[custody.RecoveryRef(o.RunID)] = o.Pipeline
	}
	for _, head := range o.observedHeads() {
		refs[reconciliationRef(o.RunID, head)] = head
	}
	return refs
}

// commitHome names the repository that holds commit, preferring the local
// gate so reconciliation reads the pipeline's own copy when there is one.
func (s *Service) commitHome(ctx context.Context, commit string) string {
	if strings.TrimSpace(commit) == "" {
		return ""
	}
	if objectExists(ctx, s.GateDir, commit) {
		return "gate"
	}
	if objectExists(ctx, s.workDir(), commit) {
		return "worktree"
	}
	return ""
}

func (s *Service) homeDir(home string) string {
	if home == "gate" {
		return s.GateDir
	}
	return s.workDir()
}

func directCommitRef(ctx context.Context, dir, ref string) (string, error) {
	if _, err := git.Run(ctx, dir, "symbolic-ref", "-q", ref); err == nil {
		return "", fmt.Errorf("%s is symbolic", ref)
	}
	head, exists, err := git.ExactRefTarget(ctx, dir, ref)
	if err != nil || !exists {
		return "", err
	}
	commit, err := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || commit != head {
		return "", fmt.Errorf("%s is not a direct commit ref", ref)
	}
	return head, nil
}

// observeReconciliation covers the terminal post-push shape whose unpublished
// tail no ordinary recovery path can reach (issue #1523). The caller may stand
// at any clean head, including the run's own submitted or pushed head, and the
// tail may live in the local gate, only in the invoking worktree, or nowhere
// at all. A tail that is no Git object is observed as unrecoverable so the
// operator can still release the branch at a head that exists. Missing or
// conflicting evidence keeps the existing refusal.
func (s *Service) observeReconciliation(ctx context.Context, state State, run *db.Run) (reconciliationObservation, error) {
	o := reconciliationObservation{}
	if run == nil || !terminalRunStatus(run.Status) || run.CustodyReturnedAt != nil ||
		run.PushActive || value(run.PushGeneration) < 1 ||
		ptr(run.LastPushedSHA) == "" || ptr(run.SubmittedHeadSHA) == "" ||
		strings.TrimSpace(run.HeadSHA) == "" || run.HeadSHA == ptr(run.LastPushedSHA) ||
		!state.Local.Clean || strings.TrimSpace(s.GateDir) == "" {
		return o, fmt.Errorf("explicit reconciliation requires a terminal post-push run with an unpublished recorded head and a clean caller")
	}
	pipelineHome := s.commitHome(ctx, run.HeadSHA)
	o.PipelineRecoverable = pipelineHome != ""
	if o.PipelineRecoverable && run.TerminalHeadVerifiedAt == nil {
		return o, fmt.Errorf("the recorded pipeline head exists but was never verified as this run's terminal head")
	}
	repo, err := s.DB.GetRepo(s.Repo.ID)
	if err != nil || repo == nil || repo.PushURL() != s.Repo.PushURL() || repo.WorkingPath != s.Repo.WorkingPath ||
		ptr(run.PushTargetFingerprint) != TargetFingerprint(repo.PushURL()) ||
		ptr(run.PushTargetKind) != targetKind(repo) || ptr(run.PushRef) != "refs/heads/"+state.Local.Branch {
		return o, fmt.Errorf("the configured push target no longer matches the recorded push")
	}
	if duplicateBranchCheckout(ctx, s.workDir(), state.Local.Branch) {
		return o, fmt.Errorf("the branch is checked out in more than one worktree")
	}
	runs, err := s.DB.GetRunsByRepo(repo.ID)
	if err != nil {
		return o, err
	}
	for _, other := range runs {
		if other.Branch != run.Branch {
			continue
		}
		o.BranchRuns++
		if other.ID != run.ID && (!terminalRunStatus(other.Status) || unpublishedPipelineHead(other)) {
			return o, fmt.Errorf("another run holds unresolved work on this branch")
		}
	}
	gate, err := directCommitRef(ctx, s.GateDir, "refs/heads/"+run.Branch)
	if err != nil || gate == "" {
		return o, fmt.Errorf("the local gate does not hold this branch at a direct commit")
	}
	if s.commitHome(ctx, ptr(run.SubmittedHeadSHA)) == "" || s.commitHome(ctx, ptr(run.LastPushedSHA)) == "" {
		return o, fmt.Errorf("the submitted or published head is no longer present in this worktree or the local gate")
	}
	if o.PipelineRecoverable && !isAncestor(ctx, s.homeDir(pipelineHome), ptr(run.LastPushedSHA), run.HeadSHA) {
		return o, fmt.Errorf("the recorded pipeline head does not extend the published head")
	}
	archives, err := s.DB.GetRecoveryArchivesByRun(run.ID)
	if err != nil || len(archives) != 0 {
		return o, fmt.Errorf("bound archive recovery must use its existing proof")
	}
	o.RunID, o.Branch, o.Local = run.ID, run.Branch, state.Local.Head
	o.Repository = repo.ID
	o.Worktree, err = git.FindGitRoot(s.workDir())
	if err != nil {
		return o, err
	}
	o.Pipeline, o.Pushed, o.Gate, o.Submitted = run.HeadSHA, ptr(run.LastPushedSHA), gate, ptr(run.SubmittedHeadSHA)
	o.Target, o.Kind = TargetFingerprint(repo.PushURL()), targetKind(repo)
	o.Generation = value(run.PushGeneration)
	if run.TerminalHeadVerifiedAt != nil {
		o.VerifiedAt = *run.TerminalHeadVerifiedAt
	}
	// Ordered inputs make the token stable across independent status calls.
	for _, dir := range []string{s.workDir(), s.GateDir} {
		for _, ref := range []string{custody.RecoveryRef(run.ID), custody.RecoveryLocalRef(run.ID), custody.RecoveryGateRef(run.ID)} {
			head, err := directCommitRef(ctx, dir, ref)
			if err != nil || (ref == custody.RecoveryRef(run.ID) && head != "" && head != run.HeadSHA) {
				return o, fmt.Errorf("recovery evidence is unreadable or conflicting")
			}
			o.Refs = append(o.Refs, ref+"="+head)
		}
		for _, head := range o.observedHeads() {
			ref := reconciliationRef(run.ID, head)
			value, err := directCommitRef(ctx, dir, ref)
			if err != nil || (value != "" && value != head) {
				return o, fmt.Errorf("reconciliation preservation evidence conflicts")
			}
			o.Refs = append(o.Refs, ref+"="+value)
		}
	}
	return o, nil
}

func (s *Service) offerReconciliation(ctx context.Context, state *State, run *db.Run) {
	o, err := s.observeReconciliation(ctx, *state, run)
	if err != nil {
		return
	}
	keep, reason := "'<local|pipeline|pushed>'", "choose local, pipeline, or pushed explicitly with --keep"
	if !o.PipelineRecoverable {
		keep = "'<local|pushed>'"
		reason = "the recorded pipeline head " + o.Pipeline + " is no longer a Git object in this worktree or the local gate, so its content cannot be recovered; release the branch by choosing local or pushed explicitly with --keep"
	}
	state.NextAction = &NextAction{Code: "reconcile_custody", Command: "no-mistakes axi sync --recover --observed " + o.token() + " --keep " + keep}
	state.Error = reason + "; every observed head will remain under a recovery ref; a changed observation refuses reconciliation"
}

// Reconcile returns custody at an explicitly chosen head. It does not push,
// alter push provenance, infer containment, or credit validation to that head.
func (s *Service) Reconcile(ctx context.Context, keep, observed string) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	refuse := func(err error) State {
		return blockedPlan(state, state.State, "blocked_reconcile_assumptions_changed", err.Error()+"; custody was not returned")
	}
	if keep != "local" && keep != "pipeline" && keep != "pushed" {
		return refuse(fmt.Errorf("--keep requires exactly local, pipeline, or pushed"))
	}
	o, err := s.observeReconciliation(ctx, state, run)
	if err != nil {
		return refuse(err)
	}
	if keep == "pipeline" && !o.PipelineRecoverable {
		return refuse(fmt.Errorf("the recorded pipeline head %s is no longer a Git object in this worktree or the local gate", o.Pipeline))
	}
	if observed == "" || observed != o.token() {
		return refuse(fmt.Errorf("the observation is missing or stale; inspect status again before choosing a head"))
	}
	chosen := map[string]string{"local": o.Local, "pipeline": o.Pipeline, "pushed": o.Pushed}[keep]
	readCtx, cancel := context.WithTimeout(ctx, s.remoteTimeout())
	remote, err := s.runLsRemote(readCtx, s.workDir(), s.resolvedPushURL(ctx, s.Repo), "refs/heads/"+o.Branch)
	cancel()
	if err != nil || remote != o.Pushed {
		return refuse(fmt.Errorf("the live push target no longer equals the observed pushed head"))
	}
	// Stage immutable objects using local fetches, in whichever direction the
	// commit is missing. No receive hook runs and no branch or durable
	// recovery ref is changed by these imports.
	for _, head := range o.observedHeads() {
		if err := s.stageObservedCommit(ctx, o.Worktree, head); err != nil {
			return refuse(err)
		}
	}
	if s.beforeReconcile != nil {
		s.beforeReconcile()
	}
	fresh, freshRun, _ := s.inspect(ctx)
	again, err := s.observeReconciliation(ctx, fresh, freshRun)
	if err != nil || again.token() != observed {
		return refuse(fmt.Errorf("the observed heads or recovery evidence changed before reconciliation"))
	}
	err = s.DB.ReconcileCustody(run, s.Repo, o.BranchRuns, func() error {
		branch, err := git.CurrentBranch(ctx, s.workDir())
		local, localErr := directCommitRef(ctx, s.workDir(), "refs/heads/"+o.Branch)
		gate, gateErr := directCommitRef(ctx, s.GateDir, "refs/heads/"+o.Branch)
		clean, _ := worktreeClean(ctx, s.workDir())
		if err != nil || localErr != nil || gateErr != nil || branch != o.Branch || local != o.Local || gate != o.Gate || !clean {
			return fmt.Errorf("the local or gate branch changed before reconciliation")
		}
		for _, dir := range []string{s.workDir(), s.GateDir} {
			for ref, head := range o.anchors() {
				if err := custody.PreserveRecoveryAnchor(ctx, dir, ref, head); err != nil {
					return err
				}
			}
		}
		if s.beforeReconcileMove != nil {
			s.beforeReconcileMove()
		}
		// Once a branch moves, caller cancellation must not abort the worktree
		// update or its verification: that would roll back the custody stamp
		// after the move, and the moved heads no longer match a retry's
		// observation. Each step still fails closed, within a bound.
		moveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileMoveTimeout)
		defer cancel()
		return s.moveReconciledHeads(moveCtx, o, chosen)
	})
	if err != nil {
		return refuse(err)
	}
	state, _, _ = s.inspect(ctx)
	state.Recovered, state.Changed = true, chosen != o.Local
	state.Error = ""
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	return state
}

const reconcileMoveTimeout = 2 * time.Minute

func (s *Service) moveReconciledHeads(ctx context.Context, o reconciliationObservation, chosen string) error {
	ref := "refs/heads/" + o.Branch
	branch, err := git.CurrentBranch(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if err != nil || branch != o.Branch || !clean {
		return fmt.Errorf("the worktree changed before reconciliation")
	}
	if err := s.moveReconciliationRef(ctx, s.GateDir, o, chosen, o.Gate, true); err != nil {
		return fmt.Errorf("the gate branch moved before reconciliation: %w", err)
	}
	rollbackGate := func() {
		_, _ = git.Run(context.WithoutCancel(ctx), s.GateDir, "update-ref", "--no-deref", ref, o.Gate, chosen)
	}
	if err := s.moveReconciliationRef(ctx, s.workDir(), o, chosen, o.Local, false); err != nil {
		rollbackGate()
		return fmt.Errorf("the local branch moved before reconciliation: %w", err)
	}
	branch, err = git.CurrentBranch(ctx, s.workDir())
	if err == nil && branch == o.Branch && chosen != o.Local {
		_, err = git.Run(ctx, s.workDir(), "read-tree", "-m", "-u", o.Local, chosen)
	} else if err != nil || branch != o.Branch {
		err = fmt.Errorf("the checked-out branch changed during reconciliation")
	}
	if err != nil {
		_, restoreErr := git.Run(context.WithoutCancel(ctx), s.workDir(), "update-ref", "--no-deref", ref, o.Local, chosen)
		rollbackGate()
		if restoreErr != nil {
			return fmt.Errorf("%w; branch restoration also failed; observed heads remain preserved", err)
		}
		return err
	}
	head, err := git.HeadSHA(ctx, s.workDir())
	clean, _ = worktreeClean(ctx, s.workDir())
	gate, gateErr := directCommitRef(ctx, s.GateDir, ref)
	if err != nil || head != chosen || !clean || gateErr != nil || gate != chosen {
		return fmt.Errorf("the worktree changed during reconciliation; observed heads remain preserved")
	}
	return nil
}

// The ref transaction verifies preservation and the old branch head together;
// a branch move cannot succeed after an anchor is removed or replaced.
func (s *Service) moveReconciliationRef(ctx context.Context, dir string, o reconciliationObservation, chosen, old string, gate bool) error {
	refs := map[string]string{}
	observed := o.Refs[:len(o.Refs)/2]
	if gate {
		observed = o.Refs[len(o.Refs)/2:]
	}
	for _, entry := range observed {
		ref, head, _ := strings.Cut(entry, "=")
		refs[ref] = head
	}
	for ref, head := range o.anchors() {
		refs[ref] = head
	}
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	var input strings.Builder
	input.WriteString("start\n")
	for _, ref := range keys {
		head := refs[ref]
		current, err := directCommitRef(ctx, dir, ref)
		if err != nil || current != head {
			return fmt.Errorf("recovery ref changed before reconciliation: %s", ref)
		}
		if head == "" {
			head = strings.Repeat("0", len(old))
		}
		fmt.Fprintf(&input, "verify %s %s\n", ref, head)
	}
	branchHead, err := directCommitRef(ctx, dir, "refs/heads/"+o.Branch)
	if err != nil || branchHead != old {
		return fmt.Errorf("branch ref changed before reconciliation")
	}
	fmt.Fprintf(&input, "update refs/heads/%s %s %s\nprepare\ncommit\n", o.Branch, chosen, old)
	_, err = git.RunWithInput(ctx, dir, input.String(), "update-ref", "--no-deref", "--stdin")
	return err
}

// stageObservedCommit makes an observed commit present in both the invoking
// worktree and the local gate by importing it from whichever already holds it,
// so every observed head can be anchored in both repositories before the
// branch moves.
func (s *Service) stageObservedCommit(ctx context.Context, worktree, head string) error {
	inWorktree, inGate := objectExists(ctx, s.workDir(), head), objectExists(ctx, s.GateDir, head)
	switch {
	case inWorktree && inGate:
		return nil
	case inGate:
		if err := git.FetchRemoteRef(ctx, s.workDir(), s.GateDir, head, head); err != nil {
			return fmt.Errorf("could not import observed commit %s from the local gate: %w", head, err)
		}
	case inWorktree:
		if err := git.FetchRemoteRef(ctx, s.GateDir, worktree, head, head); err != nil {
			return fmt.Errorf("could not preserve observed commit %s in the local gate: %w", head, err)
		}
	default:
		return fmt.Errorf("observed commit %s is no longer present in this worktree or the local gate", head)
	}
	return nil
}
