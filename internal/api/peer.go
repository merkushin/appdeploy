package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"appdeploy/internal/config"
	"appdeploy/internal/deploy"
	"appdeploy/internal/group"
	"appdeploy/internal/store"
)

// PeerServer is the API other servers of the group use, served over mutual TLS.
type PeerServer struct {
	Ctl   *Server // shares the deploy event stream with the control API
	Group *group.Group
}

func (p *PeerServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := group.VerifiedPeer(r); !ok {
			http.Error(w, "a client certificate of this group is required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (p *PeerServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /peer/v1/join", p.join)
	mux.HandleFunc("GET /peer/v1/ping", p.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "name": p.Group.Self})
	}))
	mux.HandleFunc("PUT /peer/v1/group", p.auth(p.putGroup))
	mux.HandleFunc("POST /peer/v1/image", p.auth(p.image))
	mux.HandleFunc("GET /peer/v1/image", p.auth(p.fetchImage))
	mux.HandleFunc("GET /peer/v1/finals", p.auth(func(w http.ResponseWriter, r *http.Request) {
		fs, err := p.Ctl.Eng.Finals()
		if err != nil {
			writeErr(w, err)
			return
		}
		if fs == nil {
			fs = []store.Final{}
		}
		writeJSON(w, 200, fs)
	}))
	mux.HandleFunc("PUT /peer/v1/final", p.auth(func(w http.ResponseWriter, r *http.Request) {
		var f store.Final
		if err := decode(r, &f); err != nil || !config.ValidAppName(f.App) || f.Release < 1 {
			writeErr(w, errors.New("bad record"))
			return
		}
		if err := p.Ctl.Eng.HandleFinal(f); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /peer/v1/release", p.auth(func(w http.ResponseWriter, r *http.Request) {
		app := r.URL.Query().Get("app")
		rel, _ := strconv.Atoi(r.URL.Query().Get("release"))
		if !config.ValidAppName(app) || rel < 1 {
			writeErr(w, errors.New("bad app or release"))
			return
		}
		pl, err := p.Ctl.Eng.ReleasePayload(app, rel)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, pl)
	}))
	mux.HandleFunc("POST /peer/v1/source", p.auth(p.source))
	mux.HandleFunc("POST /peer/v1/deploy", p.auth(p.deploy))
	mux.HandleFunc("GET /peer/v1/deploys/{id}/events", p.auth(p.Ctl.events))
	return mux
}

func (p *PeerServer) join(w http.ResponseWriter, r *http.Request) {
	st, ok := p.Group.HandleJoin(w, r)
	if !ok {
		return
	}
	go p.Broadcast(context.Background(), st)
}

