// Package docs holds tests that keep the documentation true: they fail when the
// docs and the code disagree about commands, configuration keys, agent flags,
// API routes, links, examples or the error messages the docs rely on.
//
// If llms-full.txt is stale, regenerate it with: UPDATE_DOCS=1 go test ./internal/docs
package docs

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"appdeploy/internal/config"
)

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func glob(t *testing.T, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root(), pattern))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		rel, _ := filepath.Rel(root(), p)
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func markdownFiles(t *testing.T) []string {
	return append([]string{"README.md"}, glob(t, "docs/*.md")...)
}

// ---- commands ----

var usageLine = regexp.MustCompile(`^  ([a-z][a-z-]*)(?: ([a-z][a-z-]*(?:\|[a-z-]+)*))?`)

// commandsInUsage lists every command of the CLI as the help text names it.
func commandsInUsage(t *testing.T) []string {
	src := read(t, "cmd/appdeploy/main.go")
	i := strings.Index(src, "const usage = `")
	if i < 0 {
		t.Fatal("usage text not found in main.go")
	}
	body := src[i+len("const usage = `"):]
	body = body[:strings.Index(body, "`")]
	var cmds []string
	for _, line := range strings.Split(body, "\n") {
		m := usageLine.FindStringSubmatch(line)
		if m == nil || m[1] == "appdeploy" {
			continue
		}
		if m[2] == "" {
			cmds = append(cmds, m[1])
			continue
		}
		for _, alt := range strings.Split(m[2], "|") {
			cmds = append(cmds, m[1]+" "+alt)
		}
	}
	if len(cmds) < 20 {
		t.Fatalf("parsed only %d commands from the usage text: %v", len(cmds), cmds)
	}
	return cmds
}

func TestEveryCommandIsDocumented(t *testing.T) {
	doc := read(t, "docs/commands.md")
	for _, c := range commandsInUsage(t) {
		if !strings.Contains(doc, "appdeploy "+c) {
			t.Errorf("docs/commands.md does not mention `appdeploy %s`", c)
		}
	}
}

// ---- configuration keys ----

func configKeys(prefix string, typ reflect.Type, out *[]string) {
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		key := prefix + tag
		ft := f.Type
		switch {
		case ft.Kind() == reflect.Struct:
			configKeys(key+".", ft, out)
		case ft.Kind() == reflect.Map && (ft.Elem().Kind() == reflect.Ptr || ft.Elem().Kind() == reflect.Struct):
			*out = append(*out, key)
			et := ft.Elem()
			if et.Kind() == reflect.Ptr {
				et = et.Elem()
			}
			configKeys(key+".<name>.", et, out)
		default:
			*out = append(*out, key)
		}
	}
}

func TestEveryConfigKeyIsDocumented(t *testing.T) {
	var keys []string
	configKeys("", reflect.TypeOf(config.App{}), &keys)
	if len(keys) < 20 {
		t.Fatalf("found only %d keys: %v", len(keys), keys)
	}
	doc := read(t, "docs/configuration.md")
	for _, k := range keys {
		if !strings.Contains(doc, "`"+k+"`") {
			t.Errorf("docs/configuration.md has no row for the key `%s`", k)
		}
	}
}

// ---- agent flags ----

var flagDef = regexp.MustCompile(`fs\.\w+\((?:&\w+, )?"([a-z][a-z-]*)"`)

func TestEveryAgentFlagIsDocumented(t *testing.T) {
	flags := flagDef.FindAllStringSubmatch(read(t, "cmd/appdeploy/agent.go"), -1)
	if len(flags) < 20 {
		t.Fatalf("found only %d agent flags", len(flags))
	}
	doc := read(t, "docs/configuration.md")
	for _, m := range flags {
		if !strings.Contains(doc, "`--"+m[1]+"`") {
			t.Errorf("docs/configuration.md has no row for the flag `--%s`", m[1])
		}
	}
}

// ---- API routes ----

var routeDef = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ [^"]+)"`)

func TestEveryAPIRouteIsDocumented(t *testing.T) {
	doc := read(t, "docs/api.md")
	n := 0
	for _, f := range glob(t, "internal/api/*.go") {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, m := range routeDef.FindAllStringSubmatch(read(t, f), -1) {
			n++
			// The route must end there: "/redeploy" is not a mention of "/redeploys".
			if !regexp.MustCompile(regexp.QuoteMeta(m[1]) + `([^A-Za-z0-9/{}_-]|$)`).MatchString(doc) {
				t.Errorf("docs/api.md does not mention the route %s (%s)", m[1], f)
			}
		}
	}
	if n < 40 {
		t.Fatalf("found only %d routes", n)
	}
}

// ---- links ----

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// slug turns a heading into the anchor GitHub gives it.
func slug(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

func anchors(t *testing.T, rel string) map[string]bool {
	out := map[string]bool{}
	inCode := false
	for _, line := range strings.Split(read(t, rel), "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if inCode || !strings.HasPrefix(line, "#") {
			continue
		}
		out[slug(strings.TrimLeft(line, "# "))] = true
	}
	return out
}

const rawPrefix = "https://raw.githubusercontent.com/merkushin/appdeploy/main/"

func TestMarkdownLinksResolve(t *testing.T) {
	files := append(markdownFiles(t), "llms.txt", "SPEC.md")
	for _, f := range files {
		for _, m := range linkRe.FindAllStringSubmatch(read(t, f), -1) {
			link := m[1]
			target, frag, _ := strings.Cut(link, "#")
			switch {
			case strings.HasPrefix(link, "mailto:"):
				continue
			case strings.HasPrefix(link, rawPrefix):
				target = strings.TrimPrefix(target, rawPrefix)
			case strings.HasPrefix(link, "http://"), strings.HasPrefix(link, "https://"):
				continue
			case target == "":
				target = f // an anchor in the same file
			default:
				target = filepath.ToSlash(filepath.Join(filepath.Dir(f), target))
			}
			if _, err := os.Stat(filepath.Join(root(), target)); err != nil {
				t.Errorf("%s links to %s, which does not exist", f, link)
				continue
			}
			if frag != "" && strings.HasSuffix(target, ".md") && !anchors(t, target)[frag] {
				t.Errorf("%s links to %s, but %s has no heading for #%s", f, link, target, frag)
			}
		}
	}
}

// ---- examples ----

var yamlBlock = regexp.MustCompile("(?s)```yaml appdeploy\\.yml\\n(.*?)```")

func TestConfigExamplesAreValid(t *testing.T) {
	n := 0
	for _, f := range markdownFiles(t) {
		for _, m := range yamlBlock.FindAllStringSubmatch(read(t, f), -1) {
			n++
			if _, err := config.Parse([]byte(m[1])); err != nil {
				t.Errorf("a ```yaml appdeploy.yml example in %s is rejected by the parser: %v\n%s", f, err, m[1])
			}
		}
	}
	if n < 4 {
		t.Fatalf("found only %d examples; did the fence marker change?", n)
	}
}

