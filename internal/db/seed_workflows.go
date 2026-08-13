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
		ID:          "wf_coding_template",
		VersionID:   "wfv_coding_template_v1",
		Name:        "Coding Template",
		VersionNote: "Simple coding workflow: Senior SWE → PR Reviewer + QA Engineer (parallel) → Loop Decision",
		StepsJSON: `[
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57,"edge_handles":{"e-step-sse-step-qa":{"sourceHandle":"source-success"},"e-step-sse-step-pr-reviewer":{},"e-step-loop-1-step-sse":{"sourceHandle":"source-loop"}}},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-55,"position_y":80},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":225,"position_y":231},
			{"id":"step-loop-1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\"}","position_x":218,"position_y":92}
		]`,
	},
	{
		ID:          "wf_coding_template_human_approval",
		VersionID:   "wfv_coding_template_human_approval_v1",
		Name:        "Coding Template with Approvers (Human)",
		VersionNote: "Full workflow: DevOps Engineer (repo) → Senior SWE → PR Reviewer + QA Engineer (parallel) → Human Approval → DevOps Engineer (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-344,"position_y":-61,"edge_handles":{"e-step-repo-step-sse":{},"e-step-qa-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-891p4xm8-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-sse-step-891p4xm8":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-approval":{},"e-step-approval-step-devops-pr":{},"e-step-pr-reviewer-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-891p4xm8-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-891p4xm8"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-165.30046447011853,"position_y":220.35712541744914},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-891p4xm8"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":80.02366886032746,"position_y":223.48563612683427},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-qa","step-pr-reviewer"],"gate_policy_ref":"","config":"{\"max_iterations\":3,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":0.0728688745555246,"position_y":435.60350052472495},
			{"id":"step-approval","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":181.4477497383573,"position_y":553.4210671596793},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":354.36662348334846,"position_y":703.3098684301641},
			{"id":"step-891p4xm8","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-37.90303848350072,"position_y":94.20367370987418}
		]`,
	},
	{
		ID:          "wf_coding_template_ai_approval",
		VersionID:   "wfv_coding_template_ai_approval_v1",
		Name:        "Coding Template with Approvers (Non-human)",
		VersionNote: "Full workflow with AI-powered approval (Code Approver worker): DevOps Engineer (repo) → Senior SWE → PR Reviewer + QA Engineer (parallel) → Code Approver → DevOps Engineer (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-344,"position_y":-61,"edge_handles":{"e-step-repo-step-sse":{},"e-step-qa-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-uquk9jv9-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-sse-step-uquk9jv9":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-approval":{},"e-step-approval-step-devops-pr":{},"e-step-pr-reviewer-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-uquk9jv9-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-uquk9jv9"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-152.66492921919235,"position_y":287.34584391606165},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-uquk9jv9"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":96.77026044664842,"position_y":288.7448938336377},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":4,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":6.558620689655072,"position_y":469.56206896551726},
			{"id":"step-approval","name":"Code Approver","kind":"approval","ref":"w_se_code_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":4,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":184.91724137931033,"position_y":590.1344827586207},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":306.8275862068965,"position_y":784.2920489081017},
			{"id":"step-uquk9jv9","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-39.47170050095599,"position_y":112.21863900619564}
		]`,
	},
	{
		ID:          "01KZ43VD5CGFXHK1SWPDJKEGPT",
		VersionID:   "wfv_coding_template_ai_approval_architect_v1",
		Name:        "Coding Template with Approvers (Non-human) - Architect",
		VersionNote: "Full workflow with AI-powered approval + Principal Architect step: DevOps (repo) -> Architect -> Design Approver -> Senior SWE -> PR Reviewer + QA Engineer (parallel) -> Code Approver -> DevOps (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-634.6434161696428,"position_y":-350.29286315717263,"edge_handles":{"e-step-qa-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-rrcz490q-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-i64wso0x-step-sse":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-sse-step-rrcz490q":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-repo-step-q4xlbg6v":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-loop-2-step-approval":{"sourceHandle":"source-success"},"e-step-i64wso0x-step-q4xlbg6v":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-q4xlbg6v-step-i64wso0x":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-devops-pr":{"sourceHandle":"source-success"},"e-step-pr-reviewer-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-rrcz490q-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-i64wso0x"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-330.0029080215844,"position_y":-58.74360838475371},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-rrcz490q"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-397.2581150575302,"position_y":267.0692727001121},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-rrcz490q"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-143.67363845559328,"position_y":267.86736384555934},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":-265.5238489863543,"position_y":432.1606427012862},
			{"id":"step-approval","name":"Code Approver","kind":"approval","ref":"w_se_code_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":-80.24304019372471,"position_y":554.3205632662957},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":153.19823076612238,"position_y":676.3205632662958},
			{"id":"step-q4xlbg6v","name":"Principal Software Architect","kind":"task","ref":"w_se_principal_architect","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-370.21653077813266,"position_y":-346.6528223715784},
			{"id":"step-i64wso0x","name":"Design Approver","kind":"approval","ref":"w_se_design_approver","worker_version":0,"depends_on":["step-q4xlbg6v"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-q4xlbg6v\"}","position_x":-333.41658002439067,"position_y":-196.35081675232993},
			{"id":"step-rrcz490q","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-308.0277894023609,"position_y":89.43532397943022}
		]`,
	},
	{
		ID:          "01KZ1W513F25ASPZM1XW4ZJ2MB",
		VersionID:   "wfv_coding_template_human_approval_architect_v1",
		Name:        "Coding Template with Approvers (Human) - Architect",
		VersionNote: "Full workflow with human approval + Principal Architect step: DevOps (repo) -> Architect -> Approval -> Senior SWE -> PR Reviewer + QA Engineer (parallel) -> Human Approval -> DevOps (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-372.70086163832957,"position_y":-329.49193145534235,"edge_handles":{"e-step-sse-step-qa":{"sourceHandle":"source-success"},"e-step-qa-step-loop-1":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-1-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-sx85j8pv-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-ji8iw0jk-step-sse":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-sse-step-sx85j8pv":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-repo-step-m10gxzqj":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-loop-1-step-approval":{"sourceHandle":"source-success"},"e-step-ji8iw0jk-step-m10gxzqj":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-m10gxzqj-step-ji8iw0jk":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-devops-pr":{"sourceHandle":"source-success"},"e-step-pr-reviewer-step-loop-1":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-sx85j8pv-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer","kind":"task","ref":"w_se_senior_software_engineer","worker_version":0,"depends_on":["step-ji8iw0jk"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-58,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-sx85j8pv"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-155.04685740301363,"position_y":268.5498466441411},
			{"id":"step-qa","name":"QA Engineer","kind":"task","ref":"w_se_qa_engineer","worker_version":0,"depends_on":["step-sse","step-sx85j8pv"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":82.6256260034038,"position_y":268.5175715261301},
			{"id":"step-loop-1","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":4,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":2.23666972180456,"position_y":452.1455503432961},
			{"id":"step-approval","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-loop-1"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":187.50682890151325,"position_y":580.2180448652155},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":393.168942967171,"position_y":741.0025758128266},
			{"id":"step-m10gxzqj","name":"Principal Software Architect","kind":"task","ref":"w_se_principal_architect","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-74.3622421716779,"position_y":-328.3734649100988},
			{"id":"step-ji8iw0jk","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-m10gxzqj"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":3,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-m10gxzqj\"}","position_x":-51.336234534795096,"position_y":-193.76156076533175},
			{"id":"step-sx85j8pv","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-33.4222772766484,"position_y":97.06251610573398}
		]`,
	},
	{
		ID:          "01KZA9H7935CRTAHVE3EHVC1NZ",
		VersionID:   "wfv_ui_nonhuman_architect_v1",
		Name:        "UI Development (Non-human) - Architect",
		VersionNote: "UI workflow with AI-powered approval + vision workers: DevOps (repo) -> Architect (Vision) -> Design Approver -> Senior SWE (Vision) -> PR Reviewer + QA (Vision, parallel) -> Code Approver -> DevOps (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-634.6434161696428,"position_y":-350.29286315717263,"edge_handles":{"e-step-qa-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-mlz9brwx-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-i64wso0x-step-sse":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-sse-step-mlz9brwx":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-repo-step-q4xlbg6v":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-loop-2-step-approval":{"sourceHandle":"source-success"},"e-step-i64wso0x-step-q4xlbg6v":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-q4xlbg6v-step-i64wso0x":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-devops-pr":{"sourceHandle":"source-success"},"e-step-pr-reviewer-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-mlz9brwx-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer - Vision","kind":"task","ref":"w_se_sse_vision","worker_version":0,"depends_on":["step-i64wso0x"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-347.4389918691219,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-mlz9brwx"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-415.3487498453055,"position_y":235.79434399252494},
			{"id":"step-qa","name":"QA Engineer - Vision","kind":"task","ref":"w_se_qa_vision","worker_version":0,"depends_on":["step-mlz9brwx"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-162.8337109658321,"position_y":234.3968142735796},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":-264.52468336464375,"position_y":420.5484405440262},
			{"id":"step-approval","name":"Code Approver","kind":"approval","ref":"w_se_code_approver","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":-48.56288132374357,"position_y":545.6805199381191},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":205.03849073518177,"position_y":689.3276236114209},
			{"id":"step-q4xlbg6v","name":"Principal Software Architect - Vision","kind":"task","ref":"w_se_architect_vision","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":6}}","position_x":-370.21653077813266,"position_y":-346.6528223715784},
			{"id":"step-i64wso0x","name":"Design Approver","kind":"approval","ref":"w_se_design_approver","worker_version":0,"depends_on":["step-q4xlbg6v"],"gate_policy_ref":"","config":"{\"reviewer\":\"worker\",\"max_iterations\":6,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-q4xlbg6v\"}","position_x":-333.41658002439067,"position_y":-196.35081675232993},
			{"id":"step-mlz9brwx","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-315.42012228773933,"position_y":89.27073732627713}
		]`,
	},
	{
		ID:          "01KZA9M2PMVNZG3QPHQ7AS3GA1",
		VersionID:   "wfv_ui_human_architect_v1",
		Name:        "UI Development (Human) - Architect",
		VersionNote: "UI workflow with human approval + vision workers: DevOps (repo) -> Architect (Vision) -> Approval -> Senior SWE (Vision) -> PR Reviewer + QA (Vision, parallel) -> Human Approval -> DevOps (PR/merge)",
		StepsJSON: `[
			{"id":"step-repo","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":[],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-634.6434161696428,"position_y":-350.29286315717263,"edge_handles":{"e-step-qa-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-loop-2-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-09zk5l01-step-qa":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-sse":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-i64wso0x-step-sse":{"sourceHandle":"source-success","targetHandle":"target-top"},"e-step-sse-step-09zk5l01":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-repo-step-q4xlbg6v":{"sourceHandle":"source-right","targetHandle":"target-left"},"e-step-loop-2-step-approval":{"sourceHandle":"source-success"},"e-step-i64wso0x-step-q4xlbg6v":{"sourceHandle":"source-loop","targetHandle":"target-top"},"e-step-q4xlbg6v-step-i64wso0x":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-approval-step-devops-pr":{"sourceHandle":"source-success"},"e-step-pr-reviewer-step-loop-2":{"sourceHandle":"source-bottom","targetHandle":"target-top"},"e-step-09zk5l01-step-pr-reviewer":{"sourceHandle":"source-bottom","targetHandle":"target-top"}}},
			{"id":"step-sse","name":"Senior Software Engineer - Vision","kind":"task","ref":"w_se_sse_vision","worker_version":0,"depends_on":["step-i64wso0x"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-336.97734156059926,"position_y":-57},
			{"id":"step-pr-reviewer","name":"PR Reviewer","kind":"task","ref":"w_se_pr_reviewer","worker_version":0,"depends_on":["step-09zk5l01"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-403.7216769507495,"position_y":238.66836301259082},
			{"id":"step-qa","name":"QA Engineer - Vision","kind":"task","ref":"w_se_qa_vision","worker_version":0,"depends_on":["step-09zk5l01"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-142.53375885008086,"position_y":236.871602540136},
			{"id":"step-loop-2","name":"Loop Decision","kind":"loop_decision","ref":"","worker_version":0,"depends_on":["step-pr-reviewer","step-qa"],"gate_policy_ref":"","config":"{\"max_iterations\":6,\"loop_branch\":\"step-sse\",\"success_branch\":\"step-approval\"}","position_x":-259.9568641373869,"position_y":412.1037198547323},
			{"id":"step-approval","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-loop-2"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":6,\"success_branch\":\"step-devops-pr\",\"loop_branch\":\"step-sse\"}","position_x":-38.92911666021371,"position_y":553.5909366242397},
			{"id":"step-devops-pr","name":"DevOps Engineer","kind":"task","ref":"w_se_devops_engineer","worker_version":0,"depends_on":["step-approval"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":169.48358174252712,"position_y":671.1144583519153},
			{"id":"step-q4xlbg6v","name":"Principal Software Architect - Vision","kind":"task","ref":"w_se_architect_vision","worker_version":0,"depends_on":["step-repo"],"gate_policy_ref":"","config":"{\"recovery\":{\"strategy\":\"summarize_restart\",\"max_attempts\":3}}","position_x":-370.21653077813266,"position_y":-346.6528223715784},
			{"id":"step-i64wso0x","name":"Approval","kind":"approval","ref":"","worker_version":0,"depends_on":["step-q4xlbg6v"],"gate_policy_ref":"","config":"{\"reviewer\":\"human\",\"max_iterations\":6,\"success_branch\":\"step-sse\",\"loop_branch\":\"step-q4xlbg6v\"}","position_x":-333.41658002439067,"position_y":-196.35081675232993},
			{"id":"step-09zk5l01","name":"Parallel","kind":"parallel","ref":"","worker_version":0,"depends_on":["step-sse"],"gate_policy_ref":"","config":"{}","position_x":-306.7399649338158,"position_y":81.10492023910098}
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
			// Current version is still the original seed — update steps.
			_, err = ttx.Exec(ctx,
				`UPDATE workflow_versions SET steps = $1::jsonb, status = 'published'
				 WHERE id = $2 AND tenant_id = 'tnt_dev'`,
				w.StepsJSON, verID,
			)
			if err != nil {
				return fmt.Errorf("update seed version steps: %w", err)
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
