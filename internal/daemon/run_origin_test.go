package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runorigin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoveredRunOriginRetainsSourceAndRebuildsAgentEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX subprocess environment probe")
	}
	source := t.TempDir()
	origin := &runorigin.Submission{SourceCheckout: source, Caller: runorigin.Caller{Namespace: "fixture", Home: t.TempDir(), Task: "recovered-task", Generation: "original-generation"}}
	m, run, _ := gatePinFixture(t, oneGateYAML, nil, origin)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	recovered, err := m.db.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "recovered-env")
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$NO_MISTAKES_EXECUTION_REF\" \"$NO_MISTAKES_EXECUTION_SOCKET\" > " + shellQuoteForTest(capture) + "\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.paths.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+bin+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := m.prepareRecoveredRun(context.Background(), recovered)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.agent.Close()
	got, err := m.executionOrigin(context.Background(), ipc.GetExecutionOriginParams{Reference: runorigin.Reference(run.ID), ExecutionCheckout: plan.workDir})
	if err != nil || got.Execution == nil || got.Execution.SourceCheckout != source || *got.Execution.Caller != origin.Caller {
		t.Fatalf("recovered source changed: %+v %v", got, err)
	}
	if _, err := m.executionOrigin(context.Background(), ipc.GetExecutionOriginParams{Reference: runorigin.Reference(run.ID), ExecutionCheckout: t.TempDir()}); err == nil {
		t.Fatal("wrong execution checkout accepted")
	}
	if _, err := plan.agent.Run(context.Background(), agent.RunOpts{CWD: plan.workDir, Prompt: "probe", Env: []string{runorigin.ReferenceEnv + "=stale-turn", runorigin.SocketEnv + "=stale-socket"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != run.ID+"\n"+m.paths.Socket() {
		t.Fatalf("recovered agent lost pinned producer environment: %s", data)
	}
}

func TestRerunOriginPreservesSourceAndRejectsNewGeneration(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, head := setupTestGitRepo(t, p, d, "rerun-origin")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	origin := &runorigin.Submission{SourceCheckout: repo.WorkingPath, Caller: runorigin.Caller{Namespace: "fixture", Home: t.TempDir(), Task: "task", Generation: "accepted"}}
	var first ipc.PushReceivedResult
	if err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: head, New: head, CallerOrigin: origin}, &first); err != nil {
		t.Fatal(err)
	}
	original := waitForRunTerminalState(t, d, first.RunID)
	var rerun ipc.RerunResult
	if err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	continued := waitForRunTerminalState(t, d, rerun.RunID)
	if continued.CallerOrigin == nil || continued.CallerOrigin.Submission != *origin || continued.CallerOrigin.SourceHead != head || continued.WorktreePath() == original.WorktreePath() {
		t.Fatalf("rerun lost source or execution association: %+v", continued.CallerOrigin)
	}
	conflict := *origin
	conflict.Caller.Generation = "new-generation"
	if err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main", CallerOrigin: &conflict}, &rerun); err == nil {
		t.Fatal("rerun silently changed generation")
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("conflicting rerun created work: runs=%d err=%v", len(runs), err)
	}
}
