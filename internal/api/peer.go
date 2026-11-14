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
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	loaded, err := p.Ctl.Dk.Load(r.Context(), zr)
	if err != nil {
		writeErr(w, err)
		return
	}
	if want := p.Ctl.Eng.ImageTag(app, rel); loaded != want {
		if err := p.Ctl.Dk.Tag(r.Context(), loaded, want); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
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
