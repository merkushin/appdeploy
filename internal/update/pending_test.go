package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install lays out an agent binary of the new version with the old one kept beside it.
func install(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	exe = filepath.Join(dir, "appdeploy")
	os.WriteFile(exe, []byte("new"), 0o755)
	os.WriteFile(exe+".previous", []byte("old"), 0o755)
	if err := WritePending(dir, Pending{From: "1.0.0", To: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return string(b)
}

func TestRevertsAfterRepeatedUnconfirmedStarts(t *testing.T) {
	dir, exe := install(t)
	for i := 1; i <= MaxStarts; i++ {
		reverted, msg, err := Recover(dir, exe)
		if err != nil || reverted || !strings.Contains(msg, "not confirmed") {
			t.Fatalf("start %d: reverted=%v msg=%q err=%v", i, reverted, msg, err)
		}
	}
	if read(t, exe) != "new" {
		t.Fatalf("reverted too early")
	}
	reverted, msg, err := Recover(dir, exe)
	if err != nil || !reverted {
		t.Fatalf("start %d: reverted=%v err=%v", MaxStarts+1, reverted, err)
	}
	if !strings.Contains(msg, "went back to 1.0.0") {
		t.Errorf("msg = %q", msg)
	}
	if read(t, exe) != "old" || read(t, exe+".failed") != "new" {
		t.Errorf("binaries: exe=%q failed=%q", read(t, exe), read(t, exe+".failed"))
	}
	if _, ok := ReadPending(dir); ok {
		t.Errorf("the marker was left behind")
	}
	f, ok := ReadFailed(dir)
	if !ok || f.To != "v1.1.0" || f.From != "1.0.0" {
		t.Errorf("failure note: %+v %v", f, ok)
	}
	// Afterwards nothing more happens.
	if reverted, _, _ := Recover(dir, exe); reverted {
		t.Errorf("reverted twice")
	}
}

func TestConfirmingStopsTheCountdown(t *testing.T) {
	dir, exe := install(t)
	Recover(dir, exe)
	Recover(dir, exe)
	if !Confirm(dir, "1.1.0") {
		t.Fatalf("a running new version was not confirmed")
	}
	for i := 0; i < 10; i++ {
		if reverted, _, _ := Recover(dir, exe); reverted {
			t.Fatalf("reverted after confirmation")
		}
	}
	if read(t, exe) != "new" {
		t.Errorf("the confirmed binary was replaced")
	}
	// A confirmed update also clears an old failure note.
	writeJSONFile(filepath.Join(dir, failedFile), Failed{To: "v1.0.5"})
	WritePending(dir, Pending{From: "1.1.0", To: "v1.2.0"})
	Confirm(dir, "1.2.0")
	if _, ok := ReadFailed(dir); ok {
		t.Errorf("the failure note survived a successful update")
	}
	// A marker for a newer version is an update this agent has just installed: it stays.
	WritePending(dir, Pending{From: "1.2.0", To: "v9.0.0"})
	if Confirm(dir, "1.2.0") {
		t.Errorf("the marker of a pending newer version confirmed something")
	}
	if p, ok := ReadPending(dir); !ok || p.To != "v9.0.0" {
		t.Errorf("the marker of the update that was just installed was cleared: %+v %v", p, ok)
	}
	// A marker for an older or unreadable version is stale and is dropped without confirming.
	WritePending(dir, Pending{From: "0.9.0", To: "v1.0.0"})
	if Confirm(dir, "1.2.0") {
		t.Errorf("a stale marker confirmed something")
	}
	if _, ok := ReadPending(dir); ok {
		t.Errorf("a stale marker was kept")
	}
	WritePending(dir, Pending{From: "x", To: "garbage"})
	Confirm(dir, "1.2.0")
	if _, ok := ReadPending(dir); ok {
		t.Errorf("a marker with an unreadable version was kept")
	}
}

func TestRecoverWithoutAPreviousBinary(t *testing.T) {
	dir, exe := install(t)
	os.Remove(exe + ".previous")
	for i := 0; i < MaxStarts; i++ {
		Recover(dir, exe)
	}
	if _, _, err := Recover(dir, exe); err == nil || !strings.Contains(err.Error(), "no") {
		t.Errorf("err = %v", err)
	}
	if read(t, exe) != "new" {
		t.Errorf("the binary was touched")
	}
}

func TestDecide(t *testing.T) {
	servers := []string{"c", "a", "b"} // unsorted on purpose
	peers := func(a, b, c PeerState) map[string]PeerState { return map[string]PeerState{"a": a, "b": b, "c": c} }
	up := func(v string) PeerState { return PeerState{Reachable: true, Version: v} }
	down := PeerState{}
	busy := PeerState{Reachable: true, Version: "1.0.0", Busy: true}

	cases := []struct {
		name                   string
		desired, current, self string
		peers                  map[string]PeerState
		localBusy              bool
		failedTo               string
		wantAct, wantDone      bool
		wantReason             string
	}{
		{"nothing wanted", "", "1.0.0", "a", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), false, "", false, true, ""},
		{"already there", "v1.1.0", "1.1.0", "a", nil, false, "", false, true, ""},
		{"never downgrade", "v1.0.0", "1.1.0", "a", nil, false, "", false, true, ""},
		{"the first server goes first", "v1.1.0", "1.0.0", "a", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), false, "", true, false, ""},
		{"b waits for a", "v1.1.0", "1.0.0", "b", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), false, "", false, false, "waiting for a to update first"},
		{"b goes once a has", "v1.1.0", "1.0.0", "b", peers(up("1.1.0"), up("1.0.0"), up("1.0.0")), false, "", true, false, ""},
		{"c waits for b although a is done", "v1.1.0", "1.0.0", "c", peers(up("1.1.0"), up("1.0.0"), up("1.0.0")), false, "", false, false, "waiting for b"},
		{"a server that is down is not waited for", "v1.1.0", "1.0.0", "b", peers(down, up("1.0.0"), up("1.0.0")), false, "", true, false, ""},
		{"a predecessor ahead of the target is fine", "v1.1.0", "1.0.0", "b", peers(up("1.2.0"), up("1.0.0"), up("1.0.0")), false, "", true, false, ""},
		{"nobody updates while a peer deploys", "v1.1.0", "1.0.0", "a", peers(up("1.0.0"), busy, up("1.0.0")), false, "", false, false, "waiting for b to finish a deploy"},
		{"nor while this server deploys", "v1.1.0", "1.0.0", "a", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), true, "", false, false, "deploys running here"},
		{"a failed version is not retried", "v1.1.0", "1.0.0", "a", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), false, "1.1.0", false, false, "failed here before"},
		{"another version is tried", "v1.2.0", "1.0.0", "a", peers(up("1.0.0"), up("1.0.0"), up("1.0.0")), false, "v1.1.0", true, false, ""},
		{"a development build is left alone", "v1.1.0", "dev", "a", nil, false, "", false, false, "only released builds"},
		{"a malformed wish", "latest", "1.0.0", "a", nil, false, "", false, false, "not a version"},
		{"an unparsable peer version blocks", "v1.1.0", "1.0.0", "b", peers(up("weird"), up("1.0.0"), up("1.0.0")), false, "", false, false, "waiting for a"},
	}
	for _, c := range cases {
		d := Decide(c.desired, c.current, c.self, servers, c.peers, c.localBusy, c.failedTo)
		if d.Act != c.wantAct || d.Done != c.wantDone || (c.wantReason != "" && !strings.Contains(d.Reason, c.wantReason)) {
			t.Errorf("%s: %+v, want act=%v done=%v reason~%q", c.name, d, c.wantAct, c.wantDone, c.wantReason)
		}
	}
}
