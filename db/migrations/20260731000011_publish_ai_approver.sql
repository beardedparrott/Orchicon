-- Publish the AI Approver worker so it's dispatchable by the TaskReconciler.

UPDATE workers SET status = 'published', current_version = 1 WHERE id = 'w_se_ai_approver' AND status = 'draft';

UPDATE worker_versions SET status = 'published', published_at = now()
WHERE worker_id = 'w_se_ai_approver' AND status = 'draft';
