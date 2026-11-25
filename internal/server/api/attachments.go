package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// inlineTypes are the files a browser may show in place: pictures that run no script.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func (s *Server) attachmentsOff(w http.ResponseWriter) bool {
	if s.Conversations.Attachments == nil {
		writeError(w, http.StatusServiceUnavailable, "this server keeps no files (GATOR_ATTACHMENTS_DIR or GATOR_ATTACHMENTS_S3_*)", "unavailable")
		return true
	}
	return false
}

// UploadAttachment takes one file into a conversation, from multipart/form-data.
func (s *Server) UploadAttachment(w http.ResponseWriter, r *http.Request) {
	p, ok := s.conversationCaller(w, r)
	if !ok || s.attachmentsOff(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, conversations.MaxAttachmentBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, conversations.ErrTooLarge.Error(), "too_large")
			return
		}
		writeError(w, http.StatusBadRequest, "send multipart/form-data with conversationId and file", "invalid")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	var conv pgtype.UUID
	if err := conv.Scan(r.FormValue("conversationId")); err != nil {
		writeError(w, http.StatusBadRequest, "conversationId is required", "invalid")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required", "invalid")
		return
	}
	defer file.Close()
	contentType, err := sniff(file, header.Header.Get("Content-Type"))
	if err != nil {
		s.fail(w, err)
		return
	}
	att, err := s.Conversations.Attachments.Upload(r.Context(), p, conv, conversations.Upload{
		Name: header.Filename, ContentType: contentType, Size: header.Size, Body: file,
	})
	if errors.Is(err, conversations.ErrTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error(), "too_large")
		return
	}
	if err != nil {
		s.failConversation(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAttachment(att))
}

// sniff trusts a file's declared type unless it says nothing; then it looks at the bytes.
func sniff(f io.ReadSeeker, declared string) (string, error) {
	if mt, _, err := mime.ParseMediaType(declared); err == nil && mt != "application/octet-stream" {
		return mt, nil
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	mt, _, _ := mime.ParseMediaType(http.DetectContentType(head[:n]))
	return mt, nil
}

// DownloadAttachment answers a file's bytes. Pictures show inline; everything else downloads,
// and nothing is sniffed into something a browser would run.
func (s *Server) DownloadAttachment(w http.ResponseWriter, r *http.Request, attachmentId gen.AttachmentId) {
	p, ok := s.conversationCaller(w, r)
	if !ok || s.attachmentsOff(w) {
		return
	}
	att, body, err := s.Conversations.Attachments.Open(r.Context(), p, fromUUID(attachmentId))
	if err != nil {
		s.failConversation(w, err)
		return
	}
	defer body.Close()
	disposition := "attachment"
	if inlineTypes[att.ContentType] {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(att.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": att.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func toAttachment(a db.MessageAttachment) gen.Attachment {
	return gen.Attachment{Id: toUUID(a.ID), ConversationId: toUUID(a.ConversationID), MessageId: toUUIDPtr(a.MessageID),
		Name: a.Name, ContentType: a.ContentType, SizeBytes: a.SizeBytes, CreatedAt: a.CreatedAt.Time}
}
