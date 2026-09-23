// Command gator-cli is how an agent talks back to Gator while it works. The runner puts it on
// the path of every job with that job's own identity, so an agent can say it is blocked, ask a
// person a question, record a document or propose what the work taught the product — instead of
// guessing for an hour and reporting the guess.
//
// Everything it prints on stdout is JSON, one object or one array, so a model can read it
// without parsing prose. Problems go to stderr as {"error": "..."} and the exit code says what
// kind of problem it was.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Exit codes, so a script around the agent can tell the cases apart without reading the text.
const (
	exitOK       = 0
	exitUserErr  = 1 // the request was wrong: a missing argument, a refused value
	exitNetwork  = 2 // the server could not be reached
	exitAuth     = 3 // no identity, or one that has expired with the job
	exitOther    = 4 // anything else, including the server failing
	exitConflict = 5 // the work moved on: the task is closed, the phase changed
)

const usage = `gator-cli — talk back to Gator about the job you are working on.

  gator-cli task show                     what this job is about, with its documents
  gator-cli task blocked --reason TEXT    stop and ask a person, instead of guessing
  gator-cli ask --question TEXT           ask a person a question and stop there
  gator-cli artifact put --type TYPE [--content TEXT | --file PATH | -]
  gator-cli product propose --kind decision|lesson --title TEXT [--content TEXT | --file PATH | -]
  gator-cli catalogue show                the parts this project is made of
  gator-cli knowledge search --query TEXT [--limit N]

Reads GATOR_URL and GATOR_JOB_TOKEN from the environment; the runner sets both.
Output is JSON on stdout. Exit codes: 0 ok, 1 your request, 2 network, 3 identity, 4 server, 5 the work moved on.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		var coded *codedError
		if errors.As(err, &coded) {
			fail(coded.code, coded.Error())
		}
		fail(exitOther, err.Error())
	}
}

func fail(code int, message string) {
	out, _ := json.Marshal(map[string]string{"error": message})
	fmt.Fprintln(os.Stderr, string(out))
	os.Exit(code)
}

type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func coded(code int, format string, args ...any) error {
	return &codedError{code: code, msg: fmt.Sprintf(format, args...)}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	command := args[0]
	rest := args[1:]
	sub := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub, rest = rest[0], rest[1:]
	}
	c, err := clientFromEnv()
	if err != nil {
		return err
	}
	switch command + " " + sub {
	case "task show":
		return c.get("/agent/task", nil)
	case "task blocked":
		reason, err := textFlag(rest, "reason", "say why in a sentence a person can act on")
		if err != nil {
			return err
		}
		return c.post("/agent/blocked", map[string]any{"reason": reason})
	case "ask ":
		question, err := textFlag(rest, "question", "ask something")
		if err != nil {
			return err
		}
		return c.post("/agent/question", map[string]any{"reason": question})
	case "artifact put":
		fs := flag.NewFlagSet("artifact put", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		typ := fs.String("type", "", "brief | plan | report | review | note")
		content := fs.String("content", "", "the document itself")
		file := fs.String("file", "", "read the document from a file, or - for stdin")
		if err := fs.Parse(rest); err != nil {
			return coded(exitUserErr, "%v", err)
		}
		body, err := contentFrom(*content, *file, fs.Args())
		if err != nil {
			return err
		}
		if strings.TrimSpace(*typ) == "" {
			return coded(exitUserErr, "--type is required: brief, plan, report, review or note")
		}
		return c.post("/agent/artifacts", map[string]any{"type": *typ, "content": body})
	case "product propose":
		fs := flag.NewFlagSet("product propose", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		kind := fs.String("kind", "", "decision | lesson")
		title := fs.String("title", "", "one line, what it is about")
		content := fs.String("content", "", "the entry itself")
		file := fs.String("file", "", "read the entry from a file, or - for stdin")
		if err := fs.Parse(rest); err != nil {
			return coded(exitUserErr, "%v", err)
		}
		if *kind != "decision" && *kind != "lesson" {
			return coded(exitUserErr, "--kind must be decision or lesson")
		}
		if strings.TrimSpace(*title) == "" {
			return coded(exitUserErr, "--title is required")
		}
		body, err := contentFrom(*content, *file, fs.Args())
		if err != nil {
			return err
		}
		return c.post("/agent/product", map[string]any{"kind": *kind, "title": *title, "content": body})
	case "catalogue show":
		return c.get("/agent/catalogue", nil)
	case "knowledge search":
		fs := flag.NewFlagSet("knowledge search", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		query := fs.String("query", "", "words to look for")
		limit := fs.Int("limit", 10, "how many documents at most")
		if err := fs.Parse(rest); err != nil {
			return coded(exitUserErr, "%v", err)
		}
		if strings.TrimSpace(*query) == "" {
			return coded(exitUserErr, "--query is required")
		}
		return c.get("/agent/knowledge", url.Values{"q": {*query}, "limit": {fmt.Sprint(*limit)}})
	}
	return coded(exitUserErr, "unknown command %q; run gator-cli help", strings.TrimSpace(command+" "+sub))
}

// textFlag reads one required text flag, accepting "-" to mean stdin so an agent can pass a
// paragraph without fighting its own shell quoting.
func textFlag(args []string, name, missing string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	value := fs.String(name, "", missing)
	if err := fs.Parse(args); err != nil {
		return "", coded(exitUserErr, "%v", err)
	}
	text, err := contentFrom(*value, "", fs.Args())
	if err != nil {
		return "", coded(exitUserErr, "--%s is required: %s", name, missing)
	}
	return text, nil
}

// contentFrom takes the text from a flag, a file, or stdin when either is "-" or the only
// remaining argument is "-".
func contentFrom(value, file string, rest []string) (string, error) {
	switch {
	case value == "-" || file == "-" || (value == "" && file == "" && len(rest) == 1 && rest[0] == "-"):
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", coded(exitUserErr, "reading stdin: %v", err)
		}
		value = string(b)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", coded(exitUserErr, "reading %s: %v", file, err)
		}
		value = string(b)
	}
	if strings.TrimSpace(value) == "" {
		return "", coded(exitUserErr, "nothing to send")
	}
	return value, nil
}

type client struct {
	base  string
	token string
	http  *http.Client
}

func clientFromEnv() (*client, error) {
	base := strings.TrimRight(os.Getenv("GATOR_URL"), "/")
	token := os.Getenv("GATOR_JOB_TOKEN")
	if base == "" || token == "" {
		return nil, coded(exitAuth, "no identity: this runs inside a Gator job, which sets GATOR_URL and GATOR_JOB_TOKEN")
	}
	return &client{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *client) get(path string, query url.Values) error {
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return coded(exitUserErr, "%v", err)
	}
	return c.do(req)
}

func (c *client) post(path string, body any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return coded(exitUserErr, "%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *client) do(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return coded(exitNetwork, "cannot reach Gator at %s: %v", c.base, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 == 2 {
		os.Stdout.Write(append(bytes.TrimSpace(raw), '\n'))
		return nil
	}
	message := strings.TrimSpace(string(raw))
	var problem struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &problem) == nil && problem.Error != "" {
		message = problem.Error
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return coded(exitAuth, "%s", message)
	case res.StatusCode == http.StatusConflict:
		return coded(exitConflict, "%s", message)
	case res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound:
		return coded(exitUserErr, "%s", message)
	}
	return coded(exitOther, "Gator answered %d: %s", res.StatusCode, message)
}
