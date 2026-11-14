package api

import (
	"fmt"
	"net/http"

	"appdeploy/internal/docker"
	"appdeploy/internal/store"
)

// HealthHandler serves GET /health/{app} for external load balancers: 200 when
// this server runs a web container of the app's live release, and 503 otherwise,
// including while a deploy is moving traffic (state switching).
// It exposes nothing but that status, so it needs no authentication.
func HealthHandler(st *store.Store, dk *docker.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/{app}", func(w http.ResponseWriter, r *http.Request) {
		app, err := st.GetApp(r.PathValue("app"))
		if err != nil || app.CurrentRelease == 0 {
			http.Error(w, "not serving", http.StatusServiceUnavailable)
			return
		}
		// While traffic is being moved, ask balancers to look elsewhere.
		if ds, _ := st.ListDeploys(app.Name, 5); len(ds) > 0 {
			for _, d := range ds {
				if d.State == store.StateSwitching {
					http.Error(w, "switching", http.StatusServiceUnavailable)
					return
				}
			}
		}
		cs, _ := st.ReleaseContainers(app.Name, app.CurrentRelease)
		for _, c := range cs {
			if c.Role != "web" {
				continue
			}
			if s, err := dk.Inspect(r.Context(), c.DockerID); err == nil && s.Running && !s.Restarting {
				fmt.Fprintln(w, "ok")
				return
			}
		}
		http.Error(w, "not serving", http.StatusServiceUnavailable)
	})
	return mux
}
