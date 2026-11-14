package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary is what a release asset is in these tests: a script that reports its version.
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho appdeploy " + version + "\n")
}

type fixture struct {
	srv  *httptest.Server
	pub  ed25519.PublicKey
	exe  string
	auth string // Authorization header seen on the last asset request
}

type releaseSpec struct {
	tag        string // release tag
	binVersion string // what the binary says it is (default: the tag without v)
	signWith   ed25519.PrivateKey
	tamper     bool // serve different bytes than the signed hash
	noBinary   bool
}

func setup(t *testing.T, spec releaseSpec) *fixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if spec.signWith == nil {
		spec.signWith = priv
	}
	if spec.binVersion == "" {
		spec.binVersion = strings.TrimPrefix(spec.tag, "v")
	}
	bin := fakeBinary(spec.binVersion)
	h := sha256.Sum256(bin)
	sums := []byte(hex.EncodeToString(h[:]) + "  appdeploy-linux-amd64\n")
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(spec.signWith, sums)))
	served := bin
	if spec.tamper {
		served = append(append([]byte{}, bin...), []byte("# evil\n")...)
	}
	f := &fixture{pub: pub}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/acme/tool/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assets := []map[string]string{
			{"name": "SHA256SUMS", "url": base + "/api/1", "browser_download_url": base + "/dl/SHA256SUMS"},
			{"name": "SHA256SUMS.sig", "url": base + "/api/2", "browser_download_url": base + "/dl/SHA256SUMS.sig"},
		}
		if !spec.noBinary {
			assets = append(assets, map[string]string{"name": "appdeploy-linux-amd64", "url": base + "/api/3", "browser_download_url": base + "/dl/appdeploy-linux-amd64"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": spec.tag, "assets": assets})
	})
	body := map[string][]byte{"SHA256SUMS": sums, "SHA256SUMS.sig": sig, "appdeploy-linux-amd64": served}
	for name, b := range body {
		b := b
		mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(b) })
	}
	for i, name := range []string{"SHA256SUMS", "SHA256SUMS.sig", "appdeploy-linux-amd64"} {
		b := body[name]
		mux.HandleFunc(fmt.Sprintf("/api/%d", i+1), func(w http.ResponseWriter, r *http.Request) {
			f.auth = r.Header.Get("Authorization")
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "wrong Accept", 415)
				return
			}
			w.Write(b)
		})
	}
	f.srv = httptest.NewServer(mux)
	base = f.srv.URL
	t.Cleanup(f.srv.Close)
	f.exe = filepath.Join(t.TempDir(), "appdeploy")
	if err := os.WriteFile(f.exe, fakeBinary("1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) opts(current string) Options {
	return Options{API: f.srv.URL, Repo: "acme/tool", PublicKey: f.pub, Current: current, OS: "linux", Arch: "amd64", Exe: f.exe}
}

func (f *fixture) content(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(f.exe)
	return strings.TrimSpace(string(b))
}

func TestUpdatesAndKeepsThePreviousBinary(t *testing.T) {
	f := setup(t, releaseSpec{tag: "v1.2.0"})
	var logs []string
	o := f.opts("1.0.0")
	o.Log = func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }
	res, err := Run(context.Background(), o)
	if err != nil || !res.Updated || res.To != "v1.2.0" {
		t.Fatalf("res=%+v err=%v logs=%v", res, err, logs)
	}
	if !strings.Contains(f.content(t), "appdeploy 1.2.0") {
		t.Errorf("the binary was not replaced: %s", f.content(t))
	}
	if b, _ := os.ReadFile(f.exe + ".previous"); !strings.Contains(string(b), "appdeploy 1.0.0") {
		t.Errorf("the previous binary was not kept: %s", b)
	}
	if st, _ := os.Stat(f.exe); st.Mode().Perm()&0o111 == 0 {
		t.Errorf("the new binary is not executable")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.exe), ".appdeploy.new")); err == nil {
		t.Errorf("a temporary file was left behind")
	}
}

