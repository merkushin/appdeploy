package deploy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func (e *Engine) appDir(app string) string      { return filepath.Join(e.opt.DataDir, "apps", app) }
func (e *Engine) repoDir(app string) string     { return filepath.Join(e.appDir(app), "repo.git") }
func (e *Engine) releasesDir(app string) string { return filepath.Join(e.appDir(app), "releases") }
func (e *Engine) buildDir(app, id string) string {
	return filepath.Join(e.appDir(app), "build", id)
}
func (e *Engine) secretsFile(app string) string {
	return filepath.Join(e.opt.DataDir, "secrets", app+".env")
}
func (e *Engine) snapshotFile(app string, release int) string {
	return filepath.Join(e.releasesDir(app), fmt.Sprintf("%d.env", release))
}
func (e *Engine) tmpDir() string { return filepath.Join(e.opt.DataDir, "tmp") }

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvKey reports whether s is a usable environment variable name.
func ValidEnvKey(s string) bool { return envKeyRe.MatchString(s) }

// ReadEnvFile parses a KEY=value file. Missing files give an empty map.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out, sc.Err()
}

// WriteEnvFile writes KEY=value lines with mode 0600, atomically.
func WriteEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SecretNames lists the names of the app's secrets (never the values).
func (e *Engine) SecretNames(app string) ([]string, error) {
	m, err := ReadEnvFile(e.secretsFile(app))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// Secrets returns the app's secrets including values. Callers decide who may see them.
func (e *Engine) Secrets(app string) (map[string]string, error) {
	return ReadEnvFile(e.secretsFile(app))
}

// SetSecrets merges set into the app's secrets and removes the keys in unset.
func (e *Engine) SetSecrets(app string, set map[string]string, unset []string) error {
	for k, v := range set {
		if !ValidEnvKey(k) {
			return fmt.Errorf("invalid secret name %q", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("secret %s: multi-line values are not supported", k)
		}
	}
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	cur, err := ReadEnvFile(e.secretsFile(app))
	if err != nil {
		return err
	}
	for k, v := range set {
		cur[k] = v
	}
	for _, k := range unset {
		delete(cur, k)
	}
	return WriteEnvFile(e.secretsFile(app), cur)
}

// snapshotSecrets freezes the app's current secrets as the release's own copy.
// Values in extra (service URLs) are overridden by secrets of the same name,
// so DATABASE_URL set as a secret points the app at an external database.
func (e *Engine) snapshotSecrets(app string, release int, extra map[string]string) error {
	e.secretsMu.Lock()
	defer e.secretsMu.Unlock()
	cur, err := ReadEnvFile(e.secretsFile(app))
	if err != nil {
		return err
	}
	merged := map[string]string{}
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range cur {
		merged[k] = v
	}
	return WriteEnvFile(e.snapshotFile(app, release), merged)
}

// ---- git ----

var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@^~-]*$`)

func git(repo string, args ...string) *exec.Cmd {
	full := append([]string{"-c", "safe.directory=*", "--git-dir", repo}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// gitResolve turns a ref or abbreviated sha into a full commit sha.
func gitResolve(repo, ref string) (string, error) {
	if !refRe.MatchString(ref) {
		return "", fmt.Errorf("invalid git ref %q", ref)
	}
	var out, errb bytes.Buffer
	cmd := git(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ref %q not found in the app repository", ref)
	}
	return strings.TrimSpace(out.String()), nil
}

// gitShow returns the content of path at the commit, or nil if it does not exist.
func gitShow(repo, sha, path string) ([]byte, error) {
	var out bytes.Buffer
	cmd := git(repo, "show", sha+":"+path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, nil
	}
	return out.Bytes(), nil
}

// gitCheckout exports the tree of the commit into dir.
func gitCheckout(repo, sha, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive := git(repo, "archive", "--format=tar", sha)
	untar := exec.Command("tar", "-x", "-C", dir)
	pr, pw := io.Pipe()
	archive.Stdout = pw
	var aerr, terr bytes.Buffer
	archive.Stderr = &aerr
	untar.Stdin = pr
	untar.Stderr = &terr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := archive.Start(); err != nil {
		pw.Close()
		_ = untar.Wait()
		return err
	}
	aWait := archive.Wait()
	pw.Close()
	tWait := untar.Wait()
	if aWait != nil {
		return fmt.Errorf("git archive: %v: %s", aWait, strings.TrimSpace(aerr.String()))
	}
	if tWait != nil {
		return fmt.Errorf("tar: %v: %s", tWait, strings.TrimSpace(terr.String()))
	}
	return nil
}

// insideDir joins rel onto base and refuses paths that escape base.
func insideDir(base, rel string) (string, error) {
	p := filepath.Join(base, rel)
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the repository", rel)
	}
	return p, nil
}

// initRepo creates the app's bare repository with a post-receive hook.
func (e *Engine) initRepo(app, branch string) error {
	repo := e.repoDir(app)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("git", "init", "--bare", "--quiet", "--initial-branch="+branch, repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v: %s", err, strings.TrimSpace(string(out)))
	}
	hooks := filepath.Join(repo, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s hook post-receive --app %s --socket %s\n",
		shQuote(e.opt.HookBin), shQuote(app), shQuote(e.opt.SocketPath))
	if err := os.WriteFile(filepath.Join(hooks, "post-receive"), []byte(script), 0o755); err != nil {
		return err
	}
	return e.chownTree(repo)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// chownTree gives the repository to the configured git user, so that
// git-receive-pack run by that user can write to it.
func (e *Engine) chownTree(root string) error {
	if e.opt.RepoOwner == "" {
		return nil
	}
	u, err := user.Lookup(e.opt.RepoOwner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func removeFile(p string) error { return os.Remove(p) }
func removeAll(p string) error {
	if p == "" {
		return nil
	}
	return os.RemoveAll(p)
}
