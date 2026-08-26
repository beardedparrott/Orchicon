package db

import (
    "context"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5"
)

type WorkItemAttachmentRow struct {
    ID         string
    TenantID   string
    WorkItemID string
    ProjectID  string
    Name       string
    MimeType   string
    SizeBytes  int64
    BlobRef    string
    CreatedAt  time.Time
    CreatedBy  *string
}

func CreateWorkItemAttachment(ctx context.Context, tx pgx.Tx, r WorkItemAttachmentRow) (WorkItemAttachmentRow, error) {
    const q = `INSERT INTO work_item_attachments (id, tenant_id, work_item_id, project_id, name, mime_type, size_bytes, blob_ref, created_by)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id, tenant_id, work_item_id, project_id, name, mime_type, size_bytes, blob_ref, created_at, created_by`
    row := r
    err := tx.QueryRow(ctx, q, r.ID, r.TenantID, r.WorkItemID, r.ProjectID, r.Name, r.MimeType, r.SizeBytes, r.BlobRef, r.CreatedBy).Scan(
        &row.ID, &row.TenantID, &row.WorkItemID, &row.ProjectID, &row.Name, &row.MimeType, &row.SizeBytes, &row.BlobRef, &row.CreatedAt, &row.CreatedBy)
    if err != nil {
        return WorkItemAttachmentRow{}, fmt.Errorf("db: create work item attachment: %w", err)
    }
    return row, nil
}

func ListWorkItemAttachments(ctx context.Context, tx pgx.Tx, tenantID, workItemID string) ([]WorkItemAttachmentRow, error) {
    const q = `SELECT id, tenant_id, work_item_id, project_id, name, mime_type, size_bytes, blob_ref, created_at, created_by FROM work_item_attachments WHERE tenant_id=$1 AND work_item_id=$2 ORDER BY created_at`
    rows, err := tx.Query(ctx, q, tenantID, workItemID)
    if err != nil {
        return nil, fmt.Errorf("db: list work item attachments: %w", err)
    }
    defer rows.Close()
    var out []WorkItemAttachmentRow
    for rows.Next() {
        var r WorkItemAttachmentRow
        if err := rows.Scan(&r.ID, &r.TenantID, &r.WorkItemID, &r.ProjectID, &r.Name, &r.MimeType, &r.SizeBytes, &r.BlobRef, &r.CreatedAt, &r.CreatedBy); err != nil {
            return nil, err
        }
        out = append(out, r)
    }
    return out, rows.Err()
}

func DeleteWorkItemAttachment(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
    const q = `DELETE FROM work_item_attachments WHERE tenant_id=$1 AND id=$2`
    tag, err := tx.Exec(ctx, q, tenantID, id)
    if err != nil {
        return fmt.Errorf("db: delete work item attachment: %w", err)
    }
    if tag.RowsAffected()==0 {
        return ErrNotFound
    }
    return nil
}

func GetWorkItemAttachment(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkItemAttachmentRow, error) {
    const q = `SELECT id, tenant_id, work_item_id, project_id, name, mime_type, size_bytes, blob_ref, created_at, created_by FROM work_item_attachments WHERE tenant_id=$1 AND id=$2`
    var r WorkItemAttachmentRow
    err := tx.QueryRow(ctx, q, tenantID, id).Scan(&r.ID, &r.TenantID, &r.WorkItemID, &r.ProjectID, &r.Name, &r.MimeType, &r.SizeBytes, &r.BlobRef, &r.CreatedAt, &r.CreatedBy)
    if err != nil {
        return WorkItemAttachmentRow{}, fmt.Errorf("db: get work item attachment: %w", err)
    }
    return r, nil
}
