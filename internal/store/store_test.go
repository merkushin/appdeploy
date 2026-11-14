package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppsAndReleases(t *testing.T) {
	s := open(t)
	if err := s.CreateApp(App{Name: "shop", Domains: []string{"a.test"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApp(App{Name: "shop"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate app: got %v", err)
	}
	a, _ := s.GetApp("shop")
	if a.Branch != "main" || a.CurrentRelease != 0 || len(a.Domains) != 1 {
		t.Fatalf("unexpected app: %+v", a)
	}
	if _, err := s.GetApp("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app: got %v", err)
	}

	n, _ := s.NextReleaseNumber("shop")
	if n != 1 {
		t.Fatalf("first release number = %d", n)
	}
	// A deploy that used number 1 and failed still burns the number.
	_ = s.CreateDeploy(Deploy{ID: "d1", App: "shop", Release: 1, State: StateQueued})
	if n, _ = s.NextReleaseNumber("shop"); n != 2 {
		t.Fatalf("number after deploy = %d", n)
	}
	_ = s.PutRelease(Release{App: "shop", Number: 2, Image: "x/shop:v2", Commit: "abc"})
	if n, _ = s.NextReleaseNumber("shop"); n != 3 {
		t.Fatalf("number after release = %d", n)
	}
	rels, _ := s.ListReleases("shop")
	if len(rels) != 1 || rels[0].Number != 2 {
		t.Fatalf("releases: %+v", rels)
	}
}

func TestDeployJournal(t *testing.T) {
	s := open(t)
	_ = s.CreateApp(App{Name: "a"})
	_ = s.CreateDeploy(Deploy{ID: "d1", App: "a", Release: 1, State: StateQueued})
	_ = s.CreateDeploy(Deploy{ID: "d2", App: "a", Release: 2, State: StateQueued})
	_ = s.SetDeployState("d1", StateBuilding, "")
	_ = s.SetDeployState("d2", StateFailed, "boom")

	un, _ := s.UnfinishedDeploys()
	if len(un) != 1 || un[0].ID != "d1" || un[0].State != StateBuilding {
		t.Fatalf("unfinished: %+v", un)
	}
	d2, _ := s.GetDeploy("d2")
	if d2.Error != "boom" || d2.FinishedAt.IsZero() {
		t.Fatalf("failed deploy: %+v", d2)
	}

	_ = s.AddEvent("d1", "one")
	_ = s.AddEvent("d1", "two")
	evs, _ := s.EventsAfter("d1", 0)
	if len(evs) != 2 || evs[0].Msg != "one" {
		t.Fatalf("events: %+v", evs)
	}
	evs, _ = s.EventsAfter("d1", evs[0].ID)
	if len(evs) != 1 || evs[0].Msg != "two" {
		t.Fatalf("events after: %+v", evs)
	}
	if n, _ := s.BumpAttempts("d1"); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
}

func TestContainersAndDelete(t *testing.T) {
	s := open(t)
	_ = s.CreateApp(App{Name: "a"})
	_ = s.AddContainer(Container{DockerID: "c1", Name: "n1", App: "a", Release: 1, Role: "web", HostPort: 1000, Memory: 10})
	_ = s.AddContainer(Container{DockerID: "c2", Name: "n2", App: "a", Release: 2, Role: "web", Memory: 20})
	if cs, _ := s.ReleaseContainers("a", 2); len(cs) != 1 || cs[0].DockerID != "c2" {
		t.Fatalf("release containers: %+v", cs)
	}
	_ = s.RemoveContainer("c1")
	if cs, _ := s.AllContainers(); len(cs) != 1 {
		t.Fatalf("all containers: %+v", cs)
	}
	if err := s.DeleteApp("a"); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.AllContainers(); len(cs) != 0 {
		t.Fatalf("containers survived app delete: %+v", cs)
	}
	if err := s.DeleteApp("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
