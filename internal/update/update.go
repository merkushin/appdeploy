// Package update replaces the running binary with the latest GitHub release,
// after checking that the release was signed by the project's key.
//
// A release consists of binaries named appdeploy-<os>-<arch>, a SHA256SUMS file
// listing their hashes, and SHA256SUMS.sig: an ed25519 signature of SHA256SUMS,
// base64-encoded. TLS only proves that a file came from GitHub; the signature
// proves that the owner of the signing key built it.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxBinary = 300 << 20
	maxText   = 1 << 20
)

// Options says what to update and how to trust it.
type Options struct {
	API       string // GitHub API base URL; default https://api.github.com
	Repo      string // owner/name
	PublicKey ed25519.PublicKey
	Current   string // version of the running binary; "dev" for unreleased builds
	OS, Arch  string
	Exe       string // path of the binary to replace
	Tag       string // install exactly this release (e.g. v1.3.0) instead of the latest
	Force     bool   // allow the same or an older version
	CheckOnly bool   // report what would happen without changing anything
	Token     string // optional GitHub token (private repositories)
	Client    *http.Client
	Log       func(format string, args ...any)
	// BeforeSwap is called right before the new binary replaces the old one; if it
	// fails the update is abandoned. The agent uses it to leave a marker that makes
	// a failing new version revert (see Pending).
	BeforeSwap func(from, to string) error
}

// Result reports what happened.
type Result struct {
	From, To string
	Updated  bool
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Web  string `json:"browser_download_url"`
}

type release struct {
	Tag    string  `json:"tag_name"`
	Assets []asset `json:"assets"`
}

// ParseVersion reads "v1.2.3", "1.2.3" or "1.2.3-rc1" as numbers; pre-release
// suffixes are ignored. ok is false for anything else (such as "dev").
func ParseVersion(s string) (v [3]int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// Newer reports whether a is a higher version than b.
func Newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// ParseSums reads sha256sum output into name -> hex hash.
func ParseSums(sums []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// VerifySums checks the signature of the SHA256SUMS file.
func VerifySums(pub ed25519.PublicKey, sums, sigB64 []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("this build has no release signing key, so it cannot verify updates")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("the signature file is malformed")
	}
	if !ed25519.Verify(pub, sums, sig) {
		return errors.New("the signature of SHA256SUMS is not valid for this build's key: refusing to install")
	}
	return nil
}

func (o *Options) get(ctx context.Context, link string, limit int64, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	req.Header.Set("User-Agent", "appdeploy-self-update")
	c := o.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", redactURL(link), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", redactURL(link), limit)
	}
	return b, nil
}

func redactURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}

func (o *Options) assetURL(a asset) (link, accept string) {
	if o.Token != "" && a.URL != "" {
		return a.URL, "application/octet-stream" // private repositories serve assets through the API URL
	}
	return a.Web, ""
}

// Run finds the latest release, verifies it and replaces o.Exe with it.
func Run(ctx context.Context, o Options) (Result, error) {
	res := Result{From: o.Current}
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.Repo == "" || !strings.Contains(o.Repo, "/") {
		return res, errors.New("no release repository is configured (use --repo owner/name)")
	}
	if o.OS == "" || o.Arch == "" {
		return res, errors.New("os and arch are required")
	}
	path := "/releases/latest"
	if o.Tag != "" {
		if _, ok := ParseVersion(o.Tag); !ok {
			return res, fmt.Errorf("%q is not a version like v1.2.3", o.Tag)
		}
		path = "/releases/tags/" + url.PathEscape(o.Tag)
		o.logf("looking for release %s of %s", o.Tag, o.Repo)
	} else {
		o.logf("looking for the latest release of %s", o.Repo)
	}
	b, err := o.get(ctx, strings.TrimRight(o.API, "/")+"/repos/"+o.Repo+path, maxText, "application/vnd.github+json")
	if err != nil {
		return res, fmt.Errorf("finding the latest release: %w (has %s published a release?)", err, o.Repo)
	}
	var rel release
	if err := json.Unmarshal(b, &rel); err != nil {
		return res, fmt.Errorf("reading the release: %w", err)
	}
	res.To = rel.Tag
	nv, ok := ParseVersion(rel.Tag)
	if !ok {
		return res, fmt.Errorf("the latest release is tagged %q, which is not a version like v1.2.3", rel.Tag)
	}
	if cv, ok := ParseVersion(o.Current); ok && !o.Force {
		if !Newer(nv, cv) {
			o.logf("already up to date (%s; the latest release is %s)", o.Current, rel.Tag)
			return res, nil
		}
	}
	o.logf("%s -> %s", o.Current, rel.Tag)
	if o.CheckOnly {
		o.logf("an update is available (run without --check to install it)")
		return res, nil
	}

	find := func(name string) (asset, error) {
		for _, a := range rel.Assets {
			if a.Name == name {
				return a, nil
			}
		}
		return asset{}, fmt.Errorf("the release %s has no %s", rel.Tag, name)
	}
	binName := fmt.Sprintf("appdeploy-%s-%s", o.OS, o.Arch)
	var files [3][]byte
	for i, name := range []string{"SHA256SUMS", "SHA256SUMS.sig", binName} {
		a, err := find(name)
		if err != nil {
			return res, err
		}
		assetLink, accept := o.assetURL(a)
		limit := int64(maxText)
		if i == 2 {
			limit = maxBinary
			o.logf("downloading %s", name)
		}
		if files[i], err = o.get(ctx, assetLink, limit, accept); err != nil {
			return res, err
		}
	}
	sums, sig, bin := files[0], files[1], files[2]
	if err := VerifySums(o.PublicKey, sums, sig); err != nil {
		return res, err
	}
	want, ok := ParseSums(sums)[binName]
	if !ok {
		return res, fmt.Errorf("SHA256SUMS does not list %s", binName)
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return res, fmt.Errorf("%s does not match the signed checksum: refusing to install", binName)
	}
	o.logf("signature and checksum are valid")

	if o.Exe == "" {
		return res, errors.New("the path of the running binary is unknown")
	}
	exe, err := filepath.EvalSymlinks(o.Exe)
	if err != nil {
		return res, err
	}
	tmp := filepath.Join(filepath.Dir(exe), "."+filepath.Base(exe)+".new")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return res, fmt.Errorf("writing next to %s: %w", exe, err)
	}
	defer os.Remove(tmp)
	// Run the new binary before trusting it with the place of the old one.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(cctx, tmp, "version")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("the downloaded binary does not run: %v: %s", err, strings.TrimSpace(out.String()))
	}
	if !strings.Contains(out.String(), strings.TrimPrefix(rel.Tag, "v")) {
		return res, fmt.Errorf("the downloaded binary reports %q, not %s", strings.TrimSpace(out.String()), rel.Tag)
	}
	if o.BeforeSwap != nil {
		if err := o.BeforeSwap(o.Current, rel.Tag); err != nil {
			return res, fmt.Errorf("preparing the update: %w", err)
		}
	}
	prev := exe + ".previous"
	_ = os.Remove(prev)
	if err := os.Rename(exe, prev); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(prev, exe)
		return res, err
	}
	o.logf("installed %s at %s (the old binary is kept as %s)", rel.Tag, exe, prev)
	res.Updated = true
	return res, nil
}

// Fingerprint is the SHA-256 of the raw public key in hex: what you compare when
// you check, out of band, that a binary trusts the key you expect. It can be
// reproduced with: echo KEY | base64 -d | sha256sum
func Fingerprint(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:])
}
