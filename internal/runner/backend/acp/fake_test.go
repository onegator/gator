package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The tests run this test binary as the agent: with ACPFAKE_SCRIPT set it is a fake ACP agent
// that plays a script from testdata, and writes down everything the client sent it (and the
// answers to its permission requests) in ACPFAKE_LOG. No real agent, and no model, ever runs.

func TestMain(m *testing.M) {
	if script := os.Getenv("ACPFAKE_SCRIPT"); script != "" {
		os.Exit(fakeAgent(script, os.Getenv("ACPFAKE_LOG")))
	}
	os.Exit(m.Run())
}

// script is what the fake agent does. Its answers are the agent's results, word for word.
type script struct {
	Initialize      json.RawMessage `json:"initialize"`
	NewSession      json.RawMessage `json:"new_session"`
	NewSessionError *RPCError       `json:"new_session_error"`
	LoadSession     json.RawMessage `json:"load_session"`
	// LoadReplay are updates the agent sends while loading: the session's history.
	LoadReplay []json.RawMessage `json:"load_replay"`
	// LoadTurn is the turn a loaded session goes on with: the process is new, the session is not.
	LoadTurn int    `json:"load_turn"`
	Turns    []turn `json:"turns"`
	// ExitAfterInit makes the agent die after initialize, saying why on stderr.
	ExitAfterInit string `json:"exit_after_init"`
	// Stdout lines written before anything else: an adapter that logs where it should not.
	Noise []string `json:"noise"`
}

type turn struct {
	Steps  []step          `json:"steps"`
	Result json.RawMessage `json:"result"`
}

type step struct {
	Update     json.RawMessage `json:"update"`
	Permission json.RawMessage `json:"permission"`
	// WaitCancel blocks the turn until the client cancels it, then ends it "cancelled".
	WaitCancel bool `json:"wait_cancel"`
	SleepMS    int  `json:"sleep_ms"`
}

func fakeAgent(path, logPath string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var s script
	if err := json.Unmarshal(raw, &s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	logf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var wmu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		os.Stdout.Write(append(b, '\n'))
	}
	record := func(v any) {
		if logf == nil {
			return
		}
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		logf.Write(append(b, '\n'))
	}
	record(map[string]any{"env": os.Environ()})
	for _, n := range s.Noise {
		fmt.Println(n)
	}
	cancelled := make(chan struct{}, 1)
	answers := map[string]chan json.RawMessage{}
	var amu sync.Mutex
	next := 1000
	ask := func(method string, params json.RawMessage) json.RawMessage {
		amu.Lock()
		next++
		id := strconv.Itoa(next)
		ch := make(chan json.RawMessage, 1)
		answers[id] = ch
		amu.Unlock()
		write(map[string]any{"jsonrpc": "2.0", "id": next, "method": method, "params": params})
		select {
		case r := <-ch:
			return r
		case <-time.After(10 * time.Second):
			return nil
		}
	}
	notify := func(params json.RawMessage) {
		write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": params})
	}
	turnAt := 0
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m rpcMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" { // an answer to one of ours
			amu.Lock()
			ch := answers[string(m.ID)]
			amu.Unlock()
			if ch != nil {
				record(map[string]any{"answer": json.RawMessage(m.Result)})
				ch <- m.Result
			}
			continue
		}
		record(map[string]any{"method": m.Method, "params": m.Params})
		reply := func(result json.RawMessage, e *RPCError) {
			if len(result) == 0 {
				result = json.RawMessage("null")
			}
			msg := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if e != nil {
				msg["error"] = e
			} else {
				msg["result"] = result
			}
			write(msg)
		}
		switch m.Method {
		case methodInitialize:
			reply(s.Initialize, nil)
			if s.ExitAfterInit != "" {
				fmt.Fprintln(os.Stderr, s.ExitAfterInit)
				return 3
			}
		case methodNewSession:
			if s.NewSessionError != nil {
				reply(nil, s.NewSessionError)
				continue
			}
			reply(s.NewSession, nil)
		case methodLoad:
			for _, u := range s.LoadReplay {
				notify(u)
			}
			turnAt = s.LoadTurn
			reply(s.LoadSession, nil)
		case methodSetMode, methodSetModel:
			reply(nil, nil)
		case methodCancel:
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case methodPrompt:
			if turnAt >= len(s.Turns) {
				reply(nil, &RPCError{Code: -32603, Message: "no more turns in the script"})
				continue
			}
			t := s.Turns[turnAt]
			turnAt++
			id := m.ID
			go func() {
				for _, st := range t.Steps {
					switch {
					case len(st.Update) > 0:
						notify(st.Update)
					case len(st.Permission) > 0:
						ask(methodPermission, st.Permission)
					case st.WaitCancel:
						select {
						case <-cancelled:
						case <-time.After(10 * time.Second):
						}
						write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "cancelled"}})
						return
					case st.SleepMS > 0:
						time.Sleep(time.Duration(st.SleepMS) * time.Millisecond)
					}
				}
				result := t.Result
				if len(result) == 0 {
					result = json.RawMessage(`{"stopReason":"end_turn"}`)
				}
				write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
			}()
		default:
			reply(nil, &RPCError{Code: -32601, Message: "unknown " + m.Method})
		}
	}
	return 0
}
