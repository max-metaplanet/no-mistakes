package db

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReconcileCustodyFencesChangesBeforeGitMutation(t *testing.T) {
	for _, changed := range []string{"head", "push", "target", "new-run", "status", "custody", "archive"} {
		t.Run(changed, func(t *testing.T) {
			d, repo, run := reconciliationDBFixture(t)
			var err error
			switch changed {
			case "head":
				err = d.UpdateRunHeadSHA(run.ID, "moved")
			case "push":
				err = d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/topic"})
			case "target":
				_, err = d.UpdateRepoForkURL(repo.ID, "https://example.invalid/fork.git")
			case "new-run":
				_, err = d.InsertRun(repo.ID, run.Branch, "new", "base")
			case "status":
				err = d.UpdateRunStatus(run.ID, types.RunRunning)
			case "custody":
				err = d.SetRunCustodyReturned(run.ID)
			case "archive":
				_, err = d.RecordRecoveryArchive(RecoveryArchive{OwnerRunID: run.ID, RunID: run.ID, RepoID: repo.ID, Branch: run.Branch, RequiredHeadSHA: "local", PreservedHeadSHA: "pipeline", ArchiveRef: "refs/heads/archive/recovery"})
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			err = d.ReconcileCustody(run, repo, 1, func() error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("stale recovery reached Git mutation: called=%v, err=%v", called, err)
			}
		})
	}
}

func TestReconcileCustodyStampsOnlySuccessfulMutation(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		d, repo, run := reconciliationDBFixture(t)
		err := d.ReconcileCustody(run, repo, 1, func() error {
			if !succeed {
				return errors.New("ref changed")
			}
			return nil
		})
		stored, readErr := d.GetRun(run.ID)
		if readErr != nil || (err == nil) != succeed || (stored.CustodyReturnedAt != nil) != succeed {
			t.Fatalf("success=%v: run=%#v, err=%v, read=%v", succeed, stored, err, readErr)
		}
	}
}

func reconciliationDBFixture(t *testing.T) (*DB, *Repo, *Run) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "work"), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "topic", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/topic"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, "pipeline"); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, repo, run
}

// TestReconcileCustodyReleasesAnUnverifiedTerminalHead covers issue #1523's
// stranded run whose recorded head was never verified and is no Git object
// anywhere: it must still be releasable, and a stamp that lands after the
// observation must still refuse.
func TestReconcileCustodyReleasesAnUnverifiedTerminalHead(t *testing.T) {
	for _, stamped := range []bool{false, true} {
		d, repo, run := unverifiedReconciliationDBFixture(t)
		if stamped {
			if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, "pipeline"); err != nil {
				t.Fatal(err)
			}
		}
		called := false
		err := d.ReconcileCustody(run, repo, 1, func() error { called = true; return nil })
		stored, readErr := d.GetRun(run.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if stamped {
			if err == nil || called || stored.CustodyReturnedAt != nil {
				t.Fatalf("a stamp after the observation was accepted: called=%v, err=%v", called, err)
			}
			continue
		}
		if err != nil || !called || stored.CustodyReturnedAt == nil {
			t.Fatalf("unverified terminal head was not released: called=%v, err=%v, run=%#v", called, err, stored)
		}
	}
}

func unverifiedReconciliationDBFixture(t *testing.T) (*DB, *Repo, *Run) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "work"), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "topic", "submitted", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "pushed", TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/topic"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "pipeline"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d, repo, run
}