// Broadcast pushes the membership to every member except this one and the newest.
func (p *PeerServer) Broadcast(ctx context.Context, st group.State) {
	c := p.Group.Client(15 * time.Second)
	body, _ := json.Marshal(st)
	for _, s := range st.Servers {
		if s.Name == p.Group.Self {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+s.Addr+"/peer/v1/group", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}

func (p *PeerServer) putGroup(w http.ResponseWriter, r *http.Request) {
	var st group.State
	if err := decode(r, &st); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := p.Group.Apply(st); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// image receives a gzip-compressed `docker save` and makes it available under this server's tag.
func (p *PeerServer) image(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	rel, _ := strconv.Atoi(r.URL.Query().Get("release"))
	if !config.ValidAppName(app) || rel < 1 {
		writeErr(w, errors.New("bad app or release"))
		return
	}
	if err := p.Ctl.Eng.ImportImage(r.Context(), app, rel, r.Body); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// source receives the build context of a release this server is to build.
func (p *PeerServer) source(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	rel, _ := strconv.Atoi(r.URL.Query().Get("release"))
	if !config.ValidAppName(app) || rel < 1 {
		writeErr(w, errors.New("bad app or release"))
		return
	}
	if err := p.Ctl.Eng.ReceiveSource(app, rel, r.Body); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// fetchImage streams a release image as a gzip-compressed `docker save`.
func (p *PeerServer) fetchImage(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	rel, _ := strconv.Atoi(r.URL.Query().Get("release"))
	if !config.ValidAppName(app) || rel < 1 {
		writeErr(w, errors.New("bad app or release"))
		return
	}
	tag := p.Ctl.Eng.ImageTag(app, rel)
	if !p.Ctl.Dk.ImageExists(r.Context(), tag) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no image " + tag})
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	zw := gzip.NewWriter(w)
	if err := p.Ctl.Dk.Save(r.Context(), tag, zw); err != nil {
		return // the stream is already under way; the receiver sees it end early
	}
	_ = zw.Close()
}

func (p *PeerServer) deploy(w http.ResponseWriter, r *http.Request) {
	var pl deploy.PeerPayload
	if err := decode(r, &pl); err != nil {
		writeErr(w, err)
		return
	}
	if !config.ValidAppName(pl.App) || pl.Release < 1 {
		writeErr(w, errors.New("bad app or release"))
		return
	}
	d, err := p.Ctl.Eng.SubmitPeer(pl)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, DeployResponse{DeployID: d.ID})
}

// ---- client side (implements deploy.Remote) ----

// PeerClient talks to the peer APIs of other servers.
type PeerClient struct {
	Group *group.Group
}

func (c *PeerClient) url(s group.Server, path string) string { return "https://" + s.Addr + path }

func (c *PeerClient) SendImage(ctx context.Context, srv group.Server, app string, release int, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(srv, fmt.Sprintf("/peer/v1/image?app=%s&release=%d", app, release)), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	return nil
}

func (c *PeerClient) Ping(ctx context.Context, srv group.Server) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(srv, "/peer/v1/ping"), nil)
	if err != nil {
		return err
	}
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	return nil
}

func (c *PeerClient) Announce(ctx context.Context, srv group.Server, f store.Final) error {
	b, _ := json.Marshal(f)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(srv, "/peer/v1/final"), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	return nil
}

func (c *PeerClient) Finals(ctx context.Context, srv group.Server) ([]store.Final, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(srv, "/peer/v1/finals"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, readError(resp)
	}
	var out []store.Final
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *PeerClient) FetchRelease(ctx context.Context, srv group.Server, app string, release int) (deploy.PeerPayload, error) {
	var pl deploy.PeerPayload
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(srv, fmt.Sprintf("/peer/v1/release?app=%s&release=%d", app, release)), nil)
	if err != nil {
		return pl, err
	}
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return pl, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return pl, readError(resp)
	}
	return pl, json.NewDecoder(resp.Body).Decode(&pl)
}

func (c *PeerClient) SendSource(ctx context.Context, srv group.Server, app string, release int, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(srv, fmt.Sprintf("/peer/v1/source?app=%s&release=%d", app, release)), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	return nil
}

func (c *PeerClient) FetchImage(ctx context.Context, srv group.Server, app string, release int) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(srv, fmt.Sprintf("/peer/v1/image?app=%s&release=%d", app, release)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Group.Client(0).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

func (c *PeerClient) StartDeploy(ctx context.Context, srv group.Server, p deploy.PeerPayload) (string, error) {
	b, _ := json.Marshal(p)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url(srv, "/peer/v1/deploy"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Group.Client(2 * time.Minute).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", readError(resp)
	}
	var out DeployResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.DeployID, nil
}

// StreamDeploy follows the journal of a deploy on another server. A dropped
// connection is retried from the last event; the deploy runs on regardless.
func (c *PeerClient) StreamDeploy(ctx context.Context, srv group.Server, id string, line func(string)) (string, string, error) {
	var after int64
	failures := 0
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url(srv, "/peer/v1/deploys/"+id+"/events?after="+strconv.FormatInt(after, 10)), nil)
		resp, err := c.Group.Client(0).Do(req)
		if err == nil && resp.StatusCode/100 != 2 {
			err = readError(resp)
			resp.Body.Close()
		}
		if err != nil {
			if failures++; failures > 30 || ctx.Err() != nil {
				return "", "", err
			}
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		failures = 0
		dec := json.NewDecoder(resp.Body)
		for {
			var l EventLine
			if dec.Decode(&l) != nil {
				break
			}
			if l.Done {
				resp.Body.Close()
				return l.State, l.Error, nil
			}
			after = l.ID
			line(strings.TrimRight(l.Msg, "\n"))
		}
		resp.Body.Close()
	}
}

var _ = store.StateSucceeded
