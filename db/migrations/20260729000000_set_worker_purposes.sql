-- Set purpose fields for the DevOps Engineer and AI Approver workers.
-- These were seeded with empty purpose in previous migrations.

UPDATE workers SET purpose = 'Automates repository management, CI/CD, and PR workflows. Creates repos under the authenticated GitHub account and merges code after approval.' WHERE id = 'w_se_devops_engineer';

UPDATE workers SET purpose = 'AI-based approval authority that reviews upstream context and decides whether work meets the acceptance criteria.' WHERE id = 'w_se_ai_approver';
