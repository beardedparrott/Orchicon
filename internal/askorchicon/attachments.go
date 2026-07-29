package askorchicon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// UploadAttachment handles file attachment uploads for the Ask Orchicon chat.
// Files are stored via the BlobStore and referenced by URL in messages.
func (s *Service) UploadAttachment(ctx context.Context, req *connect.Request[apiv1.UploadAttachmentRequest]) (*connect.Response[apiv1.UploadAttachmentResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name must not be empty"))
	}
	if len(req.Msg.Data) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("data must not be empty"))
	}
	if len(req.Msg.Data) > 50*1024*1024 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("file too large (max 50MB)"))
	}

	// Verify conversation exists.
	if req.Msg.ConversationId != "" {
		ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		_, err = db.GetConversation(ctx, ttx.Tx, tenantID, req.Msg.ConversationId)
		ttx.Rollback(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("conversation not found"))
		}
	}

	if s.blobStore == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("blob store is not available"))
	}

	// Sanitize the filename for the blob ref.
	name := strings.TrimSpace(req.Msg.Name)
	if len(name) > 255 {
		name = name[:255]
	}

	blobRef := fmt.Sprintf("ask_orchicon/%s/%s/%s", tenantID, req.Msg.ConversationId, name)
	mimeType := req.Msg.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	blob, err := s.blobStore.Put(ctx, blobRef, mimeType, bytes.NewReader(req.Msg.Data))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store attachment: %w", err))
	}

	return connect.NewResponse(&apiv1.UploadAttachmentResponse{
		AttachmentId: blob.Ref,
		Url:          blob.Ref,
	}), nil
}