// ---- error messages the docs rely on ----

// phrases are fragments of messages that docs/troubleshooting.md explains (or
// the guide for assistants quotes). Each must be in the docs and in the code, so
// renaming a message in the code fails here until the docs follow.
var phrases = []string{
	"did not pass within", "exited right after starting", "killed for exceeding its memory limit",
	"build failed", "not found in the repository (build.dockerfile)", "release command failed",
	"not enough memory", "not enough disk space to build", "not found in the app repository",
	"gave up after", "private registries need docker login", "invalid image reference",
	"failed with 5xx, above the", "failed 3 times in a row",
	"cannot reach the agent at", "is not allowed for the", "interactive shells are not available",
	"unexpected option", "create it with: appdeploy apps create", "already exists",
	"is not the deploy branch", "it may have been pruned", "there is no earlier release to roll back to",
	"is already the current release", "has nothing deployed yet", "has a deploy in progress",
	"docker is not reachable", "is not reachable", "speaks protocol", "carries the labels the roles ask for",
	"are only available when every role runs on the coordinating server", "rollout stopped in wave",
	"invalid or expired invite", "does not match the invite (wrong server?)",
	"only the server that created the group can", "to update first",
	"published a release", "signature of SHA256SUMS is not valid", "built without a release signing key",
	"are in progress", "failed here before and was reverted", "superseded by",
}

func goSources(t *testing.T) string {
	var b strings.Builder
	for _, dir := range []string{"cmd/appdeploy", "internal"} {
		_ = filepath.Walk(filepath.Join(root(), dir), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				data, _ := os.ReadFile(p)
				b.Write(data)
			}
			return nil
		})
	}
	return b.String()
}

func TestMessagesInTheDocsExistInTheCode(t *testing.T) {
	code := goSources(t)
	doc := read(t, "docs/troubleshooting.md")
	for _, p := range phrases {
		if !strings.Contains(code, p) {
			t.Errorf("the message fragment %q is not in the code any more: update docs/troubleshooting.md and this list", p)
		}
		if !strings.Contains(doc, p) {
			t.Errorf("docs/troubleshooting.md does not explain %q", p)
		}
	}
}

// ---- the LLM files ----

func TestLLMFilesAreComplete(t *testing.T) {
	idx := read(t, "llms.txt")
	for _, f := range append(markdownFiles(t), "llms-full.txt", "SPEC.md") {
		if !strings.Contains(idx, rawPrefix+f) {
			t.Errorf("llms.txt does not list %s", f)
		}
	}
	readme := read(t, "README.md")
	for _, f := range glob(t, "docs/*.md") {
		if !strings.Contains(readme, f) {
			t.Errorf("README.md does not link to %s", f)
		}
	}
}

// fullText is what llms-full.txt must contain: the documentation, in reading order.
func fullText(t *testing.T) string {
	order := []string{"README.md", "docs/llm-guide.md", "docs/getting-started.md", "docs/concepts.md", "docs/commands.md",
		"docs/configuration.md", "docs/api.md", "docs/groups.md", "docs/operations.md", "docs/troubleshooting.md",
		"docs/migration.md", "docs/comparison.md"}
	have := map[string]bool{}
	for _, f := range order {
		have[f] = true
	}
	for _, f := range glob(t, "docs/*.md") {
		if !have[f] {
			t.Fatalf("%s is not in the order list of llms-full.txt (add it in fullText)", f)
		}
	}
	var b strings.Builder
	b.WriteString("# appdeploy: full documentation\n\nGenerated from README.md and docs/*.md; do not edit. The index is llms.txt.\n")
	for _, f := range order {
		b.WriteString("\n---\n\n<!-- file: " + f + " -->\n\n")
		b.WriteString(strings.TrimSpace(read(t, f)))
		b.WriteString("\n")
	}
	return b.String()
}

func TestFullTextIsUpToDate(t *testing.T) {
	want := fullText(t)
	path := filepath.Join(root(), "llms-full.txt")
	if os.Getenv("UPDATE_DOCS") != "" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, _ := os.ReadFile(path)
	if string(got) != want {
		t.Fatalf("llms-full.txt is out of date: run  UPDATE_DOCS=1 go test ./internal/docs")
	}
}
