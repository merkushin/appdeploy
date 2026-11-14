package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendGenericJSON(t *testing.T) {
	var got Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	ev := Event{Event: DeployFailed, App: "shop", Release: 3, DeployID: "d1", Error: "boom", Time: time.Now()}
	if err := (&Notifier{}).Send(context.Background(), srv.URL, ev); err != nil {
		t.Fatal(err)
	}
	if got.Event != DeployFailed || got.App != "shop" || got.Release != 3 || got.Error != "boom" {
		t.Errorf("got %+v", got)
	}
}

func TestChatAdaptersGetText(t *testing.T) {
	ev := Event{Event: DeployRolledBack, App: "shop", Release: 4, Commit: "0123456789abcdef", Error: "5xx"}
	for _, u := range []string{"https://hooks.slack.com/services/T/B/x", "https://api.telegram.org/bot1:x/sendMessage?chat_id=5"} {
		b, err := body(u, ev)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil || len(m) != 1 || !strings.Contains(m["text"], "ROLLED BACK") || !strings.Contains(m["text"], "shop v4 (01234567)") {
			t.Errorf("%s: %s", u, b)
		}
	}
}

func TestRetriesTransientFailuresOnly(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			http.Error(w, "later", 503)
		}
	}))
	defer srv.Close()
	n := &Notifier{Backoff: time.Millisecond}
	if err := n.Send(context.Background(), srv.URL, Event{Event: DeploySucceeded}); err != nil || hits.Load() != 3 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}

	hits.Store(0)
	rej := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", 403)
	}))
	defer rej.Close()
	err := n.Send(context.Background(), rej.URL+"/secret-token", Event{Event: DeploySucceeded})
	if err == nil || hits.Load() != 1 {
		t.Fatalf("a 4xx must not be retried: err=%v hits=%d", err, hits.Load())
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaks the webhook path: %v", err)
	}
}
