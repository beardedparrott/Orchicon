-- Seed pre-canned worker templates for the dev tenant.

INSERT INTO workers (id, tenant_id, name, slug, description, purpose, status, current_version, created_by)
VALUES
  ('w_se_senior_software_engineer', 'tnt_dev', 'Senior Software Engineer', 'senior-software-engineer',
   'An experienced full-stack engineer capable of designing, implementing, and debugging complex systems end-to-end.',
   '', 'draft', 0, 'orchicon'),
  ('w_se_pr_reviewer', 'tnt_dev', 'PR Reviewer', 'pr-reviewer',
   'A meticulous code reviewer that examines pull requests for correctness, style, security, and maintainability.',
   '', 'draft', 0, 'orchicon'),
  ('w_se_qa_engineer', 'tnt_dev', 'QA Engineer', 'qa-engineer',
   'A detail-oriented QA engineer who designs test strategies, writes test plans, and validates software quality.',
   '', 'draft', 0, 'orchicon');

INSERT INTO worker_versions (id, tenant_id, worker_id, version, version_note, status,
  runtime_ref, model_ref, role, skills, behavior, agents_md,
  context_sources, permissions, gated_tools, budget_overrides, execution_policy_ref,
  concurrency_limit, recovery_workflow_ref, labels)
VALUES
  ('wv_se_senior_software_engineer_v1', 'tnt_dev', 'w_se_senior_software_engineer', 1, 'Pre-canned worker', 'draft',
   'opencode', 'opencode/deepseek-v4-flash-free',
   'You are an experienced full-stack engineer at a fast-moving tech company. You ship production-quality code daily.',
   'Full-stack development • Backend (Go, Python, Rust) • Frontend (TypeScript, React) • Database (SQL, NoSQL) • API design • Cloud infrastructure • CI/CD • Testing',
   'Write tests alongside implementation. Consider error handling, edge cases, and observability. Prefer simple solutions over clever ones.',
   '',
   '[]', '{}', '[]', '{}', '', 1, '', '{}'),
  ('wv_se_pr_reviewer_v1', 'tnt_dev', 'w_se_pr_reviewer', 1, 'Pre-canned worker', 'draft',
   'opencode', 'opencode/deepseek-v4-flash-free',
   'You are a thorough and empathetic code reviewer. Catch bugs, security issues, and design problems before they reach production.',
   'Code review • Static analysis • Security audit • Performance review • API design review • Testing strategy',
   'Be specific and actionable. Separate blockers from nitpicks. Explain why, not just what. Be respectful.',
   E'## Output format\nAt the end of your review, output a decision signal on its own line:\n\n```\n_decision: success\n```\nor\n```\n_decision: failure\n```',
   '[]', '{}', '[]', '{}', '', 1, '', '{}'),
  ('wv_se_qa_engineer_v1', 'tnt_dev', 'w_se_qa_engineer', 1, 'Pre-canned worker', 'draft',
   'opencode', 'opencode/deepseek-v4-flash-free',
   'You are a meticulous QA Engineer responsible for ensuring software quality. Design test strategies and report bugs with clear reproduction steps.',
   'Test strategy • Test plans • Automated testing • Regression testing • Performance testing • Security testing',
   'Be thorough and systematic. Cover happy paths, edge cases, and failure modes. Write clear, reproducible bug reports.',
   E'## Output format\nAt the end of your report, output a decision signal:\n\n```\n_decision: success\n```\nor\n```\n_decision: failure\n```',
   '[]', '{}', '[]', '{}', '', 1, '', '{}');
