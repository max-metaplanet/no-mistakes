package daemon

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDefaultSkipStepsNewRunAndRerun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		requested []types.StepName
		want      []types.StepName
	}{
		{"key alone", "default_skip_steps: [document]\n", nil, []types.StepName{types.StepDocument}},
		{"union", "default_skip_steps: [document]\n", []types.StepName{types.StepLint}, []types.StepName{types.StepDocument, types.StepLint}},
		{"duplicates", "default_skip_steps: [document, document]\n", []types.StepName{types.StepDocument}, []types.StepName{types.StepDocument}},
		{"empty", "default_skip_steps: []\n", []types.StepName{types.StepLint}, []types.StepName{types.StepLint}},
		{"absent", "{}\n", nil, nil},
		{"absent with flag", "{}\n", []types.StepName{types.StepLint}, []types.StepName{types.StepLint}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkDefaultSkipRuns(t, tc.yaml, tc.requested, tc.want)
		})
	}
}

func TestDefaultSkipStepsUnionProperties(t *testing.T) {
	const seed = 20261117
	t.Logf("seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < 12; i++ {
		var defaults, requested, want []types.StepName
		for _, step := range []types.StepName{types.StepDocument, types.StepLint, types.StepReview} {
			inDefaults, inRequest := rng.Intn(2) == 1, rng.Intn(2) == 1
			if inDefaults {
				defaults = append(defaults, step, step)
			}
			if inRequest {
				requested = append(requested, step, step)
			}
			if inDefaults || inRequest {
				want = append(want, step)
			}
		}
		t.Run(fmt.Sprintf("seed_%d_case_%d", seed, i), func(t *testing.T) {
			var names []string
			for _, step := range defaults {
				names = append(names, string(step))
			}
			checkDefaultSkipRuns(t, "default_skip_steps: ["+strings.Join(names, ", ")+"]\n", requested, want)
		})
	}
}

func checkDefaultSkipRuns(t *testing.T, configYAML string, requested, want []types.StepName) {
	t.Helper()
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, head := setupTestGitRepo(t, p, database, "default-skip")
	stepNames := []types.StepName{types.StepReview, types.StepDocument, types.StepLint}
	passSteps := make([]*mockPassStep, len(stepNames))
	var runSteps []pipeline.Step
	for i, name := range stepNames {
		passSteps[i] = &mockPassStep{name: name}
		runSteps = append(runSteps, passSteps[i])
	}
	manager := NewRunManager(database, p, func() []pipeline.Step { return runSteps })
	t.Cleanup(manager.Shutdown)
	for round := 0; round < 2; round++ {
		var runID string
		var err error
		if round == 0 {
			runID, err = manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "push", requested, "default skips", "", false, "", nil, nil)
		} else {
			runID, err = manager.HandleRerun(context.Background(), repo.ID, "main", "", requested, "", "", false, "", "", nil, nil)
		}
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
			t.Fatalf("round %d: status=%s error=%v", round, run.Status, run.Error)
		}
		rows, err := database.GetStepsByRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(stepNames) {
			t.Fatalf("round %d: got %d step rows, want %d", round, len(rows), len(stepNames))
		}
		for i, row := range rows {
			wantStatus, wantExecutions := types.StepStatusCompleted, int32(round+1)
			if slices.Contains(want, row.StepName) {
				wantStatus, wantExecutions = types.StepStatusSkipped, 0
			}
			if row.Status != wantStatus || passSteps[i].execCnt.Load() != wantExecutions {
				t.Fatalf("round %d step %s: status=%s executions=%d; want %s/%d; config=%q requested=%v", round, row.StepName, row.Status, passSteps[i].execCnt.Load(), wantStatus, wantExecutions, configYAML, requested)
			}
		}
	}
}

func TestDefaultSkipStepsConfigValidation(t *testing.T) {
	for _, value := range []string{
		"[document, unknown]", "[document, '']", "[document, null]",
		"['document,lint']", "[Document]", "[gate.review.custom]",
		"document", "{document: true}", "[123]",
	} {
		t.Run(value, func(t *testing.T) {
			cfg, err := config.LoadGlobalFromBytes([]byte("default_skip_steps: " + value + "\n"))
			if err == nil || cfg != nil {
				t.Fatalf("malformed default_skip_steps=%s: config=%v error=%v", value, cfg, err)
			}
		})
	}
	cfg, err := config.LoadGlobalFromBytes([]byte("default_skip_steps: [' document ']\n"))
	if err != nil || !slices.Equal(cfg.DefaultSkipSteps, []types.StepName{types.StepDocument}) {
		t.Fatalf("whitespace normalization: config=%v error=%v", cfg, err)
	}
}
