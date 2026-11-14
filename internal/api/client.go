package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"appdeploy/internal/group"
)

// Client talks to the agent over its unix socket.
type Client struct {
	Socket string
	http   *http.Client
}

// NewClient returns a client for the agent socket at path.
func NewClient(socket string) *Client {
	return &Client{
		Socket: socket,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			DisableCompression: true,
		}},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the agent at %s: %w", c.Socket, err)
	}
	return resp, nil
}

// call performs a JSON request and decodes the JSON response into out (if non-nil).
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readError(resp *http.Response) error {
	var eb errorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(b, &eb) == nil && eb.Error != "" {
		return errors.New(eb.Error)
	}
	return fmt.Errorf("agent returned %s", resp.Status)
}

func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, "GET", "/v1/ping", nil, nil)
}

func (c *Client) CreateApp(ctx context.Context, req CreateAppRequest) (AppInfo, error) {
	var out AppInfo
	err := c.call(ctx, "POST", "/v1/apps", req, &out)
	return out, err
}

func (c *Client) ListApps(ctx context.Context) ([]AppInfo, error) {
	var out []AppInfo
	err := c.call(ctx, "GET", "/v1/apps", nil, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context, app string) (StatusResponse, error) {
	var out StatusResponse
	err := c.call(ctx, "GET", "/v1/apps/"+app, nil, &out)
	return out, err
}

func (c *Client) DeleteApp(ctx context.Context, app string) error {
	return c.call(ctx, "DELETE", "/v1/apps/"+app, nil, nil)
}

func (c *Client) SetDomains(ctx context.Context, app string, domains []string) error {
	return c.call(ctx, "PUT", "/v1/apps/"+app+"/domains", map[string]any{"domains": domains}, nil)
}

func (c *Client) Releases(ctx context.Context, app string) ([]ReleaseInfo, error) {
	var out []ReleaseInfo
	err := c.call(ctx, "GET", "/v1/apps/"+app+"/releases", nil, &out)
	return out, err
}

func (c *Client) Deploy(ctx context.Context, app, ref string) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/deploy", map[string]any{"ref": ref}, &out)
	return out, err
}

// DeployImage deploys a prebuilt image; configYAML (may be empty) is the content of an appdeploy.yml.
func (c *Client) DeployImage(ctx context.Context, app, image, configYAML string) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/deploy", map[string]any{"image": image, "config_yaml": configYAML}, &out)
	return out, err
}

func (c *Client) Redeploy(ctx context.Context, app string) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/redeploy", nil, &out)
	return out, err
}

func (c *Client) Rollback(ctx context.Context, app string, to int) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/rollback", map[string]any{"to": to}, &out)
	return out, err
}

func (c *Client) Push(ctx context.Context, app, ref, sha string) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/push", map[string]any{"ref": ref, "sha": sha}, &out)
	return out, err
}

func (c *Client) GetDeploy(ctx context.Context, id string) (DeployInfo, error) {
	var out DeployInfo
	err := c.call(ctx, "GET", "/v1/deploys/"+id, nil, &out)
	return out, err
}

func (c *Client) SecretNames(ctx context.Context, app string) ([]string, error) {
	var out SecretsResponse
	err := c.call(ctx, "GET", "/v1/apps/"+app+"/secrets", nil, &out)
	return out.Names, err
}

func (c *Client) SecretValues(ctx context.Context, app string) (map[string]string, error) {
	var out SecretsResponse
	err := c.call(ctx, "GET", "/v1/apps/"+app+"/secrets?reveal=1", nil, &out)
	return out.Values, err
}

func (c *Client) SetSecrets(ctx context.Context, app string, req SetSecretsRequest) (DeployResponse, error) {
	var out DeployResponse
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/secrets", req, &out)
	return out, err
}

