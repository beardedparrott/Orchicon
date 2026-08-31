package db

import (
	"context"
	"fmt"
)

// cannedWorkflow defines a pre-canned template workflow to seed into the
// dev tenant on every boot. Idempotent — the workflow is created if it
// does not exist. If the workflow already exists AND the current version
// is still the original seed (same VersionID), the steps are updated so
// seed improvements (e.g. edge_handles) propagate. If the user has
// created new versions, their work is preserved untouched.
type cannedWorkflow struct {
	ID          string
	VersionID   string
	Name        string
	VersionNote string
	StepsJSON   string // JSON array of step objects
}

var cannedWorkflows = []cannedWorkflow{
	{
		ID:          "01KZ43VD5CGFXHK1SWPDJKEGPT",
		VersionID:   "wfv_coding_template_ai_approval_architect_v1",
		Name:        "SDLC (non-human approval)",
		VersionNote: "Full workflow with AI-powered approval + Principal Architect step: Architect -> Design Approver -> Senior SWE -> PR Reviewer + QA Engineer (parallel) -> Code Approver -> DevOps (PR/merge) with conflict loop",
		StepsJSON: `[
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-i64wso0x"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-330.0029080215844,"position_y":-58.74360838475371},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-rrcz490q"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-397.2581150575302,"position_y":267.0692727001121},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-rrcz490q"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-143.67363845559328,"position_y":267.86736384555934},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":-265.5238489863543,"position_y":432.1606427012862},
			{"id":"step-approval","name":"Code Approver","kind":"approval","ref":"w_se_code_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":-80.24304019372471,"position_y":554.3205632662957},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":125.91623741253625,"position_y":681.776961937013},
			{"id":"step-q4xlbg6v","name":"Principal Software Architect","kind":"task","ref":"w_se_principal_architect","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-370.21653077813266,"position_y":-346.6528223715784},
			{"id":"step-i64wso0x","name":"Design Approver","kind":"approval","ref":"w_se_design_approver","worker_version":0,"depends_on":["step-q4xlbg6v"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-q4xlbg6v\"}","position_x":-333.41658002439067,"position_y":-196.35081675232993},
			{"id":"step-rrcz490q","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-308.0277894023609,"position_y":89.43532397943022},
			{"id":"step-3rplua0d","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-devops-pr"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"conflict_value\":\"conflict\",\"exhausted_review\":\"\",\"loop_branch\":\"step-devops-pr\"}","position_x":257.5411926158563,"position_y":820.5813876295093}
		]`,
	},
	{
		ID:          "01KZ1W513F25ASPZM1XW4ZJ2MB",
		VersionID:   "wfv_coding_template_human_approval_architect_v1",
		Name:        "SDLC (human approval)",
		VersionNote: "Full workflow with human approval + Principal Architect step: Architect -> Design Approver -> Senior SWE -> PR Reviewer + QA Engineer (parallel) -> Code Approver -> DevOps (PR/merge) with conflict loop",
		StepsJSON: `[
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-i64wso0x"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-336.97734156059926,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-09zk5l01"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-403.7216769507495,"position_y":238.66836301259082},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-09zk5l01"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-142.53375885008086,"position_y":236.871602540136},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":-259.9568641373869,"position_y":412.1037198547323},
			{"id":"step-approval","name":"Code Approver","kind":"approval","ref":"w_se_code_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":-38.92911666021371,"position_y":553.5909366242397},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":125.91623741253625,"position_y":681.776961937013},
			{"id":"step-q4xlbg6v","name":"Principal Software Architect","kind":"task","ref":"w_se_principal_architect","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-370.21653077813266,"position_y":-346.6528223715784},
			{"id":"step-i64wso0x","name":"Design Approver","kind":"approval","ref":"w_se_design_approver","worker_version":0,"depends_on":["step-q4xlbg6v"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-q4xlbg6v\"}","position_x":-333.41658002439067,"position_y":-196.35081675232993},
			{"id":"step-09zk5l01","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-306.7399649338158,"position_y":81.10492023910098},
			{"id":"step-e75nato1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-devops-pr"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"conflict_value\":\"conflict\",\"exhausted_review\":\"\",\"loop_branch\":\"step-devops-pr\"}","position_x":257.5411926158563,"position_y":820.5813876295093}
		]`,
	},
}

// retiredCannedWorkflow is a template that was once seeded as canned but
// has been removed from cannedWorkflows.
type retiredCannedWorkflow struct {
	ID        string
	VersionID string // the seed version — the workflow is seed-managed only while this is still its current version
}

// retiredCannedWorkflows lists workflow templates that were seeded in earlier
// builds but removed from cannedWorkflows. SeedDevWorkflows hard-deletes any
// still-seed-managed instance on boot so retired templates don't linger in
// tenants. A workflow whose current version is no longer the original seed
// version has been forked by a user and is left untouched.
var retiredCannedWorkflows = []retiredCannedWorkflow{
	{
		ID:        "01KZB0CONFLICT000000000001",
		VersionID: "wfv_coding_template_ai_approval_architect_conflict_v1",
	},
	{
		ID:        "wf_devops_per_branch",
		VersionID: "wfv_devops_per_branch_v1",
	},
	{
		ID:        "wf_devops_per_branch_nogit",
		VersionID: "wfv_devops_per_branch_nogit_v1",
	},
	{
		ID:        "wf_coding_template",
		VersionID: "wfv_coding_template_v1",
	},
	{
		ID:        "wf_coding_template_human_approval",
		VersionID: "wfv_coding_template_human_approval_v1",
	},
	{
		ID:        "wf_coding_template_ai_approval",
		VersionID: "wfv_coding_template_ai_approval_v1",
	},
	{
		ID:        "01KZA9H7935CRTAHVE3EHVC1NZ",
		VersionID: "wfv_ui_nonhuman_architect_v1",
	},
	{
		ID:        "01KZA9M2PMVNZG3QPHQ7AS3GA1",
		VersionID: "wfv_ui_human_architect_v1",
	},
}

