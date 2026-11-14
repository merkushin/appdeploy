package group

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serve starts the peer listener of g on 127.0.0.1 and returns its address.
func serve(t *testing.T, g *Group, onJoin func(State)) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /peer/v1/join", func(w http.ResponseWriter, r *http.Request) {
		if st, ok := g.HandleJoin(w, r); ok && onJoin != nil {
			onJoin(st)
		}
	})
	mux.HandleFunc("GET /peer/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		name, ok := VerifiedPeer(r)
		if !ok {
			http.Error(w, "no client certificate", 401)
			return
		}
		w.Write([]byte(name))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.TLS = g.ServerTLS()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}

func TestInitInviteJoinAndMutualTLS(t *testing.T) {
	ctx := context.Background()
	// The founder must know its advertised address before it can issue certificates for it,
	// so reserve a port first.
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := probe.Addr().String()
	probe.Close()
	founder, err := Init(t.TempDir(), "prod", "s1", addr, []string{"web", "db"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	var joined State
	mux.HandleFunc("POST /peer/v1/join", func(w http.ResponseWriter, r *http.Request) {
		if st, ok := founder.HandleJoin(w, r); ok {
			joined = st
		}
	})
	mux.HandleFunc("GET /peer/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		name, ok := VerifiedPeer(r)
		if !ok {
			http.Error(w, "no client certificate", 401)
			return
		}
		w.Write([]byte(name))
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener, srv.TLS = ln, founder.ServerTLS()
	srv.StartTLS()
	defer srv.Close()

	tok, err := founder.Invite(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	second, err := Join(ctx, dir2, tok, "s2", "127.0.0.1:7071", []string{"web"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got := second.State(); len(got.Servers) != 2 || got.Version != joined.Version || second.IsFounder() {
		t.Fatalf("second's state: %+v", got)
	}
	if len(founder.Peers()) != 1 || founder.Peers()[0].Name != "s2" || !founder.Peers()[0].HasLabel("web") {
		t.Fatalf("founder peers: %+v", founder.Peers())
	}

	// Mutual TLS: the member is recognized by name; clients without a certificate are not.
	resp, err := second.Client(5 * time.Second).Get("https://" + addr + "/peer/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	b := new(strings.Builder)
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	resp.Body.Close()
	if resp.StatusCode != 200 || b.String() != "s2" {
		t.Errorf("whoami = %d %q", resp.StatusCode, b.String())
	}
	anon := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if r, err := anon.Get("https://" + addr + "/peer/v1/whoami"); err != nil || r.StatusCode != 401 {
		t.Errorf("anonymous request: %v %v", r, err)
	}

	// The invite is single-use.
	if _, err := Join(ctx, t.TempDir(), tok, "s3", "127.0.0.1:7072", nil); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Errorf("replayed invite: %v", err)
	}
	// An expired invite is refused.
	old, _ := founder.Invite(time.Nanosecond)
	time.Sleep(5 * time.Millisecond)
	if _, err := Join(ctx, t.TempDir(), old, "s3", "127.0.0.1:7072", nil); err == nil {
		t.Errorf("expired invite accepted")
	}
	// A token whose CA fingerprint does not match the server is refused before the secret is sent.
	other, _ := Init(t.TempDir(), "other", "x", addr, nil)
	badTok, _ := other.Invite(time.Minute)
	if _, err := Join(ctx, t.TempDir(), badTok, "s3", "127.0.0.1:7072", nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("wrong CA: %v", err)
	}
	// Only the founder can invite.
	if _, err := second.Invite(time.Minute); err == nil {
		t.Errorf("a member invited")
	}
	// A group is not created twice, and the state survives a reload.
	if _, err := Init(dir2, "again", "s2", addr, nil); err == nil {
		t.Errorf("init over an existing group")
	}
	again, err := Load(dir2, "s2")
	if err != nil || again == nil || len(again.State().Servers) != 2 {
		t.Errorf("reload: %v %+v", err, again)
	}
}

func TestVersionedMembership(t *testing.T) {
	g, err := Init(t.TempDir(), "g", "a", "127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := g.State()
	if changed, _ := g.Apply(State{Version: st.Version, Name: "g"}); changed {
		t.Errorf("an equal version replaced the state")
	}
	st2, _ := g.AddServer(Server{Name: "b", Addr: "127.0.0.1:2"})
	if st2.Version != st.Version+1 || len(g.State().Servers) != 2 {
		t.Errorf("add: %+v", st2)
	}
	if changed, _ := g.Apply(State{Version: 1, Name: "g"}); changed {
		t.Errorf("an older version replaced the state")
	}
	if _, err := g.RemoveServer("b"); err != nil || len(g.State().Servers) != 1 {
		t.Errorf("remove: %v", err)
	}
	if _, err := g.RemoveServer("nope"); err == nil {
		t.Errorf("removed a missing server")
	}
	_ = serve
}
