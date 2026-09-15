// Command gator-plugin helps write Gator plugins without a server:
//
//	gator-plugin new <name> [dir]                            write a plugin skeleton
//	gator-plugin manifest -- <plugin command...>             print the validated manifest
//	gator-plugin dev [-json] [-timeout 30s] <scenario.json> -- <plugin command...>
//
// dev starts the plugin against an in-memory core, plays the scenario's hooks in order and
// prints what the plugin did. It exits 1 when a step misses its expectation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/onegator/gator/plugin/plugintest"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gator-plugin:", err)
	}
	os.Exit(code)
}

const usage = `usage:
  gator-plugin new <name> [dir]
  gator-plugin manifest -- <plugin command...>
  gator-plugin dev [-json] [-timeout 30s] <scenario.json> -- <plugin command...>`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return 2, errors.New(usage)
	}
	switch args[0] {
	case "new":
		if len(args) < 2 {
			return 2, errors.New(usage)
		}
		dir := args[1]
		if len(args) > 2 {
			dir = args[2]
		}
		files, err := scaffold(args[1], dir)
		if err != nil {
			return 1, err
		}
		for _, f := range files {
			fmt.Fprintln(stdout, "wrote", f)
		}
		return 0, nil
	case "manifest":
		cmd := afterDashes(args[1:])
		if len(cmd) == 0 {
			return 2, errors.New(usage)
		}
		p, err := plugintest.Start(ctx, cmd, plugintest.NewCore(""), plugintest.Options{Stderr: stderr, Timeout: 10 * time.Second})
		if err != nil {
			return 1, err
		}
		defer p.Close()
		b, _ := json.MarshalIndent(p.Manifest, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return 0, nil
	case "dev":
		fs := flag.NewFlagSet("dev", flag.ContinueOnError)
		fs.SetOutput(stderr)
		asJSON := fs.Bool("json", false, "print the report as JSON")
		timeout := fs.Duration("timeout", 30*time.Second, "per-call timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, err
		}
		rest := fs.Args()
		cmd := afterDashes(rest)
		if len(rest) == 0 || len(cmd) == 0 || rest[0] == "--" {
			return 2, errors.New(usage)
		}
		s, err := plugintest.LoadScenario(rest[0])
		if err != nil {
			return 1, err
		}
		rep, err := plugintest.Run(ctx, cmd, s, stderr, *timeout)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Fprintln(stdout, string(b))
		} else {
			printReport(stdout, rep)
		}
		if rep.Failed() {
			return 1, nil
		}
		return 0, nil
	}
	return 2, errors.New(usage)
}

func afterDashes(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func printReport(w io.Writer, r plugintest.Report) {
	m := r.Manifest
	fmt.Fprintf(w, "plugin %s %s · capabilities %s · hooks %s\n\n", m.Name, m.Version, list(m.Capabilities), list(m.Hooks))
	for i, s := range r.Steps {
		status := "ok"
		if s.Error != "" {
			status = "error: " + s.Error
		}
		fmt.Fprintf(w, "%d. %s  %s\n", i+1, s.Hook, status)
		for _, c := range s.CoreCalls {
			line := fmt.Sprintf("     → %s %s", c.Method, short(string(c.Params)))
			if c.Error != "" {
				line += "  ✗ " + c.Error
			}
			fmt.Fprintln(w, line)
		}
		if len(s.Result) > 0 {
			fmt.Fprintf(w, "     ← %s\n", short(string(s.Result)))
		}
		if s.Problem != "" {
			fmt.Fprintf(w, "     FAIL: %s\n", s.Problem)
		}
	}
	fmt.Fprintln(w, "\nstate:")
	for _, t := range r.Tasks {
		refs, _ := json.Marshal(t.ExternalRefs)
		fmt.Fprintf(w, "  task %s  %s/%s  %q  refs %s\n", t.ID, t.Kind, t.Phase, t.Title, refs)
	}
	ids := make([]string, 0, len(r.Checks))
	for id := range r.Checks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, c := range r.Checks[id] {
			fmt.Fprintf(w, "  check %s on %s: %s %s\n", c.Name, id, c.Status, c.Detail)
		}
	}
	for _, a := range r.Artifacts {
		fmt.Fprintf(w, "  artifact %s v%d on %s/%s: %s\n", a.Type, a.Version, a.TaskID, a.Phase, short(a.Content+a.URL))
	}
	keys := make([]string, 0, len(r.KV))
	for k := range r.KV {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  kv %s = %s\n", k, short(string(r.KV[k])))
	}
	for _, l := range r.Logs {
		fmt.Fprintf(w, "  log %s: %s\n", l.Level, l.Message)
	}
	if r.Failed() {
		fmt.Fprintln(w, "\nFAILED")
	} else {
		fmt.Fprintln(w, "\nPASSED")
	}
}

func list(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