// SeedDevWorkflows creates or updates all canned workflow templates in the
// dev tenant and retires templates that have been removed from
// cannedWorkflows. Idempotent — safe to call on every boot.
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

	// Retired canned templates: workflows that used to be seeded but have
	// been removed. Delete them ONLY when they are still seed-managed
	// (their current version is the original seed version) — a user who
	// forked one keeps their workflow.
	for _, r := range retiredCannedWorkflows {
		ttx, err := p.BeginTenantTx(ctx, "tnt_dev")
		if err != nil {
			return fmt.Errorf("retire workflow %s: begin tx: %w", r.ID, err)
		}
		var exists bool
		if err := ttx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev')`, r.ID,
		).Scan(&exists); err != nil {
			ttx.Rollback(ctx)
			return fmt.Errorf("retire workflow %s: check exists: %w", r.ID, err)
		}
		if exists {
			seedManaged, err := workflowIsSeedManaged(ctx, ttx, r.ID, r.VersionID)
			if err != nil {
				ttx.Rollback(ctx)
				return fmt.Errorf("retire workflow %s: inspect: %w", r.ID, err)
			}
			if seedManaged {
				if err := deleteWorkflowByID(ctx, ttx, r.ID); err != nil {
					ttx.Rollback(ctx)
					return fmt.Errorf("retire workflow %s: %w", r.ID, err)
				}
			} else {
				// User-forked workflow owns the id — leave it.
				ttx.Rollback(ctx)
				continue
			}
		}
		if err := ttx.Commit(ctx); err != nil {
			return fmt.Errorf("retire workflow %s: commit: %w", r.ID, err)
		}
	}
	return nil
}

// workflowIsSeedManaged reports whether the workflow still carries its
// original seed version (its current version id matches the seed's
// VersionID). A workflow the user has forked by publishing new versions is
// no longer seed-managed.
func workflowIsSeedManaged(ctx context.Context, ttx *TenantTx, workflowID, seedVersionID string) (bool, error) {
	var currentVer int
	if err := ttx.QueryRow(ctx,
		`SELECT current_version FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev'`, workflowID,
	).Scan(&currentVer); err != nil {
		return false, err
	}
	var verID string
	if err := ttx.QueryRow(ctx,
		`SELECT id FROM workflow_versions WHERE workflow_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
		workflowID, currentVer,
	).Scan(&verID); err != nil {
		return false, err
	}
	return verID == seedVersionID, nil
}

// deleteWorkflowByID hard-deletes a workflow and its owned rows (versions,
// runs, step runs, edit locks) inside the seeder's tenant transaction.
// Mirrors db.DeleteWorkflow but scoped to the seeded tenant so the seeder
// can purge retired templates.
func deleteWorkflowByID(ctx context.Context, ttx *TenantTx, workflowID string) error {
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workflow_step_runs WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE tenant_id = 'tnt_dev' AND workflow_id = $1)`, workflowID); err != nil {
		return fmt.Errorf("delete workflow step runs: %w", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workflow_runs WHERE tenant_id = 'tnt_dev' AND workflow_id = $1`, workflowID); err != nil {
		return fmt.Errorf("delete workflow runs: %w", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workflow_versions WHERE tenant_id = 'tnt_dev' AND workflow_id = $1`, workflowID); err != nil {
		return fmt.Errorf("delete workflow versions: %w", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM edit_locks WHERE resource_id = $1 AND resource_type = 'workflow' AND tenant_id = 'tnt_dev'`, workflowID); err != nil {
		return fmt.Errorf("delete workflow edit locks: %w", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev'`, workflowID); err != nil {
		return fmt.Errorf("delete workflow: %w", err)
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
		// Workflow exists. Only update the original seed version
		// (matching VersionID) — newer versions are user-created
		// and must be preserved.
		var currentVer int
		_ = ttx.QueryRow(ctx,
			`SELECT current_version FROM workflows WHERE id = $1 AND tenant_id = 'tnt_dev'`, w.ID,
		).Scan(&currentVer)
		var verID string
		err := ttx.QueryRow(ctx,
			`SELECT id FROM workflow_versions WHERE workflow_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
			w.ID, currentVer,
		).Scan(&verID)
		if err == nil && verID == w.VersionID {
			// Current version is still the original seed — update steps and name.
			_, err = ttx.Exec(ctx,
				`UPDATE workflow_versions SET steps = $1::jsonb, status = 'published'
				 WHERE id = $2 AND tenant_id = 'tnt_dev'`,
				w.StepsJSON, verID,
			)
			if err != nil {
				return fmt.Errorf("update seed version steps: %w", err)
			}
			// Propagate name changes so seed renames reach seed-managed tenants.
			_, err = ttx.Exec(ctx,
				`UPDATE workflows SET name = $1, updated_at = now()
				 WHERE id = $2 AND tenant_id = 'tnt_dev' AND name IS DISTINCT FROM $1`,
				w.Name, w.ID,
			)
			if err != nil {
				return fmt.Errorf("update seed workflow name: %w", err)
			}
		}
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
