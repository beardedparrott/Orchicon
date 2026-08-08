-- Execution session transcript: the durable, append-only per-execution
-- record of the worker's opencode session (Stage 3 session transport).
--
-- Captures EVERY side of the conversation: outbound messages (goal,
-- liveness nudges, mid-run human messages via SendExecutionMessage),
-- assistant text, tool calls with input/output, reasoning, step
-- boundaries, and errors. The payload is the raw opencode part/message
-- JSON so the transcript is high-fidelity enough to reconstruct a session
-- for one-shot follow-ups and to view history after the serve/container
-- is gone.
--
-- Kept forever (it is the durable record); deleting the execution row
-- cascades and prunes it.
CREATE TABLE IF NOT EXISTS execution_session_parts (
  execution_id text NOT NULL REFERENCES worker_executions(id) ON DELETE CASCADE,
  tenant_id    text NOT NULL,
  seq          bigint NOT NULL,
  kind         text NOT NULL,
  payload      jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, execution_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_exec_session_parts_exec
  ON execution_session_parts (tenant_id, execution_id, seq);

ALTER TABLE execution_session_parts ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_session_parts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON execution_session_parts
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true));
