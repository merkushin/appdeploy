package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyPostsFullConfig(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()

	c := &Caddy{AdminURL: srv.URL, Listen: []string{":8080"}, DisableTLS: true, AdminListen: ":2019", AccessLog: "/var/log/appdeploy/access.log"}
	err := c.Apply(context.Background(), []Route{
		{App: "b", Hosts: []string{"b.test"}, Upstreams: []string{"127.0.0.1:2000", "127.0.0.1:2001"}},
		{App: "a", Hosts: []string{"a.test", "www.a.test"}, Upstreams: []string{"127.0.0.1:1000"}},
		{App: "idle", Hosts: []string{"idle.test"}}, // no upstreams: must be skipped
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "POST /load" {
		t.Fatalf("request = %q", path)
	}
	raw, _ := json.Marshal(got)
	s := string(raw)
	for _, want := range []string{`"a.test"`, `"www.a.test"`, `"127.0.0.1:1000"`, `"127.0.0.1:2001"`, `":8080"`, `"disable":true`, `"listen":":2019"`, `"status_code":404`, `"filename":"/var/log/appdeploy/access.log"`, `"default_logger_name":"appdeploy_access"`, `"include":["http.log.access.appdeploy_access"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("config is missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "idle.test") {
		t.Errorf("route without upstreams was rendered:\n%s", s)
	}
	if strings.Index(s, "a.test") > strings.Index(s, "b.test") {
		t.Errorf("routes are not sorted by app, config is not deterministic")
	}
}

func TestApplyReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad config", 400)
	}))
	defer srv.Close()
	err := (&Caddy{AdminURL: srv.URL}).Apply(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("got %v", err)
	}
}
