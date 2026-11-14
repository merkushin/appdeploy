//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func (e *testEnv) volumesOf(app string) []string {
	out, _ := sh("docker", "volume", "ls", "-q", "--filter", "label=appdeploy.instance="+e.instance, "--filter", "label=appdeploy.app="+app)
	return strings.Fields(out)
}

// Data on a named volume survives deploys, a rollback and even destroying the app,
// and goes away only when asked.
func TestVolumesKeepDataUntilTheyAreDeleted(t *testing.T) {
	e := env
	const app, host = "vol", "vol.test"
	e.mustAd(t, "apps", "create", app, "--domain", host)
	t.Cleanup(func() { _, _ = e.ad("apps", "destroy", app, "--delete-volumes") })
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "roles:\n  web:\n    volumes: [\"data:/data\"]\n")
	r.commit("with a volume")
	if out, err := r.push(app); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	if code, body := e.get(host, "/write?file=note&v=persisted"); code != 200 || body != "ok" {
		t.Fatalf("write: %d %q", code, body)
	}

	// A new release sees the data, and the journal warns that two releases share the volume.
	r.write("message.txt", "hello volumes v2\n")
	r.commit("v2")
	out, err := r.push(app)
	if err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v2: %v\n%s", err, out)
	}
	contains(t, out, "WARNING: volume(s) data are used by v1 and by v2")
	e.expectBody(t, host, "/read?file=note", "persisted")

	e.mustAd(t, "rollback", app)
	e.expectBody(t, host, "/read?file=note", "persisted")
	want := e.instance + "-" + app + "-data"
	if got := e.volumesOf(app); len(got) != 1 || got[0] != want {
		t.Fatalf("volumes of the app: %v, want [%s]", got, want)
	}

	// Destroying the app keeps the data, and says where it is.
	out = e.mustAd(t, "apps", "destroy", app)
	contains(t, out, "its data is kept in the volume(s): "+want)
	if got := e.volumesOf(app); len(got) != 1 {
		t.Fatalf("the volume was deleted by destroy: %v", got)
	}
	// A new app of the same name picks the data up again.
	e.mustAd(t, "apps", "create", app, "--domain", host)
	if out, err := r.push(app); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("redeploy after destroy: %v\n%s", err, out)
	}
	e.expectBody(t, host, "/read?file=note", "persisted")

	// Only --delete-volumes removes it.
	out = e.mustAd(t, "apps", "destroy", app, "--delete-volumes")
	notContains(t, out, "kept")
	if got := e.volumesOf(app); len(got) != 0 {
		t.Errorf("--delete-volumes left %v", got)
	}
}

// A Dockerfile VOLUME that nothing is mounted over would lose its data at every
// deploy, so the deploy is refused before it starts anything.
func TestAnImageVolumeWithoutAMountIsRefused(t *testing.T) {
	e := env
	e.newApp(t, "vguard", "vguard.test")
	r := e.newRepo(t, "hello")
	df := r.read("Dockerfile")
	r.write("Dockerfile", strings.Replace(df, "EXPOSE 8080", "EXPOSE 8080\nVOLUME /data", 1))
	r.commit("declare VOLUME")
	out, _ := r.push("vguard")
	contains(t, out, "deploy failed")
	contains(t, out, "the image declares VOLUME /data but role web mounts nothing there")
	contains(t, out, `roles.web.volumes: ["data:/data"]`)
	if cs := e.containers("vguard"); len(cs) != 0 {
		t.Errorf("a container was started: %v", cs)
	}
	if code, _ := e.get("vguard.test", "/"); code != 404 {
		t.Errorf("a refused deploy got a route: %d", code)
	}

	// With the mount it deploys, and the data lives on the named volume.
	r.write("appdeploy.yml", "roles:\n  web:\n    volumes: [\"data:/data\"]\n")
	r.commit("mount it")
	if out, err := r.push("vguard"); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("with the mount: %v\n%s", err, out)
	}
	e.expectBody(t, "vguard.test", "/", "hello v1")
	if got := e.volumesOf("vguard"); len(got) != 1 {
		t.Errorf("volumes: %v", got)
	}
}

// env: in appdeploy.yml is plain environment that travels with the commit and the
// release; a secret of the same name wins, and a rollback restores the old values.
func TestEnvFromTheConfig(t *testing.T) {
	e := env
	const app, host = "envapp", "envapp.test"
	e.newApp(t, app, host)
	read := func() string { _, b := e.get(host, "/env?name=GREETING"); return b }
	r := e.newRepo(t, "hello")
	r.write("appdeploy.yml", "env:\n  GREETING: from-config\n  LEVEL: 3\n")
	r.commit("env")
	if out, err := r.push(app); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v1: %v\n%s", err, out)
	}
	e.expectBody(t, host, "/env?name=GREETING", "from-config")
	e.expectBody(t, host, "/env?name=LEVEL", "3")

	// A secret of the same name wins; removing it brings the config's value back.
	e.mustAd(t, "secrets", "set", app, "GREETING=from-secret")
	e.expectBody(t, host, "/env?name=GREETING", "from-secret")
	e.mustAd(t, "secrets", "unset", app, "GREETING")
	e.expectBody(t, host, "/env?name=GREETING", "from-config")

	// Changing the config is a commit; going back restores the old value.
	r.write("appdeploy.yml", "env:\n  GREETING: changed\n")
	r.commit("change")
	if out, err := r.push(app); err != nil || !strings.Contains(out, "==> succeeded") {
		t.Fatalf("v4: %v\n%s", err, out)
	}
	e.expectBody(t, host, "/env?name=GREETING", "changed")
	if got := read(); got != "changed" {
		t.Fatal(got)
	}
	e.mustAd(t, "rollback", app, "--to", "1")
	e.expectBody(t, host, "/env?name=GREETING", "from-config")
	e.expectBody(t, host, "/env?name=LEVEL", "3")

	// A secret-looking name is allowed but flagged in the deploy journal.
	r.write("appdeploy.yml", "env:\n  SMTP_PASSWORD: oops\n")
	r.commit("a secret in git")
	out, _ := r.push(app)
	contains(t, out, "warning: env.SMTP_PASSWORD looks like a secret")
}
