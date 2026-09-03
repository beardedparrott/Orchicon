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
		Name:        "SDLC (non-human)",
		VersionNote: "Sequential SDLC (2026-09-02 rewire): Architect (code-grounded plan, 20-min box) -> Senior SWE (execute the plan, 45-min box) -> PR Reviewer (fix-don't-bounce, 30-min box) -> gate -> QA Engineer (UI + regression, fix-don't-bounce, 30-min box) -> gate -> DevOps (PR/merge) with conflict loop. PR Reviewer and QA run SEQUENTIALLY, not parallel: both steps mutate code under the fix-forward workhorse contract, so parallel execution would race on the same branch. Gates loop back to the SSE (failures mean genuinely-stuck, an SSE-class problem).",
		StepsJSON: `[
			{"id":"step-q4xlbg6v","ref":"w_se_principal_architect","kind":"task","name":"Principal Software Architect","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","depends_on":[],"position_x":-330.3586366719073,"position_y":-215.47301513050206,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-sse","ref":"w_se_senior_software_engineer","kind":"task","name":"Senior Software Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","depends_on":["step-q4xlbg6v"],"position_x":-330.0029080215844,"position_y":-58.74360838475371,"edge_handles":{"e-step-9gfybwnv-step-qa":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-qa-step-b2bmqv2p":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-9gfybwnv-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-b2bmqv2p-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-q4xlbg6v-step-sse":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-sse-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-3rplua0d-step-l32ezp4b":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-3rplua0d-step-devops-pr":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-b2bmqv2p-step-devops-pr":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-devops-pr-step-3rplua0d":{"sourceHandle":"source-bottom","targetHandle":"target-left"},"e-step-pr-reviewer-step-9gfybwnv":{"sourceHandle":"source-right","targetHandle":"target-left"}},"worker_version":0,"gate_policy_ref":""},
			{"id":"step-pr-reviewer","ref":"w_se_pr_reviewer","kind":"task","name":"PR Reviewer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","depends_on":["step-sse"],"position_x":-318.8609470568348,"position_y":117.03331325050539,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-9gfybwnv","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":6,\"success_branch\":\"step-qa\",\"loop_branch\":\"step-sse\"}","depends_on":["step-pr-reviewer"],"position_x":-17.29682307378056,"position_y":128.16612305782547,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-qa","ref":"w_se_qa_engineer","kind":"task","name":"QA Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","depends_on":["step-9gfybwnv"],"position_x":-293.46729547311793,"position_y":375.48472610277827,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-b2bmqv2p","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","depends_on":["step-qa"],"position_x":5.751738777258652,"position_y":380.7783609452147,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-devops-pr","ref":"w_se_devops_engineer","kind":"task","name":"DevOps Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","depends_on":["step-b2bmqv2p"],"position_x":-274.54837041266813,"position_y":575.0511487425962,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-3rplua0d","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":6,\"conflict_value\":\"conflict\",\"exhausted_review\":\"\",\"loop_branch\":\"step-devops-pr\",\"success_branch\":\"step-l32ezp4b\"}","depends_on":["step-devops-pr"],"position_x":-56.84691389070679,"position_y":714.4626144687958,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-l32ezp4b","ref":"","kind":"end","name":"End","config":"{}","depends_on":["step-3rplua0d"],"position_x":63.82420164593009,"position_y":857.9210020894698,"worker_version":0,"gate_policy_ref":""}
		]`,
	},
	{
		ID:          "01KZ1W513F25ASPZM1XW4ZJ2MB",
		VersionID:   "wfv_coding_template_human_approval_architect_v1",
		Name:        "SDLC (human approval)",
		VersionNote: "Sequential SDLC with human approvals (2026-09-02 rewire): Architect -> human Design Approval -> Senior SWE -> PR Reviewer (fix-don't-bounce) -> gate -> QA Engineer (UI + regression, fix-don't-bounce) -> gate -> human Code Approval -> DevOps (PR/merge) with conflict loop. PR Reviewer and QA run SEQUENTIALLY — both steps mutate code under the fix-forward workhorse contract, so parallel execution would race on the same branch. Gates loop back to the SSE.",
		StepsJSON: `[
			{"id":"step-m10gxzqj","ref":"w_se_principal_architect","kind":"task","name":"Principal Software Architect","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":[],"position_x":-74.3622421716779,"position_y":-328.3734649100988,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-ji8iw0jk","ref":"","kind":"approval","name":"Approval","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-m10gxzqj\"}","depends_on":["step-m10gxzqj"],"position_x":-51.336234534795096,"position_y":-193.76156076533175,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-sse","ref":"w_se_senior_software_engineer","kind":"task","name":"Senior Software Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-ji8iw0jk"],"position_x":-58,"position_y":-57,"edge_handles":{"e-step-sse-step-qa":{"sourceHandle":"source-success"},"e-step-qa-step-eecb4e4d":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-rtnfg69f-step-qa":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-eecb4e4d-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-ji8iw0jk-step-sse":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-rtnfg69f-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-sse-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-eecb4e4d-step-approval":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-ji8iw0jk-step-m10gxzqj":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-m10gxzqj-step-ji8iw0jk":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-devops-pr":{"sourceHandle":"source-success"},"e-step-devops-pr-step-e75nato1":{"sourceHandle":"source-bottom","targetHandle":"target-left"},"e-step-e75nato1-step-devops-pr":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-pr-reviewer-step-rtnfg69f":{"sourceHandle":"source-right","targetHandle":"target-left"}},"worker_version":0,"gate_policy_ref":""},
			{"id":"step-pr-reviewer","ref":"w_se_pr_reviewer","kind":"task","name":"PR Reviewer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-sse"],"position_x":-57.80804310981841,"position_y":107.60284367609393,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-rtnfg69f","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":3,\"success_branch\":\"step-qa\",\"loop_branch\":\"step-sse\"}","depends_on":["step-pr-reviewer"],"position_x":276.6179428448227,"position_y":126.29553813340257,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-qa","ref":"w_se_qa_engineer","kind":"task","name":"QA Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-sse","step-rtnfg69f"],"position_x":-52.614563990580336,"position_y":267.3998840055186,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-eecb4e4d","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":3,\"success_branch\":\"step-approval\",\"loop_branch\":\"step-sse\"}","depends_on":["step-qa"],"position_x":282.2063804478799,"position_y":280.53641597778125,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-approval","ref":"","kind":"approval","name":"Approval","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","depends_on":["step-eecb4e4d"],"position_x":187.50682890151325,"position_y":580.2180448652155,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-devops-pr","ref":"w_se_devops_engineer","kind":"task","name":"DevOps Engineer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-approval"],"position_x":393.168942967171,"position_y":741.0025758128266,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-e75nato1","ref":"","kind":"loop_decision","name":"Loop Decision","config":"{\"max_iterations\":6,\"conflict_value\":\"conflict\",\"exhausted_review\":\"\",\"loop_branch\":\"step-devops-pr\",\"success_branch\":\"step-qonmbwyu\"}","depends_on":["step-devops-pr"],"position_x":679.2434240082503,"position_y":817.0275930894546,"worker_version":0,"gate_policy_ref":""},
			{"id":"step-qonmbwyu","ref":"","kind":"end","name":"End","config":"{}","depends_on":["step-e75nato1"],"position_x":731.6759619225201,"position_y":989.0576715040506,"worker_version":0,"gate_policy_ref":""}
		]`,
	},
	{
		ID:          "01M17EZC170ZR7SZAJET4Z5RY1",
		VersionID:   "wfv_automation_research_v1",
		Name:        "Automation Research",
		VersionNote: "Planner -> Analyst -> Synthesizer -> End: market research against a per-run capability landscape (harnesses, runtimes, orchestration platforms, automation platforms, frameworks), evidence capture, and idea-state work item spawning via the automation-research role. Product targets live in the bound work item's brief, so the workflow and its workers stay product-agnostic.",
		StepsJSON: `[
  {"id":"step-5mxcx4yk","ref":"01M13DYHKHEF71MVGY07GMGMJ6","kind":"task","name":"Automation — Research Planner","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":[],"position_x":73.66668701171875,"position_y":28.75,"edge_handles":{"e-step-5mxcx4yk-step-8wdctc1f":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-8wdctc1f-step-mt87ezlw":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-mt87ezlw-step-1tzgaakr":{"sourceHandle":"source-right","targetHandle":"target-left"}},"worker_version":0,"gate_policy_ref":""},
  {"id":"step-8wdctc1f","ref":"01M13DYJWHCYHWQ1X85J1BWWZ1","kind":"task","name":"Automation — Research Analyst","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-5mxcx4yk"],"position_x":74.70288563736119,"position_y":136.46383242810788,"worker_version":0,"gate_policy_ref":""},
  {"id":"step-mt87ezlw","ref":"01M13DYM3A7CTY8ECP4R7M33SR","kind":"task","name":"Automation — Research Synthesizer","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","depends_on":["step-8wdctc1f"],"position_x":74.91912768189644,"position_y":249.66389623805503,"worker_version":0,"gate_policy_ref":""},
  {"id":"step-1tzgaakr","ref":"","kind":"end","name":"End","config":"{}","depends_on":["step-mt87ezlw"],"position_x":366.4761407515862,"position_y":259.8566843019803,"worker_version":0,"gate_policy_ref":""}
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
