// Package proxy configures the reverse proxy that routes domains to app containers.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const accessLogger = "appdeploy_access"

// Route sends requests for Hosts to the Upstreams ("host:port").
type Route struct {
	App       string
	Hosts     []string
	Upstreams []string
}

// Proxy applies the complete desired routing table atomically.
type Proxy interface {
	Apply(ctx context.Context, routes []Route) error
}

// Caddy drives a Caddy server through its admin API (POST /load).
//
// Every Apply sends the full configuration, so Apply is idempotent and the
// switch from one set of upstreams to another is atomic.
type Caddy struct {
	AdminURL    string   // e.g. http://127.0.0.1:2019
	AdminListen string   // set only if the admin endpoint must be re-declared in the config
	Listen      []string // default [":443"]; Caddy adds the :80 redirect itself
	DisableTLS  bool     // disable automatic HTTPS (tests, or TLS terminated upstream)
	AccessLog   string   // file Caddy writes JSON access logs to; the agent's traffic watchdog reads it
	HTTP        *http.Client
}

func (c *Caddy) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Config renders the Caddy JSON config for the routes.
func (c *Caddy) Config(routes []Route) map[string]any {
	listen := c.Listen
	if len(listen) == 0 {
		listen = []string{":443"}
	}
	sorted := append([]Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].App < sorted[j].App })

	var rs []any
	for _, r := range sorted {
		if len(r.Hosts) == 0 || len(r.Upstreams) == 0 {
			continue
		}
		var ups []any
		for _, u := range r.Upstreams {
			ups = append(ups, map[string]any{"dial": u})
		}
		rs = append(rs, map[string]any{
			"match": []any{map[string]any{"host": r.Hosts}},
			"handle": []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": ups,
				// Passive health checking: skip a dead upstream briefly instead of
				// failing every request routed to it.
				"load_balancing": map[string]any{"selection_policy": map[string]any{"policy": "round_robin"}, "try_duration": "5s", "try_interval": "250ms"},
			}},
		})
	}
	// Unknown hosts get a plain 404 rather than Caddy's empty 200.
	rs = append(rs, map[string]any{
		"handle": []any{map[string]any{"handler": "static_response", "status_code": 404, "body": "no such app\n"}},
	})

	server := map[string]any{
		"listen": listen,
		"routes": rs,
	}
	if c.DisableTLS {
		server["automatic_https"] = map[string]any{"disable": true}
	}
	cfg := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{"servers": map[string]any{"appdeploy": server}},
		},
	}
	if c.AdminListen != "" {
		cfg["admin"] = map[string]any{"listen": c.AdminListen}
	}
	if c.AccessLog != "" {
		server["logs"] = map[string]any{"default_logger_name": accessLogger}
		cfg["logging"] = map[string]any{"logs": map[string]any{
			accessLogger: map[string]any{
				"writer":  map[string]any{"output": "file", "filename": c.AccessLog, "roll_size_mb": 20, "roll_keep": 3},
				"encoder": map[string]any{"format": "json"},
				"include": []any{"http.log.access." + accessLogger},
			},
		}}
	}
	return cfg
}

// Apply loads the configuration for routes into Caddy.
func (c *Caddy) Apply(ctx context.Context, routes []Route) error {
	body, err := json.Marshal(c.Config(routes))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.AdminURL, "/")+"/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("caddy admin: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Nop is a Proxy that does nothing, for running without a proxy.
type Nop struct{}

func (Nop) Apply(context.Context, []Route) error { return nil }
