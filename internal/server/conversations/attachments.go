package conversations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// MaxAttachmentBytes is the largest file a message carries. Larger files stay in the team's own
// tools and come in through integrations.
const MaxAttachmentBytes = 25 << 20

const maxAttachmentName = 255

// ErrTooLarge answers a file over MaxAttachmentBytes.
var ErrTooLarge = fmt.Errorf("%w: a file is at most %d MB", ErrInvalid, MaxAttachmentBytes>>20)

// BlobStore keeps the bytes of uploaded files: an S3-compatible bucket, like the backups, or a
// directory on the server's disk.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Attachments takes files into a conversation and hands them back to its readers. A file is
// uploaded first and belongs to no message until its author posts the message that carries it.
type Attachments struct {
	pool   *pgxpool.Pool
	access *Access
	blobs  BlobStore
}

// NewAttachments builds the file keeper.
func NewAttachments(pool *pgxpool.Pool, access *Access, blobs BlobStore) *Attachments {
	return &Attachments{pool: pool, access: access, blobs: blobs}
}

// Upload is one file on its way in.
type Upload struct {
	Name        string
	ContentType string
	Size        int64
	Body        io.Reader
}

// Upload takes a file into a conversation the caller may write in.
func (a *Attachments) Upload(ctx context.Context, p auth.Principal, conversation pgtype.UUID, up Upload) (db.MessageAttachment, error) {
	conv, rights, err := a.access.RightsByID(ctx, conversation, p)
	if err != nil {
		return db.MessageAttachment{}, err
	}
	if !rights.Read {
		return db.MessageAttachment{}, ErrNotFound
	}
	me, ok := memberOf(p)
	if !ok || !rights.Write {
		return db.MessageAttachment{}, ErrForbidden
	}
	name := cleanName(up.Name)
	switch {
	case up.Size > MaxAttachmentBytes:
		return db.MessageAttachment{}, ErrTooLarge
	case up.Size < 0:
		return db.MessageAttachment{}, invalid("the file's size is unknown")
	case name == "":
		return db.MessageAttachment{}, invalid("a file needs a name")
	}
	contentType := up.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	key := fmt.Sprintf("attachments/%s/%s", uuidString(conv.ID), randomKey())
	if err := a.blobs.Put(ctx, key, io.LimitReader(up.Body, up.Size), up.Size, contentType); err != nil {
		return db.MessageAttachment{}, err
	}
	att, err := db.New(a.pool).CreateAttachment(ctx, db.CreateAttachmentParams{
		ConversationID: conv.ID, UploadedBy: me.ID, Name: name, ContentType: contentType, SizeBytes: up.Size, StorageKey: key,
	})
	if err != nil {
		_ = a.blobs.Delete(context.WithoutCancel(ctx), key)
		return db.MessageAttachment{}, err
	}
	return att, nil
}

// Open answers a file and its bytes, for a reader of the conversation it was sent in. A file not
// sent yet is its uploader's alone; a file whose message was deleted is gone.
func (a *Attachments) Open(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.MessageAttachment, io.ReadCloser, error) {
	q := db.New(a.pool)
	att, err := q.GetAttachment(ctx, id)
	if err != nil {
		return att, nil, err
	}
	_, rights, err := a.access.RightsByID(ctx, att.ConversationID, p)
	if err != nil {
		return att, nil, err
	}
	if !rights.Read {
		return att, nil, ErrNotFound
	}
	if !att.MessageID.Valid {
		if me, ok := memberOf(p); !ok || me.ID != att.UploadedBy {
			return att, nil, ErrNotFound
		}
	} else {
		msg, err := q.GetMessage(ctx, att.MessageID)
		if err != nil {
			return att, nil, err
		}
		if msg.DeletedAt.Valid {
			return att, nil, ErrNotFound
		}
	}
	r, err := a.blobs.Open(ctx, att.StorageKey)
	return att, r, err
}

// attach implements attacher: the files go with the message only if its author uploaded each of
// them into this conversation and has not sent them yet.
func (a *Attachments) attach(ctx context.Context, q *db.Queries, msg db.Message, by Author, ids []pgtype.UUID) error {
	if by.Kind != AuthorUser {
		return invalid("only people send files")
	}
	ids = unique(ids)
	n, err := q.AttachToMessage(ctx, db.AttachToMessageParams{MessageID: msg.ID, Ids: ids, ConversationID: msg.ConversationID, UploadedBy: by.ID})
	if err != nil {
		return err
	}
	if n != int64(len(ids)) {
		return invalid("a file goes with a message only from the person who uploaded it into this conversation, once")
	}
	return nil
}

// describeAttachments adds the files to a page of messages.
func describeAttachments(ctx context.Context, q *db.Queries, ids []pgtype.UUID, views []View, at map[[16]byte]int) error {
	rows, err := q.ListAttachmentsOfMessages(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		v := &views[at[r.MessageID.Bytes]]
		if !v.Message.DeletedAt.Valid {
			v.Attachments = append(v.Attachments, r)
		}
	}
	return nil
}

// cleanName keeps a file's base name, without paths or control characters.
func cleanName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > maxAttachmentName {
		name = name[:maxAttachmentName]
	}
	return name
}

// errNoBlob is a file whose bytes are gone from the store.
var errNoBlob = errors.New("no such file")
