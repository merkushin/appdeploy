// Package api is the agent's local control API: HTTP over a unix socket.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/deploy"
	"appdeploy/internal/docker"
	"appdeploy/internal/export"
	"appdeploy/internal/group"
	"appdeploy/internal/services"
	"appdeploy/internal/store"
	"appdeploy/internal/update"
)

// Server serves the control API.
type Server struct {
	Eng *deploy.Engine
	St  *store.Store
	Dk  *docker.Client
	Svc *services.Manager

	// SelfUpdate installs the latest signed release; set by the agent. AfterUpdate
	// is called after a successful install, to restart the agent.
	SelfUpdate  func(ctx context.Context, check, force bool, repo string, log func(string)) (update.Result, error)
	AfterUpdate func()

	Group *group.Group // nil on a standalone server
	Peer  *PeerServer  // used to broadcast membership changes
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, store.ErrNotFound) {
		code = http.StatusNotFound
	}
	writeJSON(w, code, errorBody{Error: err.Error()})
}

func decode(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v1/apps", s.createApp)
	mux.HandleFunc("GET /v1/apps", s.listApps)
	mux.HandleFunc("GET /v1/apps/{app}", s.appStatus)
	mux.HandleFunc("DELETE /v1/apps/{app}", s.deleteApp)
	mux.HandleFunc("PUT /v1/apps/{app}/domains", s.setDomains)
	mux.HandleFunc("POST /v1/apps/{app}/deploy", s.deploy)
	mux.HandleFunc("POST /v1/apps/{app}/redeploy", s.redeploy)
	mux.HandleFunc("POST /v1/apps/{app}/rollback", s.rollback)
	mux.HandleFunc("POST /v1/apps/{app}/push", s.push)
	mux.HandleFunc("GET /v1/apps/{app}/releases", s.releases)
	mux.HandleFunc("GET /v1/apps/{app}/secrets", s.getSecrets)
	mux.HandleFunc("POST /v1/apps/{app}/secrets", s.setSecrets)
	mux.HandleFunc("GET /v1/apps/{app}/logs", s.logs)
	mux.HandleFunc("POST /v1/apps/{app}/exec", s.exec)
	mux.HandleFunc("POST /v1/apps/{app}/db/backup", s.dbBackup)
	mux.HandleFunc("GET /v1/apps/{app}/db/backups", s.dbBackups)
	mux.HandleFunc("POST /v1/apps/{app}/db/restore", s.dbRestore)
	mux.HandleFunc("GET /v1/apps/{app}/export", s.export)
	mux.HandleFunc("POST /v1/self-update", s.selfUpdate)
	mux.HandleFunc("GET /v1/group", s.groupInfo)
	mux.HandleFunc("POST /v1/group/invite", s.groupInvite)
	mux.HandleFunc("DELETE /v1/group/servers/{name}", s.groupRemove)
	mux.HandleFunc("GET /v1/deploys/{id}", s.getDeploy)
	mux.HandleFunc("GET /v1/deploys/{id}/events", s.events)
	return mux
}

// ---- apps ----

type CreateAppRequest struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Branch  string   `json:"branch"`
}

type AppInfo struct {
	Name           string    `json:"name"`
	Domains        []string  `json:"domains"`
	Branch         string    `json:"branch"`
	CurrentRelease int       `json:"current_release"`
	CreatedAt      time.Time `json:"created_at"`
}

func appInfo(a store.App) AppInfo {
	if a.Domains == nil {
		a.Domains = []string{}
	}
	return AppInfo{Name: a.Name, Domains: a.Domains, Branch: a.Branch, CurrentRelease: a.CurrentRelease, CreatedAt: a.CreatedAt}
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req CreateAppRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	a, err := s.Eng.CreateApp(req.Name, req.Domains, req.Branch)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, appInfo(a))
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.St.ListApps()
	if err != nil {
		writeErr(w, err)
		return
	}
	out := []AppInfo{}
	for _, a := range apps {
		out = append(out, appInfo(a))
	}
	writeJSON(w, 200, out)
}

type ContainerInfo struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Release  int    `json:"release"`
	Running  bool   `json:"running"`
	Restarts int    `json:"restarts"`
	HostPort int    `json:"host_port,omitempty"`
}

type DeployInfo struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	Release    int       `json:"release"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	Ref        string    `json:"ref,omitempty"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func deployInfo(d store.Deploy) DeployInfo {
	return DeployInfo{ID: d.ID, App: d.App, Release: d.Release, State: d.State, Error: d.Error,
		Ref: d.Ref, Source: d.Source, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt}
}

