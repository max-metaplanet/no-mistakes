//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runorigin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunOriginCLIToBothProcessBoundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX boundary probe scripts; adapter and storage coverage is portable")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	probeLog := filepath.Join(h.HomeDir, "origin-probes.ndjson")
	release := filepath.Join(h.HomeDir, "release")
	t.Setenv(runorigin.ReferenceEnv, "stale-ambient-reference")
	t.Setenv(runorigin.SocketEnv, "stale-ambient-socket")
	t.Setenv("FM_TASK_ID", "stale-ambient-task")
	probe := filepath.Join(h.BinDir, "nm-origin-probe")
	writeMachineLocalScript(t, probe, `set -eu
result=$(`+shellQuote(h.NMBin)+` axi execution-origin)
printf '{"boundary":"%s","result":%s}\n' "$1" "$result" >> `+shellQuote(probeLog)+`
if [ "$1" = configured ]; then
  while [ ! -f `+shellQuote(release)+` ]; do sleep 0.1; done
fi
`)
	realDir := filepath.Join(h.BinDir, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	realAgent := filepath.Join(realDir, "claude")
	if err := os.Symlink(h.FakeAgent, realAgent); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(h.BinDir, "claude")
	if err := os.Remove(agentPath); err != nil {
		t.Fatal(err)
	}
	writeMachineLocalScript(t, agentPath, shellQuote(probe)+" agent\nexec "+shellQuote(realAgent)+" \"$@\"\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branches := []string{"feature/origin-a", "feature/origin-b"}
	origins := map[string]runorigin.Caller{}
	for i, branch := range branches {
		h.CommitChange(branch, ".no-mistakes.yaml", "commands:\n  test: nm-origin-probe configured\n", "configure process probe")
		h.CommitChange(branch, fmt.Sprintf("feature-%d.txt", i), "change\n", "add feature")
		origin := runorigin.Caller{Namespace: "fixture.firstmate", Home: h.HomeDir, Task: fmt.Sprintf("task-%d", i), Generation: fmt.Sprintf("generation-%d", i)}
		origins[branch] = origin
		data, _ := json.Marshal(origin)
		path := filepath.Join(h.HomeDir, fmt.Sprintf("origin-%d.json", i))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		out, err := h.Run("axi", "run", "--intent", "Prove accepted caller origin reaches real child processes", "--caller-origin-file", path, "--wait", "1s")
		if err != nil && !strings.Contains(out, "elapsed") {
			t.Fatalf("submit: %v\n%s", err, out)
		}
	}
	type observation struct {
		Boundary string                       `json:"boundary"`
		Result   ipc.GetExecutionOriginResult `json:"result"`
	}
	var observed []observation
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(probeLog)
		observed = nil
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var o observation
			if json.Unmarshal([]byte(line), &o) == nil {
				observed = append(observed, o)
			}
		}
		commands := 0
		for _, o := range observed {
			if o.Boundary == "configured" {
				commands++
			}
		}
		if commands >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	found := map[string]map[string]bool{}
	refs := map[string]runorigin.Reference{}
	for _, o := range observed {
		e := o.Result.Execution
		if o.Result.ProtocolVersion != 1 || e == nil || e.Caller == nil {
			t.Fatalf("missing producer binding at %s: %+v", o.Boundary, o.Result)
		}
		if e.Caller.Task == "stale-ambient-task" || e.SourceCheckout != h.WorkDir || e.ExecutionCheckout == e.SourceCheckout {
			t.Fatalf("wrong source/execution provenance: %+v", e)
		}
		t.Logf("boundary=%s run=%s namespace=%s task=%s generation=%s", o.Boundary, e.RunID, e.Caller.Namespace, e.Caller.Task, e.Caller.Generation)
		run := h.RunInfo(e.RunID)
		want := origins[run.Branch]
		if *e.Caller != want {
			t.Fatalf("cross-run caller leak: got %+v want %+v", e.Caller, want)
		}
		if found[run.Branch] == nil {
			found[run.Branch] = map[string]bool{}
		}
		found[run.Branch][o.Boundary] = true
		refs[run.Branch] = e.Reference
	}
	for _, branch := range branches {
		if !found[branch]["agent"] || !found[branch]["configured"] {
			t.Fatalf("missing real boundary for %s: %+v", branch, found)
		}
	}
	if refs[branches[0]] == refs[branches[1]] {
		t.Fatal("independent runs shared a reference")
	}
	for _, branch := range branches {
		run := h.ActiveRun(branch)
		if run == nil || run.Status != types.RunRunning {
			t.Fatalf("runs did not overlap: %+v", run)
		}
		out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{runorigin.ReferenceEnv: string(refs[branch]), runorigin.SocketEnv: filepath.Join(h.NMHome, "socket")}, "axi", "execution-origin")
		if err == nil {
			t.Fatalf("source checkout accepted as execution checkout: %s", out)
		}
	}
	if err := os.WriteFile(release, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, branch := range branches {
		if run := h.WaitForRun(branch, 90*time.Second); run.Status != types.RunCompleted {
			t.Fatalf("run failed: %+v", run)
		}
	}
	beforeLegacy, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatal(err)
	}
	h.CommitChange("feature/operator-origin", ".no-mistakes.yaml", "commands:\n  test: nm-origin-probe configured\n", "configure operator probe")
	h.CommitChange("feature/operator-origin", "operator.txt", "change\n", "add operator change")
	if out, err := h.Run("axi", "run", "--intent", "Keep an operator run explicitly unattributed"); err != nil {
		t.Fatalf("operator launch: %v %s", err, out)
	}
	afterLegacy, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatal(err)
	}
	legacyRows := strings.Split(strings.TrimSpace(string(afterLegacy[len(beforeLegacy):])), "\n")
	if len(legacyRows) < 2 {
		t.Fatal("operator run did not reach both process boundaries")
	}
	for _, line := range legacyRows {
		var o observation
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			t.Fatal(err)
		}
		if o.Result.Execution != nil {
			t.Fatalf("operator process inherited attribution: %s", line)
		}
	}
	out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{runorigin.ReferenceEnv: "", runorigin.SocketEnv: ""}, "axi", "execution-origin")
	if err != nil || !strings.Contains(out, `"execution":null`) {
		t.Fatalf("unattributed query: %v %s", err, out)
	}
}

