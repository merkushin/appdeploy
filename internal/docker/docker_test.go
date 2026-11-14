package docker

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`/app/server worker`:    {"/app/server", "worker"},
		`  a   b  `:             {"a", "b"},
		`sh -c "echo hi there"`: {"sh", "-c", "echo hi there"},
		`sh -c 'echo "x y"'`:    {"sh", "-c", `echo "x y"`},
		`a "" b`:                {"a", "", "b"},
		`"a\"b"`:                {`a"b`},
		``:                      nil,
	}
	for in, want := range cases {
		got, err := SplitCommand(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{`"open`, `'open`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestLineWriter(t *testing.T) {
	var lines []string
	w := NewLineWriter(func(s string) { lines = append(lines, s) })
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\r\nthree"))
	w.Flush()
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("got %q", lines)
	}
}

func TestFreeSubnetValidatesPool(t *testing.T) {
	for _, bad := range []string{"nonsense", "10.0.0.0/25", "fd00::/48"} {
		c := &Client{NetworkPool: bad}
		if _, err := c.freeSubnet(context.Background()); err == nil || !strings.Contains(err.Error(), "network pool") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
