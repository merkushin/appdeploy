// Command hello is a tiny HTTP app for testing appdeploy.
//
//	hello            serve HTTP on $PORT (default 8080)
//	hello worker     log a line every second until stopped
//	hello migrate    pretend to run database migrations
package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed message.txt
var message string

func main() {
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "serve":
		serve()
	case "worker":
		worker()
	case "migrate":
		if os.Getenv("FAIL_MIGRATE") != "" {
			fmt.Println("migrate: failing on purpose")
			os.Exit(1)
		}
		fmt.Println("migrate: ok")
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func serve() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv(r.URL.Query().Get("name")))
	})
	// /write?file=a&v=text and /read?file=a use $DATA_DIR (default /data): a volume in the tests.
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	mux.HandleFunc("/write", func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(r.URL.Query().Get("file"))), []byte(r.URL.Query().Get("v")), 0o644); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/read", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.Base(r.URL.Query().Get("file"))))
		if err != nil {
			http.Error(w, "none", 404)
			return
		}
		fmt.Fprint(w, string(b))
	})
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("d"))
		time.Sleep(d)
		fmt.Fprintln(w, "slept")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, strings.TrimSpace(message))
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("%s listening on :%s", strings.TrimSpace(message), port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func worker() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("worker stopping")
			return
		case <-t.C:
			log.Println("worker tick")
		}
	}
}
