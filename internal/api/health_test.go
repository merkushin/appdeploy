package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"appdeploy/internal/docker"
	"appdeploy/internal/store"
)

func TestHealthIsUnavailableWhileSwitching(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateApp(store.App{Name: "shop"})
	_ = st.SetCurrentRelease("shop", 1)
	h := HealthHandler(st, &docker.Client{})

	get := func() (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/health/shop", nil))
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	// No container at all: not serving, and not because of a switch.
	if code, body := get(); code != http.StatusServiceUnavailable || body != "not serving" {
		t.Errorf("idle: %d %q", code, body)
	}
	_ = st.CreateDeploy(store.Deploy{ID: "d1", App: "shop", Release: 2, State: store.StateSwitching})
	if code, body := get(); code != http.StatusServiceUnavailable || body != "switching" {
		t.Errorf("switching: %d %q", code, body)
	}
	// Once the deploy moves on to observing, the switch is over.
	_ = st.SetDeployState("d1", store.StateObserving, "")
	if _, body := get(); body == "switching" {
		t.Errorf("still reported as switching while observing")
	}
}
