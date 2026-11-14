//go:build e2e

package e2e

import "runtime"

func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }
