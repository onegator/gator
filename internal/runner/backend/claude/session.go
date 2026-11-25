//go:build unix

package claude

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// Modes of the backend. The zero value is ModePrint; the runner runs ModeSession unless
// GATOR_CLAUDE_MODE=print says otherwise.
const (
	// ModePrint is `claude -p <prompt>`: one process per turn, a steer restarts it with --resume,
	// a stop kills it, nothing is asked before a tool.
	ModePrint = "print"
	// ModeSession is one process per run with stream-json on both sides (Claude Code's control
	// protocol, the one its Agent SDK speaks): a person's message goes into the running turn as a
	// user line, a stop is an interrupt request, and the CLI asks the runner before a tool its
	// permission mode does not settle (can_use_tool).
	ModeSession = "session"
)

// UnreadWait is how long the input stays open after a turn that left a message not seen read: the
// CLI reads what is queued before it exits, so closing only stops later steers going in live.
const UnreadWait = 30 * time.Second

// InterruptWait is how long a stop waits for the turn to end after the interrupt request, before
// the process is closed and, after the grace period, killed.
const InterruptWait = 2 * time.Second

// live says whether runs keep their session open.
func (b Backend) live() bool {
	return b.Mode == ModeSession && (b.Fallback == nil || !b.Fallback.Load())
}

// sessionCapabilities are what a session can do: steer and stop without a restart, ask before a
// tool (and lose what it asked if the process dies), take images.
var sessionCapabilities = proto.BackendCapabilities{ActiveSteering: true, NativeInterrupt: true, ToolApprovals: true,
	ApprovalsLiveOnly: true, Attachments: true, Checkpoints: true, Enforcement: proto.EnforcementNative}

// session is one run of a live session: the process, what was written to it, and how far it got.
type session struct {
	p      *proc
	spec   backend.Spec
	emit   backend.Emit
	parser *Parser

	// writeMu orders the writes; stateMu guards closed, and is never held across a write, so the
	// input can be closed (which ends a write blocked on a full pipe) while a write waits.
	writeMu sync.Mutex
	stateMu sync.Mutex
	closed  bool          // stdin is closed: nothing more goes in
	closing chan struct{} // closed with it

	mu sync.Mutex
	// pending are the user messages written and not yet known read, in order; replays says
	// whether the CLI replays them (--replay-user-messages) at all. unread are the steers taken
	// from the caller and not delivered: the session closed, or the CLI died before reading them.
	pending   []outgoing
	replays   bool
	unread    []string
	sessionID string
	turnEnded chan struct{}
	seq       atomic.Int64   // request ids of ours
	answers   sync.WaitGroup // control requests of the CLI's being answered
	// asking are the answers still being sought, by the CLI's request id, to stop when it cancels.
	asking map[string]context.CancelFunc
}

// outgoing is one user message written into the session: a steer is the caller's (reported
// applied once read), results counts the turns that ended since it was written.
type outgoing struct {
	text    string
	steer   bool
	results int
}

