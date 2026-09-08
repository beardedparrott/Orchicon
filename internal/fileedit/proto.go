package fileedit

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RowToProto maps a durable ledger row onto its proto message. It is THE
// shared mapper: the server's event publisher (execution.file_edit bus
// events), the GetSessionFileEdits fetch, and the StreamFileEdits live path
// all render rows through it, so a GUI and a TUI consumer of either surface
// see byte-identical fields. Only proto / db / timestamppb imports — no
// pipeline dependency, no cycle.
func RowToProto(r *db.FileEditLedgerRow) *apiv1.FileEdit {
	p := &apiv1.FileEdit{
		Id:           r.ID,
		OwnerKind:    r.OwnerKind,
		OwnerId:      r.OwnerID,
		Seq:          r.Seq,
		Path:         r.Path,
		Kind:         r.Kind,
		UnifiedDiff:  r.UnifiedDiff,
		BeforeSize:   r.BeforeSize,
		AfterSize:    r.AfterSize,
		Tool:         r.Tool,
		IsBinary:     r.IsBinary,
		Truncated:    r.Truncated,
		GitConfirmed: r.GitConfirmed,
		CreatedAt:    timestamppb.New(r.CreatedAt),
	}
	if r.CreatedAt.IsZero() {
		p.CreatedAt = nil
	}
	return p
}

// ProtoToRow is the inverse of RowToProto: a published envelope decoded on
// the stream side back into the row shape (the subscriber's live path).
func ProtoToRow(p *apiv1.FileEdit) *db.FileEditLedgerRow {
	r := &db.FileEditLedgerRow{
		ID:           p.GetId(),
		OwnerKind:    p.GetOwnerKind(),
		OwnerID:      p.GetOwnerId(),
		Seq:          p.GetSeq(),
		Path:         p.GetPath(),
		Kind:         p.GetKind(),
		UnifiedDiff:  p.GetUnifiedDiff(),
		BeforeSize:   p.GetBeforeSize(),
		AfterSize:    p.GetAfterSize(),
		Tool:         p.GetTool(),
		IsBinary:     p.GetIsBinary(),
		Truncated:    p.GetTruncated(),
		GitConfirmed: p.GetGitConfirmed(),
	}
	if p.GetCreatedAt() != nil {
		r.CreatedAt = p.GetCreatedAt().AsTime()
	}
	return r
}
