package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"appdeploy/internal/update"
)

// What releasetool signs, the updater accepts: the two sides agree on the format.
func TestSignedByReleasetoolIsAcceptedByTheUpdater(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")
	if err := keygen([]string{"-out", keyFile}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(keyFile); st.Mode().Perm() != 0o600 {
		t.Errorf("the private key has mode %v", st.Mode().Perm())
	}
	if err := keygen([]string{"-out", keyFile}); err == nil {
		t.Errorf("an existing key was overwritten")
	}
	k, err := loadKey(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("abc  appdeploy-linux-amd64\n")
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(k, sums))
	if err := update.VerifySums(k.Public().(ed25519.PublicKey), sums, []byte(sig+"\n")); err != nil {
		t.Errorf("the updater rejects a signature made like sign does: %v", err)
	}
	// A key from the environment is the same key, and wrong input is refused.
	t.Setenv("TEST_KEY", strings.TrimSpace(readFile(t, keyFile)))
	if k2, err := loadKey("", "TEST_KEY"); err != nil || !k2.Equal(k) {
		t.Errorf("the key from the environment differs: %v", err)
	}
	if _, err := loadKey(keyFile, "TEST_KEY"); err == nil {
		t.Errorf("both -key and -key-env were accepted")
	}
	t.Setenv("BAD_KEY", "not a key")
	if _, err := loadKey("", "BAD_KEY"); err == nil {
		t.Errorf("a malformed key was accepted")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