// runSession runs one prompt in a live session and every message a person adds while it runs.
func (b Backend) runSession(ctx context.Context, spec backend.Spec, emit backend.Emit) backend.Outcome {
	common, cleanup, err := b.commonArgs(spec)
	if err != nil {
		return failed(err.Error())
	}
	defer cleanup()
	args := append([]string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--replay-user-messages"}, common...)
	args = append(args, sessionPermissionArgs(spec, mcpNames(spec.MCP), hasSkills(common))...)

	p, err := b.spawn(spec, args, true)
	if err != nil {
		return failed(err.Error())
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s := &session{p: p, spec: spec, emit: emit, parser: &Parser{}, turnEnded: make(chan struct{}, 1), closing: make(chan struct{})}

	// The reader and the supervisor run before anything is written: a CLI that answers the first
	// message at once, or never reads it, is heard and stopped.
	parsed := make(chan struct{})
	go func() {
		defer close(parsed)
		if err := p.lines(func(line []byte) { s.line(runCtx, cancel, line) }); err != nil {
			cancel(fmt.Errorf("%w: %v", errOutput, err))
		}
		s.closeInput() // the CLI is gone: nothing more can go in
	}()
	exited := make(chan struct{})
	go s.supervise(runCtx, exited, b.grace())
	if err := s.say(spec.Prompt, false, images(spec.Attachments)...); err != nil {
		cancel(err)
	}
	var steering sync.WaitGroup
	steering.Add(1)
	go func() {
		defer steering.Done()
		s.steer(runCtx, exited)
	}()

	waitErr := p.cmd.Wait()
	close(exited)
	p.release(parsed)
	s.closeInput()
	fallback, why := b.fallBack(s, waitErr)
	var out backend.Outcome
	if !fallback {
		out = b.outcome(runCtx, spec, s.parser, p, waitErr)
	}
	// A question still waiting for a person when the process ended is not going to be answered
	// here: its approver is told, and the run returns once every answer gave up.
	cancel(errors.New("the session ended"))
	s.answers.Wait()
	steering.Wait() // a steer it took as the run ended is in unread by now
	unread := s.undelivered()
	if fallback {
		emit("backend_fallback", map[string]any{"mode": ModePrint, "reason": why})
		if len(spec.Access.Ask) > 0 {
			// Print mode asks nobody: what the policy asks about is refused (permissionArgs).
			emit("approvals_held_as_denials", map[string]any{"rules": len(spec.Access.Refused())})
		}
		out = b.runPrint(ctx, spec, emit)
	}
	out.Unread = append(unread, out.Unread...)
	return out
}

// undelivered are the steers the CLI never read: those taken when the session had closed, and
// those written but neither replayed nor followed by the end of a turn (the CLI died first).
func (s *session) undelivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string{}, s.unread...)
	for _, o := range s.pending {
		if o.steer && o.results == 0 {
			out = append(out, o.text)
		}
	}
	return out
}

// fallBack says whether a session that never started was refused by a CLI that does not speak the
// protocol: then this backend runs in print mode from now on, and says so in its capabilities.
func (b Backend) fallBack(s *session, waitErr error) (bool, string) {
	if b.Fallback == nil || waitErr == nil || s.parser.SessionID != "" || s.parser.Result != nil {
		return false, ""
	}
	stderr := strings.ToLower(s.p.stderr.String())
	for _, sign := range []string{"unknown option", "unknown argument", "input-format", "replay-user-messages", "permission-prompts"} {
		if strings.Contains(stderr, sign) {
			b.Fallback.Store(true)
			return true, clip(strings.TrimSpace(s.p.stderr.String()), 300)
		}
	}
	return false, ""
}

// supervise stops the session when the run's context ends: an interrupt first, which ends the turn
// in the CLI and keeps the process; then its input closes, it gets the grace period to exit, and
// after that its group is killed.
func (s *session) supervise(ctx context.Context, exited <-chan struct{}, grace time.Duration) {
	select {
	case <-exited:
		return
	case <-ctx.Done():
	}
	select {
	case <-s.turnEnded: // an earlier turn's end is not this one's
	default:
	}
	if err := s.control(map[string]any{"subtype": "interrupt"}); err == nil {
		select {
		case <-s.turnEnded:
		case <-exited:
			return
		case <-time.After(InterruptWait):
		}
	}
	s.closeInput()
	select {
	case <-exited:
	case <-time.After(grace):
		s.p.terminate(exited, grace)
	}
}

// steer writes what a person says into the running session, while it is open. A message taken
// once the session closed is kept as unread, for the caller to send in the next run.
func (s *session) steer(ctx context.Context, exited <-chan struct{}) {
	if s.spec.Live == nil || s.spec.Live.Steer == nil {
		return
	}
	for {
		select {
		case <-exited:
			return
		case <-ctx.Done():
			return
		case <-s.closing:
			return
		case msg := <-s.spec.Live.Steer:
			// Applied is said once the CLI read it (replayed, or a turn ended after it), not when
			// it was written: a CLI that dies first never read it, and it goes back as unread.
			if err := s.say(msg, true); err != nil {
				s.mu.Lock()
				s.unread = append(s.unread, msg)
				s.mu.Unlock()
				return
			}
		}
	}
}

