package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runorigin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCallerOriginPushTransportIsLiteralAndRejectsConflicts(t *testing.T) {
	origin := &runorigin.Submission{SourceCheckout: t.TempDir(), Caller: runorigin.Caller{Namespace: "fixture", Home: filepath.Clean(t.TempDir()), Task: "literal ' $() `task`", Generation: "generation"}}
	options := formatCallerOriginPushOptions(origin)
	got, err := parseCallerOriginPushOptions(options)
	if err != nil || !reflect.DeepEqual(got, origin) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range [][]string{{originPushPrefix + "bad encoding"}, {options[0], options[0]}, {originPushPrefix + "e30"}} {
		if _, err := parseCallerOriginPushOptions(bad); err == nil {
			t.Fatalf("accepted conflicting or malformed wire: %v", bad)
		}
	}
}

func TestSelectedRunOriginRejectsDifferentOriginsAtTheSameHead(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	origin := runorigin.Submission{SourceCheckout: t.TempDir(), Caller: runorigin.Caller{
		Namespace: "firstmate", Home: t.TempDir(), Task: "task-a", Generation: "spawn-a",
	}}
	repo, err := d.InsertRepo(origin.SourceCheckout, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "main", head, "", nil, "", "", "", "", false, nil,
		&runorigin.Origin{Submission: origin, SourceHead: head})
	if err != nil {
		t.Fatal(err)
	}
	ref := runorigin.Reference(run.ID)
	info := ipc.RunInfo{ID: run.ID, RepoID: repo.ID, Branch: "main", HeadSHA: head, Status: types.RunRunning, OriginReference: &ref}
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "completed-before-poll"}[terminal], func(t *testing.T) {
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
				if terminal {
					return &ipc.GetActiveRunResult{}, nil
				}
				return &ipc.GetActiveRunResult{Run: &info}, nil
			})
			srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
				completed := info
				completed.Status = types.RunCompleted
				return &ipc.GetRunsResult{Runs: []ipc.RunInfo{completed}}, nil
			})
			p := paths.WithRoot(makeSocketSafeTempDir(t))
			startIPCServer(t, srv, p.Socket())
			client := dialReady(t, p.Socket())
			defer client.Close()
			selected, err := waitForTriggeredRunForHead(context.Background(), client, repo.ID, "main", head, map[string]struct{}{}, time.Second)
			if err != nil || selected == nil || selected.ID != run.ID {
				t.Fatalf("select same-head run: %+v, %v", selected, err)
			}
			if err := acceptSelectedRunOrigin(d, selected.ID, &origin); err != nil {
				t.Fatalf("matching frozen submission refused: %v", err)
			}
			for _, field := range []string{"source_checkout", "namespace", "home", "task", "generation"} {
				t.Run(field, func(t *testing.T) {
					other := origin
					switch field {
					case "source_checkout":
						other.SourceCheckout = t.TempDir()
					case "namespace":
						other.Caller.Namespace = "other-owner"
					case "home":
						other.Caller.Home = t.TempDir()
					case "task":
						other.Caller.Task = "task-b"
					case "generation":
						other.Caller.Generation = "spawn-b"
					}
					if err := acceptSelectedRunOrigin(d, selected.ID, &other); err == nil || !strings.Contains(err.Error(), "caller origin conflicts") {
						t.Fatalf("same-head submission with different %s: %v", field, err)
					}
				})
			}
		})
	}
	unattributed, err := d.InsertRun(repo.ID, "other", head, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{unattributed.ID, "missing-run"} {
		if err := acceptSelectedRunOrigin(d, id, &origin); err == nil {
			t.Fatalf("accepted run without persisted origin: %s", id)
		}
		if err := acceptSelectedRunOrigin(d, id, nil); err != nil {
			t.Fatalf("origin-free selection changed: %v", err)
		}
	}
}
