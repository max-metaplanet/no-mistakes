package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runorigin"
)

func TestRunOriginIsImmutableAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a/b.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	origin := &runorigin.Origin{Submission: runorigin.Submission{SourceCheckout: t.TempDir(), Caller: runorigin.Caller{Namespace: "fixture", Home: t.TempDir(), Task: "task-a", Generation: "generation-a"}}, SourceHead: "original-source"}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "submitted", "base", nil, "", "", "", "", false, nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	origin.Submission.Caller.Generation = "generation-b"
	if run.CallerOrigin.Submission.Caller.Generation != "generation-a" {
		t.Fatal("caller mutated the accepted origin")
	}
	if err = d.UpdateRunHeadSHA(run.ID, "pipeline-fix"); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CallerOrigin == nil || got.CallerOrigin.Submission.Caller.Generation != "generation-a" || got.CallerOrigin.SourceHead != "original-source" || got.WorktreeDir != nil {
		t.Fatalf("lost immutable origin before execution setup: %+v", got.CallerOrigin)
	}
	legacy, err := d.InsertRun(repo.ID, "legacy", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err = d.GetRun(legacy.ID)
	if err != nil || legacy.CallerOrigin != nil {
		t.Fatalf("fabricated legacy origin: %+v %v", legacy, err)
	}
	if _, err = d.sql.Exec(`UPDATE runs SET caller_origin='{"submission":false}' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.GetRun(run.ID); err == nil {
		t.Fatal("malformed persisted origin passed")
	}
}

func TestOriginConflictCannotConsumeLaunchReceipt(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a/b.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	origin := &runorigin.Submission{SourceCheckout: t.TempDir(), Caller: runorigin.Caller{Namespace: "fixture", Home: t.TempDir(), Task: "task", Generation: "accepted"}}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, nil, &runorigin.Origin{Submission: *origin, SourceHead: "head"})
	if err != nil {
		t.Fatal(err)
	}
	conflict := *origin
	conflict.Caller.Generation = "other"
	got, claimed, err := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, &conflict)
	if err != nil || claimed || got.LaunchReceiptClaimedAt != nil {
		t.Fatalf("conflict consumed receipt: %+v claimed=%v err=%v", got, claimed, err)
	}
	got, claimed, err = d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, origin)
	if err != nil || !claimed || got.CallerOrigin.Submission != *origin || got.ID != run.ID {
		t.Fatalf("exact origin could not claim: %+v claimed=%v err=%v", got, claimed, err)
	}
}