// line handles one line from the CLI: a control request (a permission prompt) or a reply to ours,
// a user message the CLI replays, or the stream every run has.
func (s *session) line(ctx context.Context, cancel context.CancelCauseFunc, raw []byte) {
	var head struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Message   json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(raw, &head)
	switch head.Type {
	case "control_request":
		s.answers.Add(1)
		go func() {
			defer s.answers.Done()
			s.answer(ctx, head.RequestID, head.Request)
		}()
		return
	case "control_cancel_request":
		// The CLI no longer needs an answer (the tool call was abandoned): whoever is asked is told.
		s.cancelAnswer(head.RequestID)
		return
	case "control_response", "keep_alive":
		return
	case "user":
		if s.replayed(head.Message) {
			return
		}
	}
	_ = s.parser.Line(raw, func(typ string, payload any) { s.emit(typ, payload) })
	s.mu.Lock()
	s.sessionID = s.parser.SessionID
	s.mu.Unlock()
	if s.spec.MaxToolCalls > 0 && s.parser.ToolCalls > s.spec.MaxToolCalls && ctx.Err() == nil {
		cancel(errToolLimit)
	}
	if head.Type == "result" {
		s.ended(ctx)
	}
}

// ended is the end of a turn: the worktree is checkpointed, and when nothing a person said is
// still to be read the session's input closes, so the CLI exits once it is done.
func (s *session) ended(ctx context.Context) {
	select {
	case s.turnEnded <- struct{}{}:
	default:
	}
	if s.spec.Live != nil && s.spec.Live.TurnEnded != nil {
		s.spec.Live.TurnEnded()
	}
	// What is queued is read before the CLI exits at the end of its input, so closing is always
	// safe; it only means that what a person says from now on goes in a resumed run instead. A CLI
	// that does not replay what it read cannot say what is still queued: its input closes at once.
	s.mu.Lock()
	kept := s.pending[:0]
	var applied []string
	for _, o := range s.pending {
		o.results++
		if !s.replays {
			// It cannot say what it read: what was written before a turn ended is taken as read.
			if o.steer {
				applied = append(applied, o.text)
			}
			continue
		}
		kept = append(kept, o)
	}
	s.pending = kept
	done := len(s.pending) == 0 || !s.replays
	s.mu.Unlock()
	for _, m := range applied {
		s.emit("steer_applied", map[string]any{"message": m, "live": true})
	}
	if done || ctx.Err() != nil {
		s.closeInput()
		return
	}
	// Something written is not seen read yet: the next turn should read it. A text the CLI
	// replays changed (or merged with another) is never matched, so the input closes after a
	// while anyway rather than leave the CLI waiting for input for good.
	time.AfterFunc(UnreadWait, s.closeInput)
}

// replayed says whether a user line is one of ours, replayed as the CLI read it.
func (s *session) replayed(message json.RawMessage) bool {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(message, &m) != nil {
		return false
	}
	text := ""
	for _, b := range blocks(m.Content) {
		if b.Type == "tool_result" {
			return false
		}
		if b.Type == "text" && text == "" {
			text = b.Text
		}
	}
	s.mu.Lock()
	for i, o := range s.pending {
		if o.text == text {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			s.replays = true
			s.mu.Unlock()
			if o.steer {
				s.emit("steer_applied", map[string]any{"message": o.text, "live": true})
			}
			return true
		}
	}
	s.mu.Unlock()
	return false
}

// images are the attachments shown to the agent as images: content blocks of the first message.
// One that cannot be read is left to its path in the prompt.
func images(files []backend.Attachment) []map[string]any {
	var out []map[string]any
	for _, f := range files {
		if !f.Image {
			continue
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": f.MediaType,
			"data": base64.StdEncoding.EncodeToString(data)}})
	}
	return out
}