type StatusResponse struct {
	App        AppInfo         `json:"app"`
	Containers []ContainerInfo `json:"containers"`
	Deploys    []DeployInfo    `json:"deploys"`
	Releases   []ReleaseInfo   `json:"releases"`
}

type ReleaseInfo struct {
	Number    int       `json:"number"`
	Commit    string    `json:"commit"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	Current   bool      `json:"current"`
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.GetApp(r.PathValue("app"))
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := StatusResponse{App: appInfo(a), Containers: []ContainerInfo{}, Deploys: []DeployInfo{}, Releases: []ReleaseInfo{}}
	cs, _ := s.St.AppContainers(a.Name)
	for _, c := range cs {
		ci := ContainerInfo{Name: c.Name, Role: c.Role, Release: c.Release, HostPort: c.HostPort}
		if st, err := s.Dk.Inspect(r.Context(), c.DockerID); err == nil {
			ci.Running, ci.Restarts = st.Running, st.RestartCount
		}
		resp.Containers = append(resp.Containers, ci)
	}
	ds, _ := s.St.ListDeploys(a.Name, 5)
	for _, d := range ds {
		resp.Deploys = append(resp.Deploys, deployInfo(d))
	}
	resp.Releases = s.releaseInfos(a)
	writeJSON(w, 200, resp)
}

func (s *Server) releaseInfos(a store.App) []ReleaseInfo {
	out := []ReleaseInfo{}
	rels, _ := s.St.ListReleases(a.Name)
	for _, rel := range rels {
		out = append(out, ReleaseInfo{Number: rel.Number, Commit: rel.Commit, Image: rel.Image, CreatedAt: rel.CreatedAt, Current: rel.Number == a.CurrentRelease})
	}
	return out
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.GetApp(r.PathValue("app"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.releaseInfos(a))
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	if err := s.Eng.RemoveApp(r.Context(), r.PathValue("app")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) setDomains(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domains []string `json:"domains"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Eng.SetDomains(r.Context(), r.PathValue("app"), req.Domains); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- deploys ----

type DeployResponse struct {
	DeployID string `json:"deploy_id"`
	Message  string `json:"message,omitempty"`
}

func (s *Server) submit(w http.ResponseWriter, req deploy.Request) {
	d, err := s.Eng.Submit(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, DeployResponse{DeployID: d.ID})
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref        string `json:"ref"`
		Image      string `json:"image"`
		ConfigYAML string `json:"config_yaml"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	s.submit(w, deploy.Request{App: r.PathValue("app"), Ref: req.Ref, Source: "cli", Image: req.Image, ConfigYAML: req.ConfigYAML})
}

func (s *Server) redeploy(w http.ResponseWriter, r *http.Request) {
	s.submit(w, deploy.Request{App: r.PathValue("app"), Source: "redeploy"})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To int `json:"to"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	a, err := s.St.GetApp(r.PathValue("app"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.To == 0 { // the newest release older than the current one
		rels, _ := s.St.ListReleases(a.Name)
		for _, rel := range rels {
			if rel.Number < a.CurrentRelease {
				req.To = rel.Number
				break
			}
		}
		if req.To == 0 {
			writeErr(w, errors.New("there is no earlier release to roll back to"))
			return
		}
	}
	s.submit(w, deploy.Request{App: a.Name, RollbackTo: req.To})
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	d, msg, err := s.Eng.HandlePush(r.PathValue("app"), req.Ref, req.SHA)
	if err != nil {
		writeErr(w, err)
		return
	}
	if d == nil {
		writeJSON(w, 200, DeployResponse{Message: msg})
		return
	}
	writeJSON(w, 202, DeployResponse{DeployID: d.ID})
}

func (s *Server) getDeploy(w http.ResponseWriter, r *http.Request) {
	d, err := s.St.GetDeploy(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, deployInfo(d))
}

// EventLine is one line of the deploy event stream. The last line has Done set.
type EventLine struct {
	ID    int64     `json:"id,omitempty"`
	Time  time.Time `json:"time,omitempty"`
	Msg   string    `json:"msg,omitempty"`
	Done  bool      `json:"done,omitempty"`
	State string    `json:"state,omitempty"`
	Error string    `json:"error,omitempty"`
}

// events streams the deploy journal as JSON lines until the deploy ends. The
// deploy itself does not depend on this connection.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.St.GetDeploy(id); err != nil {
		writeErr(w, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for {
		d, err := s.St.GetDeploy(id) // read the state before the events: terminal + no events = done
		if err != nil {
			return
		}
		evs, err := s.St.EventsAfter(id, after)
		if err != nil {
			return
		}
		for _, e := range evs {
			after = e.ID
			if err := enc.Encode(EventLine{ID: e.ID, Time: e.Time, Msg: e.Msg}); err != nil {
				return
			}
		}
		if len(evs) == 0 && store.IsTerminal(d.State) {
			_ = enc.Encode(EventLine{Done: true, State: d.State, Error: d.Error})
			if fl != nil {
				fl.Flush()
			}
			return
		}
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- secrets ----

type SecretsResponse struct {
	Names  []string          `json:"names,omitempty"`
	Values map[string]string `json:"values,omitempty"`
}

func (s *Server) getSecrets(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if _, err := s.St.GetApp(app); err != nil {
		writeErr(w, err)
		return
	}
	if r.URL.Query().Get("reveal") == "1" {
		vals, err := s.Eng.Secrets(app)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, SecretsResponse{Values: vals})
		return
	}
	names, err := s.Eng.SecretNames(app)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, SecretsResponse{Names: names})
}

type SetSecretsRequest struct {
	Set    map[string]string `json:"set"`
	Unset  []string          `json:"unset"`
	Deploy bool              `json:"deploy"` // roll the change out as a new release
}

func (s *Server) setSecrets(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	a, err := s.St.GetApp(app)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req SetSecretsRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Eng.SetSecrets(app, req.Set, req.Unset); err != nil {
		writeErr(w, err)
		return
	}
	if req.Deploy && a.CurrentRelease > 0 {
		s.submit(w, deploy.Request{App: app, Source: "redeploy"})
		return
	}
	writeJSON(w, 200, DeployResponse{})
}

// ---- logs and exec ----

func (s *Server) appContainers(r *http.Request) ([]store.Container, error) {
	a, err := s.St.GetApp(r.PathValue("app"))
	if err != nil {
		return nil, err
	}
	role := r.URL.Query().Get("role")
	cs, err := s.St.ReleaseContainers(a.Name, a.CurrentRelease)
	if err != nil {
		return nil, err
	}
	var out []store.Container
	for _, c := range cs {
		if role == "" || c.Role == role {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no running containers for this app/role")
	}
	return out, nil
}

// flushWriter flushes after every write so output streams in real time.
type flushWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// prefixWriter prefixes each line with the container name.
type prefixWriter struct {
	prefix string
	out    io.Writer
	lw     *docker.LineWriter
}

func newPrefixWriter(prefix string, out io.Writer) *prefixWriter {
	p := &prefixWriter{prefix: prefix, out: out}
	p.lw = docker.NewLineWriter(func(line string) { fmt.Fprintf(out, "%s | %s\n", prefix, line) })
	return p
}

func (p *prefixWriter) Write(b []byte) (int, error) { return p.lw.Write(b) }

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	cs, err := s.appContainers(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	follow := r.URL.Query().Get("follow") == "1"
	tail := 100
	if t, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil {
		tail = t
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	out := &flushWriter{w: w, f: fl}
	var wg sync.WaitGroup
	for _, c := range cs {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			pw := newPrefixWriter(c.Name, out)
			_ = s.Dk.Logs(r.Context(), c.DockerID, follow, tail, pw)
			pw.lw.Flush()
		}()
	}
	wg.Wait()
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string   `json:"role"`
		Cmd  []string `json:"cmd"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if len(req.Cmd) == 0 {
		writeErr(w, errors.New("cmd is required"))
		return
	}
	if req.Role == "" {
		req.Role = "web"
	}
	q := r.URL.Query()
	q.Set("role", req.Role)
	r.URL.RawQuery = q.Encode()
	cs, err := s.appContainers(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "Appdeploy-Exit")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	out := &flushWriter{w: w, f: fl}
	err = s.Dk.Exec(r.Context(), cs[0].DockerID, req.Cmd, out)
	code := "0"
	if err != nil {
		code = "1"
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			code = strconv.Itoa(ee.ExitCode())
		} else {
			fmt.Fprintf(out, "appdeploy: %v\n", err)
		}
	}
	w.Header().Set("Appdeploy-Exit", code)
}

// ---- database backups ----

func (s *Server) dbApp(w http.ResponseWriter, r *http.Request) (string, bool) {
	app := r.PathValue("app")
	if _, err := s.St.GetApp(app); err != nil {
		writeErr(w, err)
		return "", false
	}
	if s.Svc == nil {
		writeErr(w, errors.New("this agent has no service support"))
		return "", false
	}
	return app, true
}

func (s *Server) dbBackup(w http.ResponseWriter, r *http.Request) {
	app, ok := s.dbApp(w, r)
	if !ok {
		return
	}
	path, err := s.Svc.Backup(r.Context(), app)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

func (s *Server) dbBackups(w http.ResponseWriter, r *http.Request) {
	app, ok := s.dbApp(w, r)
	if !ok {
		return
	}
	names, err := s.Svc.ListBackups(app)
	if err != nil {
		writeErr(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, 200, names)
}

func (s *Server) dbRestore(w http.ResponseWriter, r *http.Request) {
	app, ok := s.dbApp(w, r)
	if !ok {
		return
	}
	var req struct {
		File string `json:"file"`
	}
	if err := decode(r, &req); err != nil || req.File == "" {
		writeErr(w, errors.New("file is required"))
		return
	}
	// Only files of this app's backup directory may be restored.
	if strings.ContainsAny(req.File, "/\\") || strings.Contains(req.File, "..") {
		writeErr(w, errors.New("file must be a backup name from `db list`"))
		return
	}
	if err := s.Svc.Restore(r.Context(), app, req.File); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- group ----

func (s *Server) groupInfo(w http.ResponseWriter, r *http.Request) {
	if s.Group == nil {
		writeErr(w, errors.New("this server is not in a group (appdeploy group init)"))
		return
	}
	writeJSON(w, 200, s.Group.State())
}

func (s *Server) groupInvite(w http.ResponseWriter, r *http.Request) {
	if s.Group == nil {
		writeErr(w, errors.New("this server is not in a group"))
		return
	}
	tok, err := s.Group.Invite(time.Hour)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"token": tok})
}

func (s *Server) groupRemove(w http.ResponseWriter, r *http.Request) {
	if s.Group == nil || !s.Group.IsFounder() {
		writeErr(w, errors.New("only the server that created the group can change its membership"))
		return
	}
	name := r.PathValue("name")
	if name == s.Group.Self {
		writeErr(w, errors.New("the founder cannot remove itself"))
		return
	}
	st, err := s.Group.RemoveServer(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.Peer != nil {
		go s.Peer.Broadcast(context.Background(), st)
	}
	writeJSON(w, 200, st)
}

// ---- export ----

// export renders the app's live release as the configuration of another tool.
// It contains the names of secrets, never their values.
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.GetApp(r.PathValue("app"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if a.CurrentRelease == 0 {
		writeErr(w, errors.New("the app has no release yet; deploy it first"))
		return
	}
	rel, err := s.St.GetRelease(a.Name, a.CurrentRelease)
	if err != nil {
		writeErr(w, err)
		return
	}
	cfg, err := config.FromJSON(rel.Config)
	if err != nil {
		writeErr(w, err)
		return
	}
	names, err := s.Eng.SecretNames(a.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	seen := map[string]bool{}
	var domains []string
	for _, d := range append(append([]string{}, a.Domains...), cfg.Domains...) {
		if d != "" && !seen[d] {
			seen[d] = true
			domains = append(domains, d)
		}
	}
	spec := export.Spec{App: a.Name, Release: a.CurrentRelease, Config: cfg, Domains: domains, SecretNames: names}
	if s.Group != nil {
		spec.Servers = s.Group.State().Servers
	}
	files, err := export.Render(r.URL.Query().Get("format"), spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"files": files})
}

// ---- self-update ----

func (s *Server) selfUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Check bool   `json:"check"`
		Force bool   `json:"force"`
		Repo  string `json:"repo"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if s.SelfUpdate == nil {
		writeErr(w, errors.New("this agent cannot update itself"))
		return
	}
	var lines []string
	reply := func(res update.Result, err error) {
		out := UpdateResult{Log: lines, From: res.From, To: res.To, Updated: res.Updated}
		if err != nil {
			out.Error = err.Error()
		}
		writeJSON(w, 200, out)
	}
	if !req.Check && !req.Force {
		// Restarting the agent interrupts the step of a running deploy; it resumes afterwards,
		// but a quiet moment is better.
		if un, _ := s.St.UnfinishedDeploys(); len(un) > 0 {
			reply(update.Result{}, fmt.Errorf("%d deploy(s) are in progress; try again when they are done, or use --force", len(un)))
			return
		}
	}
	res, err := s.SelfUpdate(r.Context(), req.Check, req.Force, req.Repo, func(l string) { lines = append(lines, l) })
	reply(res, err)
	if err == nil && res.Updated && s.AfterUpdate != nil {
		s.AfterUpdate()
	}
}