// StreamEvents copies a deploy's journal lines to out until the deploy ends,
// and returns its final state. If the connection drops, the deploy continues
// on the server and the caller may call StreamEvents again.
func (c *Client) StreamEvents(ctx context.Context, id string, out io.Writer) (state, errMsg string, err error) {
	var after int64
	for {
		resp, e := c.do(ctx, "GET", "/v1/deploys/"+id+"/events?after="+strconv.FormatInt(after, 10), nil)
		if e != nil {
			return "", "", e
		}
		if resp.StatusCode/100 != 2 {
			err := readError(resp)
			resp.Body.Close()
			return "", "", err
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var l EventLine
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			if l.Done {
				resp.Body.Close()
				return l.State, l.Error, nil
			}
			after = l.ID
			fmt.Fprintln(out, l.Msg)
		}
		resp.Body.Close()
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		// Stream ended without the final line (agent restart?): reconnect.
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		default:
		}
	}
}

// Logs streams container logs to out.
func (c *Client) Logs(ctx context.Context, app, role string, follow bool, tail int, out io.Writer) error {
	q := url.Values{}
	q.Set("role", role)
	if follow {
		q.Set("follow", "1")
	}
	q.Set("tail", strconv.Itoa(tail))
	resp, err := c.do(ctx, "GET", "/v1/apps/"+app+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readError(resp)
	}
	_, err = io.Copy(out, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Exec runs a command in the app's container and returns its exit code.
func (c *Client) Exec(ctx context.Context, app, role string, cmd []string, out io.Writer) (int, error) {
	resp, err := c.do(ctx, "POST", "/v1/apps/"+app+"/exec", map[string]any{"role": role, "cmd": cmd})
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 1, readError(resp)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return 1, err
	}
	code, _ := strconv.Atoi(resp.Trailer.Get("Appdeploy-Exit"))
	return code, nil
}

func (c *Client) DBBackup(ctx context.Context, app string) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	err := c.call(ctx, "POST", "/v1/apps/"+app+"/db/backup", nil, &out)
	return out.Path, err
}

func (c *Client) DBBackups(ctx context.Context, app string) ([]string, error) {
	var out []string
	err := c.call(ctx, "GET", "/v1/apps/"+app+"/db/backups", nil, &out)
	return out, err
}

func (c *Client) DBRestore(ctx context.Context, app, file string) error {
	return c.call(ctx, "POST", "/v1/apps/"+app+"/db/restore", map[string]any{"file": file}, nil)
}

func (c *Client) Group(ctx context.Context) (group.State, error) {
	var out group.State
	err := c.call(ctx, "GET", "/v1/group", nil, &out)
	return out, err
}

func (c *Client) GroupInvite(ctx context.Context) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.call(ctx, "POST", "/v1/group/invite", nil, &out)
	return out.Token, err
}

func (c *Client) GroupRemove(ctx context.Context, name string) error {
	return c.call(ctx, "DELETE", "/v1/group/servers/"+name, nil, nil)
}

// UpdateResult is the outcome of a self-update.
type UpdateResult struct {
	Log     []string `json:"log"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Updated bool     `json:"updated"`
	Error   string   `json:"error,omitempty"`
}

// SelfUpdate asks the agent to install the latest signed release. On success the
// agent restarts itself shortly afterwards.
func (c *Client) SelfUpdate(ctx context.Context, check, force bool, repo string) (UpdateResult, error) {
	var out UpdateResult
	err := c.call(ctx, "POST", "/v1/self-update", map[string]any{"check": check, "force": force, "repo": repo}, &out)
	if err == nil && out.Error != "" {
		err = errors.New(out.Error)
	}
	return out, err
}

// Export returns the files of the app's configuration for another tool, by path.
func (c *Client) Export(ctx context.Context, app, format string) (map[string]string, error) {
	var out struct {
		Files map[string]string `json:"files"`
	}
	err := c.call(ctx, "GET", "/v1/apps/"+app+"/export?format="+format, nil, &out)
	return out.Files, err
}

// GroupSetVersion makes version (empty to clear) the release every server should run.
func (c *Client) GroupSetVersion(ctx context.Context, version string) error {
	return c.call(ctx, "POST", "/v1/group/version", map[string]any{"version": version}, nil)
}

// GroupVersions lists the agent version each server runs, next to the desired one.
func (c *Client) GroupVersions(ctx context.Context) (GroupVersions, error) {
	var out GroupVersions
	err := c.call(ctx, "GET", "/v1/group/versions", nil, &out)
	return out, err
}