func TestVersionRules(t *testing.T) {
	ctx := context.Background()
	f := setup(t, releaseSpec{tag: "v1.2.0"})
	for _, cur := range []string{"1.2.0", "v1.3.0", "2.0.0"} {
		if res, err := Run(ctx, f.opts(cur)); err != nil || res.Updated {
			t.Errorf("current %s: %+v %v (no downgrade or reinstall without --force)", cur, res, err)
		}
	}
	if !strings.Contains(f.content(t), "appdeploy 1.0.0") {
		t.Fatalf("the binary changed")
	}
	o := f.opts("1.0.0")
	o.CheckOnly = true
	if res, err := Run(ctx, o); err != nil || res.Updated || !strings.Contains(f.content(t), "1.0.0") {
		t.Errorf("--check changed something: %+v %v", res, err)
	}
	o = f.opts("2.0.0")
	o.Force = true
	if res, err := Run(ctx, o); err != nil || !res.Updated {
		t.Errorf("--force: %+v %v", res, err)
	}
	// A development build has no version to compare, so it takes the release.
	f = setup(t, releaseSpec{tag: "v0.4.1"})
	if res, err := Run(ctx, f.opts("dev")); err != nil || !res.Updated {
		t.Errorf("dev build: %+v %v", res, err)
	}
}

func TestRefusesWhatIsNotSignedAndIntact(t *testing.T) {
	ctx := context.Background()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]struct {
		spec releaseSpec
		want string
	}{
		"signed by another key":       {releaseSpec{tag: "v1.2.0", signWith: other}, "signature of SHA256SUMS is not valid"},
		"binary differs from the sum": {releaseSpec{tag: "v1.2.0", tamper: true}, "does not match the signed checksum"},
		"binary says another version": {releaseSpec{tag: "v1.2.0", binVersion: "0.0.1"}, "reports"},
		"binary missing":              {releaseSpec{tag: "v1.2.0", noBinary: true}, "has no appdeploy-linux-amd64"},
		"tag is not a version":        {releaseSpec{tag: "nightly"}, "not a version"},
	}
	for name, c := range cases {
		f := setup(t, c.spec)
		_, err := Run(ctx, f.opts("1.0.0"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want %q", name, err, c.want)
		}
		if !strings.Contains(f.content(t), "appdeploy 1.0.0") {
			t.Errorf("%s: the running binary was replaced", name)
		}
		if _, err := os.Stat(f.exe + ".previous"); err == nil {
			t.Errorf("%s: a .previous file appeared", name)
		}
	}
	// A build without a key cannot verify anything.
	f := setup(t, releaseSpec{tag: "v1.2.0"})
	o := f.opts("1.0.0")
	o.PublicKey = nil
	if _, err := Run(ctx, o); err == nil || !strings.Contains(err.Error(), "no release signing key") {
		t.Errorf("no key: %v", err)
	}
	// And a missing repository or release is a clear error.
	o = f.opts("1.0.0")
	o.Repo = ""
	if _, err := Run(ctx, o); err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Errorf("no repo: %v", err)
	}
	o.Repo = "acme/nothing"
	if _, err := Run(ctx, o); err == nil || !strings.Contains(err.Error(), "has acme/nothing published a release") {
		t.Errorf("no release: %v", err)
	}
}

func TestTokenUsesTheAPIAssetURLs(t *testing.T) {
	f := setup(t, releaseSpec{tag: "v1.2.0"})
	o := f.opts("1.0.0")
	o.Token = "ghp_secret"
	if res, err := Run(context.Background(), o); err != nil || !res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	if f.auth != "Bearer ghp_secret" {
		t.Errorf("asset requests did not carry the token: %q", f.auth)
	}
}

func TestParseVersionAndSums(t *testing.T) {
	for in, want := range map[string][3]int{"v1.2.3": {1, 2, 3}, "1.2.3-rc1": {1, 2, 3}, "0.10.0+build": {0, 10, 0}} {
		if got, ok := ParseVersion(in); !ok || got != want {
			t.Errorf("ParseVersion(%q) = %v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"dev", "", "1.2", "1.2.x", "v-1.0.0"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) parsed", bad)
		}
	}
	if !Newer([3]int{1, 10, 0}, [3]int{1, 9, 9}) || Newer([3]int{1, 0, 0}, [3]int{1, 0, 0}) {
		t.Errorf("Newer is wrong")
	}
	m := ParseSums([]byte("ABC123  appdeploy-linux-amd64\ndef456 *appdeploy-darwin-arm64\nnot a line\n"))
	if m["appdeploy-linux-amd64"] != "abc123" || m["appdeploy-darwin-arm64"] != "def456" || len(m) != 2 {
		t.Errorf("ParseSums = %v", m)
	}
}