// say writes a user message into the session: its text, and blocks after it (images). A steer is
// the caller's, reported applied once read.
func (s *session) say(text string, steer bool, more ...map[string]any) error {
	content := append([]map[string]any{{"type": "text", "text": text}}, more...)
	s.mu.Lock()
	msg := map[string]any{"type": "user", "session_id": s.sessionID, "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": content}}
	s.pending = append(s.pending, outgoing{text: text, steer: steer})
	s.mu.Unlock()
	if err := s.write(msg); err != nil {
		s.mu.Lock()
		for i := len(s.pending) - 1; i >= 0; i-- {
			if s.pending[i].text == text {
				s.pending = append(s.pending[:i], s.pending[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// control sends a control request of ours.
func (s *session) control(request map[string]any) error {
	return s.write(map[string]any{"type": "control_request", "request_id": "gator-" + strconv.FormatInt(s.seq.Add(1), 10), "request": request})
}

var errInputClosed = errors.New("the session takes no more input")

func (s *session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.inputClosed() {
		return errInputClosed
	}
	_, err = s.p.stdin.Write(append(b, '\n'))
	return err
}

func (s *session) inputClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

// closeInput closes the session's input, without waiting for a write: closing the pipe ends a write
// that blocks on it.
func (s *session) closeInput() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.closing)
		_ = s.p.stdin.Close()
	}
}

// cancelAnswer stops seeking the answer to a request the CLI cancelled.
func (s *session) cancelAnswer(id string) {
	s.mu.Lock()
	cancel := s.asking[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// questionTool is Claude Code's tool that asks the person a question.
const questionTool = "AskUserQuestion"

// answer answers one control request of the CLI's. A permission prompt goes to the job's approver;
// without one, or for anything else, the answer is no: a missing approver is never a yes.
func (s *session) answer(runCtx context.Context, id string, raw json.RawMessage) {
	ctx, cancel := context.WithCancel(runCtx)
	defer cancel()
	s.mu.Lock()
	if s.asking == nil {
		s.asking = map[string]context.CancelFunc{}
	}
	s.asking[id] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.asking, id)
		s.mu.Unlock()
	}()
	var req struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.Subtype != "can_use_tool" {
		_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id,
			"error": fmt.Sprintf("%q is not offered by this host", req.Subtype)}})
		return
	}
	kind := backend.RequestToolApproval
	if req.ToolName == questionTool {
		kind = backend.RequestQuestion
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}
	d := backend.Decision{Message: "Nobody can approve tools in this session, so this one is refused."}
	if s.spec.Live != nil && s.spec.Live.Approve != nil {
		d = s.spec.Live.Approve.Decide(ctx, backend.ToolRequest{Kind: kind, Tool: req.ToolName, Input: req.Input, ToolUseID: req.ToolUseID})
	}
	if ctx.Err() != nil && runCtx.Err() == nil {
		return // the CLI cancelled its request: it wants no answer
	}
	s.emit("permission", map[string]any{"tool": req.ToolName, "tool_use_id": req.ToolUseID, "allowed": d.Allow, "stop": d.Stop,
		"kind": kind, "reason": clip(d.Message, 500)})
	behaviour := map[string]any{"behavior": "allow", "updatedInput": req.Input}
	if !d.Allow {
		msg := d.Message
		if kind == backend.RequestQuestion && d.Answer != "" {
			msg = "The question went to a person; their answer follows as a message."
		}
		if msg == "" {
			msg = "Refused."
		}
		behaviour = map[string]any{"behavior": "deny", "message": msg, "interrupt": d.Stop}
	}
	_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": behaviour}})
	if kind == backend.RequestQuestion && d.Answer != "" && !d.Stop {
		_ = s.say("A person answered your question:\n\n"+d.Answer, false)
	}
}

// sessionPermissionArgs are permissionArgs for a session, with who answers what the permission
// mode does not settle: nobody for a job that only reads (anything that would prompt is denied),
// the runner — and through it a person — for a job that works and has an approver.
func sessionPermissionArgs(spec backend.Spec, mcp []string, skills bool) []string {
	args := permissionArgs(spec.Access, mcp, skills)
	if spec.Access.ReadOnly || spec.Access.NoTools || spec.Live == nil || spec.Live.Approve == nil {
		return append(args, "--permission-prompts", "none")
	}
	// A job with an approver asks about everything its mode does not allow, and the runner
	// settles all but what the project's approvals policy names (executor's approver); what the
	// policy denies is denied by the CLI too.
	args = []string{"--permission-mode", "manual", "--permission-prompts", "host"}
	if len(spec.Access.Deny) > 0 {
		args = append(args, "--disallowedTools", strings.Join(cliRules(spec.Access.Deny), ","))
	}
	return args
}
