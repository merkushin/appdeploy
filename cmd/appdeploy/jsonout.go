package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// jsonOut is set by --json: commands then print JSON instead of text, errors included.
var jsonOut bool

// errReported marks an error that was already printed (as the final line of a
// JSON event stream), so that it is not printed twice.
var errReported = errors.New("")

// extractJSON removes --json from args (anywhere before a "--") and records it.
func extractJSON(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "--json" {
			jsonOut = true
			continue
		}
		out = append(out, a)
	}
	return out
}

// emit prints v as one line of JSON.
func emit(v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 1, err
	}
	fmt.Println(string(b))
	return 0, nil
}

// reportError prints an error the way the current mode wants it.
func reportError(msg string) {
	if jsonOut {
		b, _ := json.Marshal(map[string]string{"error": msg})
		fmt.Println(string(b))
		return
	}
	fmt.Fprintf(os.Stderr, "appdeploy: %s\n", msg)
}
