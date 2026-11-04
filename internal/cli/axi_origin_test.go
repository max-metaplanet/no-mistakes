package cli

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runorigin"
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
