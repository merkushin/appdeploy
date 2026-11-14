// Command broken misbehaves in the way mode.txt says, to exercise failed deploys.
//
//	ok         behaves like a healthy app
//	unhealthy  serves HTTP but answers 500 on /up
//	crash      exits with an error shortly after starting
//	silent     runs but never listens on its port
//	errors     healthy on /up (so it passes the deploy checks) but answers 500 everywhere else
//	fail-on:NAME  unhealthy only on the server NAME ($APPDEPLOY_SERVER), healthy elsewhere
//	dies-later passes the deploy checks, then exits 4 seconds after starting
package main

import (
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed mode.txt
var modeFile string

func main() {
	mode := strings.TrimSpace(modeFile)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("broken app starting in mode %q", mode)
	switch mode {
	case "crash":
		time.Sleep(500 * time.Millisecond)
		log.Fatal("boom: crashing as requested")
	case "silent":
		time.Sleep(time.Hour)
	}
	if mode == "dies-later" {
		go func() {
			time.Sleep(4 * time.Second)
			log.Fatal("dying later, as requested")
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		if mode == "unhealthy" || mode == "fail-on:"+os.Getenv("APPDEPLOY_SERVER") {
			http.Error(w, "unhealthy", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if mode == "errors" {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "broken app, mode "+mode)
	})
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