func TestRunOriginBaseCheckoutUsesRecordedAssociation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command probe")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	log := filepath.Join(h.HomeDir, "base-origin.ndjson")
	writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-origin-base"), `set -eu
`+shellQuote(h.NMBin)+` axi execution-origin >> `+shellQuote(log)+`
echo '--- FAIL: TestFixture (0.01s)'
printf 'FAIL\tfixture/package\t0.1s\n'
exit 1
`)
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\ncommands:\n  test: nm-origin-base\ntest:\n  base_attribution: true\n", "configure origin probe")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("fixture upstream: %v %s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	h.CommitChange("feature/base-origin", "feature.txt", "change\n", "add change")
	origin := runorigin.Caller{Namespace: "fixture", Home: h.HomeDir, Task: "base-task", Generation: "base-generation"}
	data, _ := json.Marshal(origin)
	path := filepath.Join(h.HomeDir, "origin.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("axi", "run", "--intent", "Prove base configured-command checkout origin", "--caller-origin-file", path); err != nil {
		t.Fatalf("launch: %v %s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 2 {
		t.Fatalf("head and base did not both query origin: %s", data)
	}
	var head, base ipc.GetExecutionOriginResult
	if err = json.Unmarshal([]byte(rows[0]), &head); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(rows[1]), &base); err != nil {
		t.Fatal(err)
	}
	if head.Execution == nil || base.Execution == nil || head.Execution.Reference != base.Execution.Reference || head.Execution.ExecutionCheckout == base.Execution.ExecutionCheckout || *base.Execution.Caller != origin || base.Execution.SourceCheckout != h.WorkDir {
		t.Fatalf("wrong base association: head=%+v base=%+v", head.Execution, base.Execution)
	}
	t.Logf("CLI submission reached head and base command checkouts with one frozen task generation")
}

func TestRunOriginRefusesStalePinnedHookBeforeSubmission(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	h.CommitChange("feature/stale-origin-hook", "feature.txt", "change\n", "add change")
	origin := runorigin.Caller{Namespace: "fixture", Home: h.HomeDir, Task: "task", Generation: "generation"}
	data, _ := json.Marshal(origin)
	path := filepath.Join(h.HomeDir, "origin.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(h.NMHome, "repos", h.repoID()+".git", "hooks", "post-receive")
	data, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(hook, []byte(strings.Replace(string(data), "# caller-origin protocol v1\n", "", 1)), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := h.Run("axi", "run", "--intent", "Refuse an incompatible pinned helper before mutation", "--caller-origin-file", path)
	if err == nil || !strings.Contains(out, "post-receive hook pinned to this executable") {
		t.Fatalf("stale helper accepted: %v %s", err, out)
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("incompatible helper started runs: %+v", runs)
	}
}

func TestRunOriginNonceReplayAndUpToDateFallback(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	h.CommitChange("feature/origin-replay", "feature.txt", "change\n", "add change")
	origin := runorigin.Caller{Namespace: "fixture", Home: h.HomeDir, Task: "replay-task", Generation: "original-generation"}
	data, _ := json.Marshal(origin)
	path := filepath.Join(h.HomeDir, "origin.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"axi", "run", "--intent", "Keep the exact caller across proof retries", "--caller-origin-file", path, "--launch-nonce", "origin-first", "--validation-generation", "fixture-generation"}
	if out, err := h.Run(args...); err != nil {
		t.Fatalf("initial: %v %s", err, out)
	}
	if out, err := h.Run(args...); err != nil {
		t.Fatalf("same origin replay: %v %s", err, out)
	}
	if runs := h.Runs(); len(runs) != 1 {
		t.Fatalf("replay created a new run: %d", len(runs))
	}
	args[len(args)-3] = "origin-next"
	if out, err := h.Run(args...); err != nil {
		t.Fatalf("up-to-date fallback: %v %s", err, out)
	}
	runs := h.Runs()
	if len(runs) != 2 || runs[0].OriginReference == nil || runs[1].OriginReference == nil || *runs[0].OriginReference == *runs[1].OriginReference {
		t.Fatalf("fallback lost distinct immutable origin association: %+v", runs)
	}
	origin.Generation = "rebound-generation"
	data, _ = json.Marshal(origin)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run(args...); err == nil || !strings.Contains(out, "caller origin differs") {
		t.Fatalf("conflicting replay accepted: %v %s", err, out)
	}
}

func TestRunOriginRecordsTheSubmittingLinkedCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command probe")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	proof := filepath.Join(h.HomeDir, "linked-origin.json")
	writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-linked-origin"), shellQuote(h.NMBin)+" axi execution-origin > "+shellQuote(proof)+"\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	source := filepath.Join(h.HomeDir, "submitting-checkout")
	if out, err := h.runGit(context.Background(), h.WorkDir, "worktree", "add", "-b", "feature/linked-origin", source, "main"); err != nil {
		t.Fatalf("fixture linked checkout: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(source, ".no-mistakes.yaml"), []byte("commands:\n  test: nm-linked-origin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(context.Background(), source, "add", ".no-mistakes.yaml"); err != nil {
		t.Fatalf("fixture add: %v %s", err, out)
	}
	if out, err := h.runGit(context.Background(), source, "commit", "-m", "configure linked origin probe"); err != nil {
		t.Fatalf("fixture commit: %v %s", err, out)
	}
	origin := runorigin.Caller{Namespace: "fixture", Home: h.HomeDir, Task: "linked-task", Generation: "linked-generation"}
	data, _ := json.Marshal(origin)
	path := filepath.Join(h.HomeDir, "origin.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.RunInDir(source, "axi", "run", "--intent", "Capture the actual submitting checkout", "--caller-origin-file", path); err != nil {
		t.Fatalf("linked launch: %v %s", err, out)
	}
	data, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	var result ipc.GetExecutionOriginResult
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution == nil || result.Execution.SourceCheckout != source || result.Execution.SourceCheckout == h.WorkDir || *result.Execution.Caller != origin {
		t.Fatalf("registered checkout replaced actual source: %+v", result.Execution)
	}
}
