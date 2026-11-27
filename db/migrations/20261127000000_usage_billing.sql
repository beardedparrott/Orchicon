-- USAGE BILLING REGIME (plan-quota vs metered) — the marker that stops "$0.00" meaning "free".
--
-- WHY A COLUMN. An Antigravity run on `oauth-personal` / `oauth-business` draws on the operator's
-- plan quota: agy reports NO money figure at all (docs/adapters/antigravity-acp.md §8 row 8), and
-- there is no per-token USD to record. Storing cost_usd = 0 with no marker makes the cost explorer
-- render "$0.0000", which an operator reads as "this run was free" — a materially different claim
-- from "this run was included in a subscription they already pay for". The existing precedent for
-- "zero is not free" is orchicon.BillingDisclaimer (internal/orchicon/catalog_data.go); this column
-- is what lets the UI reach for the same honesty instead of a bare zero.
--
-- VOCABULARY. '' means metered (the default: a priced per-token call, which is what every row that
-- predates this migration is). 'plan_quota' means the turn was covered by a subscription plan and no
-- per-token price applies. A new regime must be added HERE and to the CHECK below, so a typo cannot
-- land a row the UI has no rule for.
ALTER TABLE usage_records
  ADD COLUMN IF NOT EXISTS billing text NOT NULL DEFAULT '';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'usage_records_billing_chk'
  ) THEN
    ALTER TABLE usage_records
      ADD CONSTRAINT usage_records_billing_chk
      CHECK (billing IN ('', 'plan_quota'));
  END IF;
END
$$;
