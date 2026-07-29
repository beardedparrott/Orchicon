package db

import (
	"context"
	"fmt"
)

// cannedWorkflow defines a pre-canned template workflow to seed into the
// dev tenant on every boot. Idempotent — the workflow is created if it
// does not exist, and updated if it does.
type cannedWorkflow struct {
	ID          string
	VersionID   string
	Name        string
	VersionNote string
	StepsJSON   string // JSON array of step objects
}

var cannedWorkflows = []cannedWorkflow{
	{
		ID:        "wf_coding_template",
		VersionID: "wfv_coding_template_v1",
		Name:      "Coding Template",
		VersionNote: "Simple coding workflow: Senior SWE → PR Reviewer → Loop Decision → QA Engineer → Loop Decision",
		StepsJSON: `[
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-55,"position_y":80},
			{"id":"step-loop-1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-qa\"}","position_x":218,"position_y":92},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-loop-1"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":225,"position_y":231},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\"}","position_x":551,"position_y":88}
		]`,
	},
	{
		ID:        "wf_coding_template_human_approval",
		VersionID: "wfv_coding_template_human_approval_v1",
		Name:      "Coding Template with Approvers (Human)",
		VersionNote: "Full workflow: DevOps Engineer (repo) → Senior SWE → PR Reviewer → Loop → QA Engineer → Loop → Human Approval → DevOps Engineer (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-344,"position_y":-61},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-55,"position_y":80},
			{"id":"step-loop-1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-qa\"}","position_x":236,"position_y":99},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-loop-1"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":225,"position_y":231},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":502,"position_y":304},
			{"id":"step-approval","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":526,"position_y":442},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":506,"position_y":564}
		]`,
	},
	{
		ID:        "wf_coding_template_ai_approval",
		VersionID: "wfv_coding_template_ai_approval_v1",
		Name:      "Coding Template with Approvers (Non-human)",
		VersionNote: "Full workflow with AI-powered approval (AI Approver worker): DevOps Engineer (repo) → Senior SWE → PR Reviewer → QA Engineer → AI Approver → DevOps Engineer (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-344,"position_y":-61},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-55,"position_y":80},
			{"id":"step-loop-1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer"],"gate_policy_ref":"","config":"{\"max_iterations\":4,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-qa\"}","position_x":237,"position_y":99},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-loop-1"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":225,"position_y":231},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":4,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":502,"position_y":304},
			{"id":"step-approval","name":"AI Approver","kind":"approval","ref":"w_se_ai_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":4,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":526,"position_y":442},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":506,"position_y":564}
		]`,
	},
}

// SeedDevWorkflows creates or updates all canned workflow templates in the
// dev tenant. Idempotent — safe to call on every boot.
func SeedDevWorkflows(ctx context.Context, p *Pool) error {
	for _, w := range cannedWorkflows {
		ttx, err := p.BeginTenantTx(ctx, "tnt_dev")
		if err != nil {
			return fmt.Errorf("seed workflow %s: begin tx: %w", w.ID, err)
		}

		if err := seedWorkflow(ctx, ttx, w); err != nil {
			ttx.Rollback(ctx)
			return fmt.Errorf("seed workflow %s: %w", w.ID, err)
		}

		if err := ttx.Commit(ctx); err != nil {
			return fmt.Errorf("seed workflow %s: commit: %w", w.ID, err)
		}
	}
	return nil
}

func seedWorkflow(ctx context.Context, ttx *TenantTx, w cannedWorkflow) error {
	// Check if workflow already exists.
	var existingID string
	err := ttx.QueryRow(ctx,
		`SELECT id FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev'`, w.ID,
	).Scan(&existingID)

	if err == nil {
		// Workflow exists — ensure published status and update the
		// current version's steps and name.
		_, err := ttx.Exec(ctx,
			`UPDATE workflows SET status = 'published', name = $1, current_version = GREATEST(current_version, 1)
			 WHERE id = $2 AND tenant_id = 'tnt_dev'`,
			w.Name, w.ID,
		)
		if err != nil {
			return fmt.Errorf("update workflow: %w", err)
		}

		// Update the current version's steps.
		_, _ = ttx.Exec(ctx,
			`UPDATE workflow_versions SET steps = $1::jsonb, status = 'published'
			 WHERE workflow_id = $2 AND tenant_id = 'tnt_dev' AND version = (
			   SELECT current_version FROM workflows WHERE id = $2 AND tenant_id = 'tnt_dev'
			 )`,
			w.StepsJSON, w.ID,
		)
		return nil
	}

	// Create workflow.
	_, err = ttx.Exec(ctx,
		`INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, type)
		 VALUES ($1, 'tnt_dev', '', $2, 1, 'published', 'template')
		 ON CONFLICT (id) DO NOTHING`,
		w.ID, w.Name,
	)
	if err != nil {
		return fmt.Errorf("insert workflow: %w", err)
	}

	// Create workflow version.
	_, err = ttx.Exec(ctx,
		`INSERT INTO workflow_versions (id, tenant_id, workflow_id, version, version_note, status, steps, inputs, outputs)
		 VALUES ($1, 'tnt_dev', $2, 1, $3, 'published', $4::jsonb, '{}'::jsonb, '{}'::jsonb)
		 ON CONFLICT DO NOTHING`,
		w.VersionID, w.ID, w.VersionNote, w.StepsJSON,
	)
	if err != nil {
		return fmt.Errorf("insert workflow version: %w", err)
	}

	return nil
}
