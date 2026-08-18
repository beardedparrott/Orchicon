-- Enforce the work-item dependency DAG invariant at the persistence
-- boundary: a BEFORE INSERT OR UPDATE trigger on work_item_dependencies
-- rejects any edge that would close a cycle (direct or multi-hop) in the
-- directional dependency graph (docs/02 §2.2, docs/09 §3.2/§11).
--
-- The app layer already cycle-checks its write paths (AddDependency,
-- UpdateWorkItem set-replace), but enforcement lives here so the DAG
-- invariant holds for EVERY writer — the API RPCs, any future bulk-import
-- path, and raw SQL. "Invalid graphs never persist" is a DB guarantee,
-- and the acceptance criteria explicitly guard the bulk-import path.
--
-- Semantics (mirror the app layer):
--   * self-loops (from_id = to_id) are rejected for EVERY edge type,
--     including relates_to — a self-loop is a trivial cycle and the app
--     layer blocks it unconditionally, so the DB stays consistent;
--   * relates_to rows are exempt from the reachability walk — it is a
--     symmetric, non-ordering relationship (A relates_to B + B
--     relates_to A is valid, and a relates_to edge must never be judged
--     by a depends_on/blocks reachability path);
--   * depends_on/blocks rows are checked with the same WITH RECURSIVE
--     reachability walk as the app-layer CheckCycleWithRecursiveCTE:
--     traverse forward from NEW.to_id; if NEW.from_id is reachable, the
--     edge closes a cycle and the transaction aborts (nothing persists);
--   * concurrent writers are serialized per project with a transactional
--     advisory lock taken before the walk. Two transactions inserting
--     A→B and B→A at the same time under READ COMMITTED would each walk
--     an empty (uncommitted) graph and both commit — a live cycle. The
--     lock is held until commit/abort, so the second writer re-walks
--     after the first commits, sees its edge, and is rejected. It is
--     reentrant within one transaction, so multi-edge writes (set-replace
--     on UpdateWorkItem, CreateWorkItem's depends_on block) contend only
--     on their first edge; only a transaction writing to multiple
--     projects can deadlock with another such transaction, which Postgres
--     deadlock detection resolves by aborting one.
--
-- The function is SECURITY INVOKER (default), so RLS scopes the
-- reachability walk to the row's tenant automatically; an INSERT outside
-- a tenant transaction is already impossible (FORCE ROW LEVEL SECURITY
-- rejects it before the trigger fires).
--
-- New function + trigger only (no table changes) — hand-authored; run
-- `make migrate-hash` after editing.

CREATE OR REPLACE FUNCTION enforce_work_dag_acyclic()
RETURNS trigger AS $$
BEGIN
  -- Self-loop: a trivial cycle on every edge type (including relates_to).
  -- Runs BEFORE the relates_to early return so a relates_to self-loop is
  -- refused too, consistent with the service-layer AddDependency rule.
  IF NEW.from_id = NEW.to_id THEN
    RAISE EXCEPTION 'cannot add dependency % -> %: would create a cycle in the work DAG',
      NEW.from_id, NEW.to_id;
  END IF;

  -- relates_to is symmetric and never an ordering edge: skip the
  -- reachability walk entirely.
  IF NEW.type = 'relates_to' THEN
    RETURN NEW;
  END IF;

  -- Serialize all DAG-edge writers for this tenant+project with a
  -- transactional advisory lock. Without it, two concurrent inserts that
  -- close a cycle (A→B and B→A) each walk the graph before the other
  -- commits and both pass under READ COMMITTED — a TOCTOU race that
  -- leaves a live cycle. Holding the lock to commit/abort makes the
  -- second writer re-walk after the first commits and see its edge. The
  -- lock is reentrant within a transaction (multi-edge writes take it
  -- once); keyed on the tenant+project pair so unrelated projects never
  -- contend.
  PERFORM pg_advisory_xact_lock(hashtextextended(NEW.tenant_id || ':' || NEW.project_id, 0));

  -- Reachability walk over DAG edges only (depends_on/blocks): traverse
  -- forward from NEW.to_id; if NEW.from_id is reachable, NEW closes a
  -- cycle. Same shape as db.CheckCycleWithRecursiveCTE.
  IF EXISTS (
    WITH RECURSIVE reach AS (
      SELECT to_id AS node FROM work_item_dependencies
      WHERE tenant_id = NEW.tenant_id AND project_id = NEW.project_id
        AND type IN ('depends_on', 'blocks') AND from_id = NEW.to_id
      UNION
      SELECT d.to_id FROM work_item_dependencies d
      JOIN reach r ON d.from_id = r.node
      WHERE d.tenant_id = NEW.tenant_id AND d.project_id = NEW.project_id
        AND d.type IN ('depends_on', 'blocks')
    )
    SELECT 1 FROM reach WHERE node = NEW.from_id
  ) THEN
    RAISE EXCEPTION 'cannot add dependency % -> %: would create a cycle in the work DAG',
      NEW.from_id, NEW.to_id;
  END IF;

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER enforce_work_dag_acyclic
BEFORE INSERT OR UPDATE ON work_item_dependencies
FOR EACH ROW EXECUTE FUNCTION enforce_work_dag_acyclic();
