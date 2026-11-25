package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/api/gen"
)

// upload sends one file into a conversation as multipart/form-data.
func (h *harness) upload(token, conversation, name string, content []byte) gen.Attachment {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("conversationId", conversation)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out gen.Attachment
	if res.StatusCode != 201 || json.NewDecoder(res.Body).Decode(&out) != nil {
		h.t.Fatalf("upload: %d", res.StatusCode)
	}
	return out
}

// get is a GET with a token, answering the status, the body and the content type.
func (h *harness) get(token, path string) (int, []byte, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header.Get("Content-Type")
}

// A screenshot sent with a question reaches the agent that answers: the reply's lease names it, and
// the job's own identity fetches it — only it, and only from the conversation the reply answers
// in, never as a page.
func TestFilesSentWithAnAskReachTheAgent(t *testing.T) {
	h := newHarness(t)
	backend := uniqueBackend()
	task := h.task()
	project := task.ProjectId.String()
	if _, err := h.pool.Exec(context.Background(), "UPDATE projects SET process_config = $2 WHERE id = $1",
		project, `{"autopilot":{"backend":"`+backend+`"}}`); err != nil {
		t.Fatal(err)
	}
	general := h.generalOf(project)
	root := h.say(project, general.Id.String(), "checkout crashes")
	var thread gen.ConversationDetail
	h.do("GET", "/messages/"+root.Id.String()+"/thread", nil, &thread)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	shot := h.upload(h.admin, thread.Id.String(), "crash.png", png)
	elsewhere := h.upload(h.admin, general.Id.String(), "other.txt", []byte("not yours"))
	h.do("POST", "/conversations/"+general.Id.String()+"/messages", gen.NewMessage{Body: ptr("see this"), Attachments: &[]openapi_types.UUID{elsewhere.Id}}, nil)
	body, mention := "@reviewer what is this?", []gen.MemberRef{{Kind: gen.MemberRefKindAgent, Id: h.agentID(project, "reviewer")}}
	var ask gen.Message
	if code := h.do("POST", "/conversations/"+thread.Id.String()+"/messages", gen.NewMessage{Body: &body, Mentions: &mention,
		Attachments: &[]openapi_types.UUID{shot.Id}}, &ask); code != 201 {
		t.Fatalf("ask: %d", code)
	}
	turn := h.turnOf(ask, project, "reviewer")
	raw, _ := h.dialRaw(h.runner, backend, proto.Version, 1)
	jobs := raw.lease(1)
	if len(jobs) != 1 || jobs[0].JobID != *turn.Job {
		t.Fatalf("lease: %+v", jobs)
	}
	j := jobs[0]
	if len(j.Attachments) != 1 || j.Attachments[0].ID != shot.Id.String() || j.Attachments[0].Name != "crash.png" || j.Attachments[0].Size != int64(len(png)) {
		t.Fatalf("attachments: %+v", j.Attachments)
	}
	code, got, typ := h.get(j.AgentToken, proto.AttachmentPath+shot.Id.String())
	if code != 200 || !bytes.Equal(got, png) || typ != "application/octet-stream" {
		t.Fatalf("fetch: %d %q", code, typ)
	}
	if code, _, _ := h.get(j.AgentToken, proto.AttachmentPath+elsewhere.Id.String()); code != 404 {
		t.Fatalf("a file of another conversation: %d", code)
	}
	if code, _, _ := h.get(h.admin, proto.AttachmentPath+shot.Id.String()); code != 403 {
		t.Fatalf("a person through the agent's door: %d", code)
	}
}
