package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/runorigin"
)

func (m *RunManager) executionOrigin(ctx context.Context, p ipc.GetExecutionOriginParams) (*ipc.GetExecutionOriginResult, error) {
	result := &ipc.GetExecutionOriginResult{ProtocolVersion: 1}
	if p.Reference == "" && p.ExecutionCheckout == "" {
		return result, nil
	}
	if err := p.Reference.Validate(); err != nil {
		return nil, err
	}
	run, err := m.db.GetRun(string(p.Reference))
	if err != nil {
		return nil, err
	}
	if run == nil || run.CallerOrigin == nil {
		return nil, fmt.Errorf("execution origin is absent for this reference")
	}
	r := run.CallerOrigin
	requested, err := filepath.EvalSymlinks(p.ExecutionCheckout)
	if err != nil {
		return nil, fmt.Errorf("resolve execution checkout: %w", err)
	}
	recorded := run.WorktreePath()
	expectedCommon := m.paths.RepoDir(run.RepoID)
	primary, err := filepath.EvalSymlinks(recorded)
	if err != nil || requested != primary {
		expectedCommon, err = m.db.AuxiliaryRunCheckout(run.ID, requested)
		if err != nil {
			return nil, err
		}
		recorded = requested
	}
	top, err := git.Run(ctx, requested, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(top, recorded) {
		return nil, fmt.Errorf("execution reference requires the exact run checkout")
	}
	common, err := git.Run(ctx, requested, "rev-parse", "--git-common-dir")
	if err != nil || !samePath(resolveGitPath(requested, common), expectedCommon) {
		return nil, fmt.Errorf("execution checkout no longer belongs to the run gate")
	}
	result.Execution = &runorigin.Execution{Reference: p.Reference, RunID: run.ID, SourceCheckout: r.Submission.SourceCheckout, SourceHead: r.SourceHead, ExecutionCheckout: recorded, Caller: &r.Submission.Caller, Status: string(run.Status)}
	return result, nil
}

func originEnvironment(run *db.Run, socket string, base runenv.Overlay) runenv.Overlay {
	env := base.Clone()
	delete(env.Set, runorigin.ReferenceEnv)
	delete(env.Set, runorigin.SocketEnv)
	env.Unset = append(env.Unset, runorigin.ReferenceEnv, runorigin.SocketEnv)
	if run.CallerOrigin != nil {
		if env.Set == nil {
			env.Set = map[string]string{}
		}
		env.Set[runorigin.ReferenceEnv] = run.ID
		env.Set[runorigin.SocketEnv] = socket
	}
	return env
}

func (m *RunManager) inheritedOrigin(run *db.Run, requested *runorigin.Submission) (*runorigin.Submission, string, error) {
	r := run.CallerOrigin
	if r == nil {
		return requested, "", nil
	}
	if requested != nil && *requested != r.Submission {
		return nil, "", fmt.Errorf("caller origin conflicts with the selected run; start a fresh launch instead of rebinding a continuation")
	}
	return &r.Submission, r.SourceHead, nil
}
