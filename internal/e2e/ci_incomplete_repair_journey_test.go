//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciIncompleteJourney stands up a real daemon whose PR and CI go through the
// reference provider plugin, with one red "build" check that never turns
// green. The CI step parks the failure (auto_fix.ci is 0 in the harness), the
// test answers it with `fix`, and the fake agent's CI repair edits repairPath
// and reports repairComplete. It returns the harness, the branch, and the head
// the run published before the repair.
type ciIncompleteJourney struct {
	h             *Harness
	branch        string
	publishedHead string
	statePath     string
}

const ciIncompleteReason = "merged main and renamed the migration, but the snapshot restore, the guards, the type-check and the tests never ran; do not publish"

func startCIIncompleteJourney(t *testing.T, branch, protectedPath, repairComplete, trustedExtra string) *ciIncompleteJourney {
	t.Helper()
	const (
		pluginHost = "ssm.example.com"
		repoPath   = "team/repo"
		remoteURL  = "https://" + pluginHost + "/" + repoPath + ".git"
	)
	protectedEdit := ""
	if protectedPath != "" {
		protectedEdit = "      - path: \"" + protectedPath + "\"\n        new: \"renamed migration\\n\"\n"
	}
	scenarioPath := filepath.Join(t.TempDir(), "scenario.yaml")
	scenario := `actions:
  - match: "The following CI checks have failed on this PR. Diagnose and fix the issues.\n\nContext:\n- branch: ` + branch + `"
    text: "ci repair"
    edits:
      - path: "fix.txt"
        new: "repaired by the CI agent\n"
` + protectedEdit + `    structured:
      summary: "` + ciIncompleteReason + `"
      code_change_needed: true
      repair_complete: ` + repairComplete + `
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(scenarioPath, []byte(scenario), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: scenarioPath,
		GlobalConfigExtra: "provider_plugins:\n" +
			"  ssm:\n" +
			"    command: " + fakeplugin.ExecutableName + "\n" +
			"    hosts: [" + pluginHost + "]\n" +
			"    timeout: 30s\n",
	})
	if trustedExtra != "" {
		commitTrustedRepoConfig(t, h, trustedExtra)
	}
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set plugin-host origin: %v\n%s", err, out)
	}
	stateDir := filepath.Dir(h.AgentLog)
	statePath := filepath.Join(stateDir, "provider-plugin-state.json")
	if err := fakeplugin.Save(statePath, fakeplugin.State{
		Checks:     []fakeplugin.Check{{Name: "build", Bucket: "fail", State: "FAILURE"}},
		FailedLogs: "build: TestMigrationSnapshot failed\n",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeplugin.EnvState, statePath)
	t.Setenv(fakeplugin.EnvLog, filepath.Join(stateDir, "provider-plugin-requests.ndjson"))

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
	h.PushToGate(branch)

	run := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusAwaitingApproval, 150*time.Second)
	published := h.UpstreamBranchSHA(branch)
	if published != run.HeadSHA {
		t.Fatalf("pre-repair published head = %s, want run head %s", published, run.HeadSHA)
	}
	ids := ciStepFindingIDs(t, run)
	if len(ids) == 0 {
		t.Fatalf("CI gate parked without findings")
	}
	t.Logf("CI gate before repair parks on %v at published head %s", ids, published)
	h.RespondWithFindings(run.ID, types.StepCI, types.ActionFix, ids)
	return &ciIncompleteJourney{h: h, branch: branch, publishedHead: published, statePath: statePath}
}

func ciStepFindings(t *testing.T, run *ipc.RunInfo) []types.Finding {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepCI)
	if !ok || step.FindingsJSON == nil {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse CI findings: %v", err)
	}
	return findings.Items
}

func ciStepFindingIDs(t *testing.T, run *ipc.RunInfo) []string {
	var ids []string
	for _, f := range ciStepFindings(t, run) {
		ids = append(ids, f.ID)
	}
	return ids
}

// waitForCIGate waits until the CI step parks again with a finding set that
// satisfies match, so a test does not mistake the gate it just answered for
// the next one.
func waitForCIGate(t *testing.T, h *Harness, branch string, timeout time.Duration, match func([]types.Finding) bool) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "no active run"
	for time.Now().Before(deadline) {
		if run := h.ActiveRun(branch); run != nil {
			step, ok := findStep(run.Steps, types.StepCI)
			if ok && (step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview) && match(ciStepFindings(t, run)) {
				return run
			}
			last = fmt.Sprintf("run %s status=%s ci=%s findings=%v", run.ID, run.Status, step.Status, ciStepFindingIDs(t, run))
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("CI gate for %s did not reach the expected findings in %v; last: %s", branch, timeout, last)
	return nil
}

func hasFinding(findings []types.Finding, id string) (types.Finding, bool) {
	for _, f := range findings {
		if f.ID == id {
			return f, true
		}
	}
	return types.Finding{}, false
}

func pushedHeadsSince(t *testing.T, h *Harness, branch, from string) string {
	t.Helper()
	out, err := h.runGit(t.Context(), h.UpstreamDir, "log", "--format=%h %s", from+".."+branch)
	if err != nil {
		t.Fatalf("upstream log: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func logAxiStatus(t *testing.T, h *Harness, branch string) {
	t.Helper()
	wt := h.AddWorktree(branch)
	defer h.RemoveWorktree(wt)
	out, err := h.RunInDir(wt, "axi", "status")
	t.Logf("axi status (err=%v):\n%s", err, out)
}

// TestCIIncompleteRepairJourney_IsHeldAtAGate reproduces the incident: the CI
// repair agent commits a change but reports the repair incomplete. The daemon
// must not push it; the run parks at an ask-user gate carrying the agent's
// reason.
func TestCIIncompleteRepairJourney_IsHeldAtAGate(t *testing.T) {
	j := startCIIncompleteJourney(t, "feature/ci-incomplete", "", "false", "")
	run := waitForCIGate(t, j.h, j.branch, 150*time.Second, func(f []types.Finding) bool {
		_, ok := hasFinding(f, "ci-repair-incomplete")
		return ok
	})
	f, _ := hasFinding(ciStepFindings(t, run), "ci-repair-incomplete")
	if f.Action != types.ActionAskUser {
		t.Fatalf("incomplete-repair finding action = %q, want ask-user", f.Action)
	}
	if !strings.Contains(f.Description, ciIncompleteReason) {
		t.Fatalf("gate does not carry the agent's reason:\n%s", f.Description)
	}
	if got := j.h.UpstreamBranchSHA(j.branch); got != j.publishedHead {
		t.Fatalf("upstream head moved to %s (pushed: %s); want it held at %s", got, pushedHeadsSince(t, j.h, j.branch, j.publishedHead), j.publishedHead)
	}
	if run.HeadSHA == j.publishedHead {
		t.Fatalf("run head did not record the held repair commit")
	}
	logAxiStatus(t, j.h, j.branch)
	t.Logf("held repair: run head %s, upstream still %s; gate finding:\n%s", run.HeadSHA, j.publishedHead, f.Description)
}

// TestCIIncompleteRepairJourney_CompleteRepairPublishes is the control: the
// same repair reported complete is pushed as before.
func TestCIIncompleteRepairJourney_CompleteRepairPublishes(t *testing.T) {
	j := startCIIncompleteJourney(t, "feature/ci-complete", "", "true", "")
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		if got := j.h.UpstreamBranchSHA(j.branch); got != j.publishedHead {
			if run := j.h.ActiveRun(j.branch); run != nil {
				if _, ok := hasFinding(ciStepFindings(t, run), "ci-repair-incomplete"); ok {
					t.Fatalf("a complete repair parked as incomplete")
				}
			}
			t.Logf("complete repair published: upstream %s -> %s\n%s", j.publishedHead, got, pushedHeadsSince(t, j.h, j.branch, j.publishedHead))
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	j.h.dumpDebugState()
	t.Fatalf("complete repair was never published")
}

// TestCIIncompleteRepairJourney_ProtectedPathApprovalStillHolds is the
// adversarial path: the unfinished repair also touches a protected path, so it
// first parks at the protected-path gate. Answering that gate with fix must not
// publish; the run lands at the incomplete-repair gate.
func TestCIIncompleteRepairJourney_ProtectedPathApprovalStillHolds(t *testing.T) {
	j := startCIIncompleteJourney(t, "feature/ci-incomplete-protected", "db/migrations/0002_rename.sql", "false", "protected_paths:\n  - \"db/migrations/**\"\n")
	refusal := waitForCIGate(t, j.h, j.branch, 150*time.Second, func(f []types.Finding) bool {
		_, protected := hasFinding(f, "protected-path-refusal")
		_, incomplete := hasFinding(f, "ci-repair-incomplete")
		return protected && incomplete
	})
	ids := ciStepFindingIDs(t, refusal)
	t.Logf("protected-path gate findings: %v", ids)
	for _, f := range ciStepFindings(t, refusal) {
		t.Logf("  %s [%s] %s", f.ID, f.Action, f.Description)
	}
	if got := j.h.UpstreamBranchSHA(j.branch); got != j.publishedHead {
		t.Fatalf("upstream moved at the protected-path gate: %s", got)
	}
	// The operator resolves the protected edit (here: discards the migration
	// rename in the run worktree) and retries with fix, as the protected-path
	// gate instructs.
	protectedFile := filepath.Join(paths.WithRoot(j.h.NMHome).WorktreeDir(j.h.repoID(), refusal.ID), "db/migrations/0002_rename.sql")
	if err := os.Remove(protectedFile); err != nil {
		t.Fatalf("resolve protected edit: %v", err)
	}
	j.h.RespondWithFindings(refusal.ID, types.StepCI, types.ActionFix, ids)

	held := waitForCIGate(t, j.h, j.branch, 150*time.Second, func(f []types.Finding) bool {
		_, protected := hasFinding(f, "protected-path-refusal")
		_, incomplete := hasFinding(f, "ci-repair-incomplete")
		return incomplete && !protected
	})
	if got := j.h.UpstreamBranchSHA(j.branch); got != j.publishedHead {
		t.Fatalf("approving the protected path published the unfinished repair: upstream %s (pushed: %s)", got, pushedHeadsSince(t, j.h, j.branch, j.publishedHead))
	}
	if held.HeadSHA == j.publishedHead {
		t.Fatalf("retry did not commit the retained repair locally")
	}
	logAxiStatus(t, j.h, j.branch)
	t.Logf("after protected-path fix: run head %s held locally, upstream still %s, gate %v", held.HeadSHA, j.publishedHead, ciStepFindingIDs(t, held))
}
