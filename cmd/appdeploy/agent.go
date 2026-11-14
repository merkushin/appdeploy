package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"appdeploy/internal/api"
	"appdeploy/internal/deploy"
	"appdeploy/internal/docker"
	"appdeploy/internal/group"
	"appdeploy/internal/notify"
	"appdeploy/internal/proxy"
	"appdeploy/internal/services"
	"appdeploy/internal/store"
	"appdeploy/internal/traffic"
	"appdeploy/internal/update"
)

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	dataDir := fs.String("data-dir", envOr("APPDEPLOY_DATA_DIR", defaultDataDir), "state directory")
	socket := fs.String("socket", envOr("APPDEPLOY_SOCKET", defaultSocket), "control socket path")
	socketGroup := fs.String("socket-group", "", "group that may use the socket (default: root only)")
	instance := fs.String("instance", "ad", "name prefix for Docker objects (lets several agents share one Docker)")
	caddyAdmin := fs.String("caddy-admin", "http://127.0.0.1:2019", "Caddy admin API URL, or \"none\" to run without a proxy")
	caddyListen := fs.String("caddy-listen", ":443", "address Caddy serves apps on")
	caddyAdminListen := fs.String("caddy-admin-listen", "", "re-declare Caddy's admin listen address in the config it is given")
	noTLS := fs.Bool("no-tls", false, "disable automatic HTTPS (plain HTTP only)")
	upstreamHost := fs.String("upstream-host", "127.0.0.1", "host the proxy uses to reach app containers")
	repoOwner := fs.String("repo-owner", "", "unix user that owns app git repositories (the SSH user)")
	accessLog := fs.String("access-log", "", "Caddy access log the traffic watchdog reads (enables 5xx-based rollback)")
	caddyAccessLog := fs.String("caddy-access-log", "", "path of the access log as Caddy sees it, if different from --access-log")
	watchWindow := fs.Duration("default-watch-window", 0, "watch.window for apps that do not set it (default 3m)")
	pruneCache := fs.Duration("prune-build-cache", 0, "after each deploy, remove Docker build cache unused for this long (e.g. 168h); off by default because the cache is shared with everything else using this Docker")
	networkPool := fs.String("network-pool", "", "CIDR to allocate /24 subnets for app networks from, instead of Docker's small default pool (e.g. 172.80.0.0/12)")
	healthListen := fs.String("health-listen", "", "address for GET /health/<app>, a status-only endpoint for load balancers (off by default)")
	monitorEvery := fs.Duration("monitor-every", 30*time.Second, "how often to look for crashed containers and low disk space (0 = never)")
	updateRepoFlag := fs.String("update-repo", updateRepo, "GitHub repository (owner/name) that publishes releases for self-update")
	updateAPI := fs.String("update-api", "", "GitHub API URL for self-update (default https://api.github.com; for tests)")
	updateEvery := fs.Duration("update-check-every", time.Minute, "how often a group member checks whether it is its turn to install the group's desired version (0 = never)")
	confirmDelay := fs.Duration("update-confirm-after", time.Minute, "how long a freshly updated agent must stay healthy before the update counts as good")
	peerListen := fs.String("peer-listen", "", "address of the peer API (default: the port of this server's advertised address)")
	backupEvery := fs.Duration("backup-every", 24*time.Hour, "back up every app's Postgres database this often (0 = never)")
	var notifyURLs stringList
	fs.Var(&notifyURLs, "notify-url", "webhook that receives every app's deploy events (repeatable)")
	minFreeBytes := fs.Int64("min-free-bytes", 0, "refuse to build with less free disk than this (default 5 GiB)")
	minFreeFrac := fs.Float64("min-free-frac", 0, "refuse to build with less free disk fraction than this (default 0.10)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if testFailStart != "" {
		return die("this build was made to fail at start (test hook)")
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("appdeployd: ")

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return die("%v", err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "state.db"))
	if err != nil {
		return die("opening state: %v", err)
	}
	defer st.Close()

	dk := &docker.Client{NetworkPool: *networkPool}
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = dk.Ping(pctx)
	pcancel()
	if err != nil {
		return die("docker is not reachable: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var counter *traffic.Counter
	if *accessLog != "" {
		counter = traffic.NewCounter()
		go counter.Tail(ctx, *accessLog)
	}
	var px proxy.Proxy = proxy.Nop{}
	if *caddyAdmin != "none" {
		logForCaddy := *caddyAccessLog
		if logForCaddy == "" {
			logForCaddy = *accessLog
		}
		px = &proxy.Caddy{AdminURL: *caddyAdmin, AdminListen: *caddyAdminListen, Listen: []string{*caddyListen},
			DisableTLS: *noTLS, AccessLog: logForCaddy}
	}
	grp, err := group.Load(filepath.Join(*dataDir, "group"), "")
	if err != nil {
		return die("loading the group: %v", err)
	}
	var remote deploy.Remote
	if grp != nil {
		remote = &api.PeerClient{Group: grp}
	}
	svc := &services.Manager{Dk: dk, Instance: *instance, DataDir: *dataDir}
	self, _ := os.Executable()
	// Test hook: APPDEPLOY_FAILPOINT=<name> kills this process (SIGKILL) at that step.
	var failPoint func(string)
	if fp := os.Getenv("APPDEPLOY_FAILPOINT"); fp != "" {
		failPoint = func(name string) {
			if name == fp {
				log.Printf("failpoint %s: killing the agent", name)
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	}
	eng, err := deploy.New(st, dk, px, deploy.Options{
		DataDir: *dataDir, Instance: *instance, UpstreamHost: *upstreamHost,
		HookBin: self, SocketPath: *socket, RepoOwner: *repoOwner,
		MinFreeBytes: *minFreeBytes, MinFreeFrac: *minFreeFrac, FailPoint: failPoint,
		Group: grp, Remote: remote, Services: svc, Traffic: counter, DefaultWatchWindow: *watchWindow, NotifyURLs: notifyURLs, PruneBuildCache: *pruneCache,
	})
	if err != nil {
		return die("%v", err)
	}

	// Listen before recovering so that clients can already connect.
	if err := os.MkdirAll(filepath.Dir(*socket), 0o755); err != nil {
		return die("%v", err)
	}
	_ = os.Remove(*socket) // stale socket from a previous run
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return die("listening on %s: %v", *socket, err)
	}
	if err := setSocketPerms(*socket, *socketGroup); err != nil {
		return die("%v", err)
	}
	if *healthListen != "" {
		hln, err := net.Listen("tcp", *healthListen)
		if err != nil {
			return die("health listener on %s: %v", *healthListen, err)
		}
		hsrv := &http.Server{Handler: api.HealthHandler(st, dk)}
		go func() { _ = hsrv.Serve(hln) }()
		defer hsrv.Close()
	}
	ctl := &api.Server{Eng: eng, St: st, Dk: dk, Svc: svc, Group: grp}
	upd := &updater{dataDir: *dataDir, repo: *updateRepoFlag, api: *updateAPI}
	ctl.Version = version
	ctl.SelfUpdate = func(uctx context.Context, check, force bool, repo string, logf func(string)) (update.Result, error) {
		return upd.install(uctx, check, force, repo, "", logf)
	}
	// The service manager starts the new binary: exit cleanly once the reply is out.
	ctl.AfterUpdate = func() { time.AfterFunc(time.Second, stop) }
	srv := &http.Server{Handler: ctl.Handler()}
	if grp != nil {
		peer := &api.PeerServer{Ctl: ctl, Group: grp}
		ctl.Peer = peer
		addr := *peerListen
		if addr == "" {
			_, port, _ := net.SplitHostPort(grp.Me().Addr)
			addr = ":" + port
		}
		pln, err := net.Listen("tcp", addr)
		if err != nil {
			return die("peer listener on %s: %v", addr, err)
		}
		psrv := &http.Server{Handler: peer.Handler()}
		go func() { _ = psrv.Serve(tls.NewListener(pln, grp.ServerTLS())) }()
		defer psrv.Close()
		log.Printf("group %q: this is %s, peers listen on %s", grp.State().Name, grp.Self, addr)
	}
	go svc.RunScheduled(ctx, *backupEvery, eng.PostgresApps, func(app string, err error) {
		eng.NotifyGlobal(notify.BackupFailed, app, err.Error())
	})

	if err := eng.Recover(ctx); err != nil {
		// A proxy that is down must not stop the agent: apps keep running and
		// the next deploy re-applies the routes.
		log.Printf("recovery: %v", err)
	}
	if *monitorEvery > 0 {
		go eng.RunMonitor(ctx, *monitorEvery)
	}
	if grp != nil {
		go eng.RunCatchUp(ctx, 30*time.Second)
		if *updateEvery > 0 {
			go upd.runLoop(ctx, *updateEvery, grp, st, &api.PeerClient{Group: grp}, stop)
		}
	}
	go confirmAfter(ctx, *confirmDelay, *dataDir, api.NewClient(*socket))
	if f, ok := update.ReadFailed(*dataDir); ok && !f.Notified {
		log.Printf("the last update was reverted: %s", f.Reason)
		eng.NotifyGlobal(notify.UpdateFailed, "", f.Reason)
		update.MarkFailedNotified(*dataDir)
	}
	log.Printf("agent %s ready: socket %s, data %s, instance %q", version, *socket, *dataDir, *instance)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		log.Printf("shutting down; unfinished deploys resume on the next start")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("serve: %v", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	eng.Close()
	_ = os.Remove(*socket)
	return 0
}

func setSocketPerms(path, group string) error {
	mode := os.FileMode(0o600)
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fmt.Errorf("socket group: %w", err)
		}
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(path, -1, gid); err != nil {
			return err
		}
		mode = 0o660
	}
	return os.Chmod(path, mode)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
