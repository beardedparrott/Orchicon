package workitem

import (
    "context"
    "fmt"
    "os"
    "path/filepath"

    "github.com/beardedparrott/orchicon/internal/db"
    "github.com/jackc/pgx/v5"
)

// MaterializeWorkItemAttachments ensures the attachment manifest directory exists; blob fetch is via blobstore (wired when available).
func MaterializeWorkItemAttachments(ctx context.Context, tx pgx.Tx, tenantID, workItemID, workDir string) error {
    attachments, err := db.ListWorkItemAttachments(ctx, tx, tenantID, workItemID)
    if err != nil {
        return err
    }
    if len(attachments) == 0 {
        return nil
    }
    dir := filepath.Join(workDir, ".orchicon", "work-item-attachments", workItemID)
    if err := os.MkdirAll(dir, 0755); err != nil {
        return fmt.Errorf("mkdir attachments: %w", err)
    }
    return nil
}
