package runorigin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestCallerOriginRejectsMalformedAndSecretFields(t *testing.T) {
	valid := Caller{Namespace: "fixture", Home: filepath.Clean(t.TempDir()), Task: "task-a", Generation: "generation-a"}
	data, _ := json.Marshal(valid)
	if _, err := Decode(data); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`null`, `{}`, string(data) + ` {}`, strings.Replace(string(data), `"task-a"`, `"bad\u0000task"`, 1), strings.Replace(string(data), `"generation-a"`, `""`, 1), strings.TrimSuffix(string(data), "}") + `,"token":"never forward"}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("accepted malformed origin %q", bad)
		}
	}
}

func TestOriginStampIgnoresAmbientAndTurnIdentity(t *testing.T) {
	t.Setenv(ReferenceEnv, "ambient-run")
	t.Setenv(SocketEnv, "ambient-socket")
	for _, pin := range []runenv.Overlay{{}, {Set: map[string]string{ReferenceEnv: "run-a", SocketEnv: "socket-a"}}, {Set: map[string]string{ReferenceEnv: "run-b", SocketEnv: "socket-b"}}} {
		got := Stamp([]string{ReferenceEnv + "=forged-turn", SocketEnv + "=forged-socket", "KEEP=yes"}, pin)
		joined := strings.Join(got, "\n")
		if strings.Contains(joined, "forged") || strings.Contains(joined, "ambient") {
			t.Fatal(joined)
		}
		if want := pin.Set[ReferenceEnv]; want != "" && !strings.Contains(joined, ReferenceEnv+"="+want) {
			t.Fatal(joined)
		}
	}
}
