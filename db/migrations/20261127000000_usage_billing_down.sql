-- Reverse of 20261127000000_usage_billing.sql.
--
-- Dropping the column loses NO money figure: a plan-quota row's cost_usd is 0 and stays 0, so a
-- reverted deployment reads the same numbers it read before the migration — it merely stops being
-- able to distinguish "included in plan" from "free". The tokens, which is what a budget enforcement
-- actually spends, are untouched.
ALTER TABLE usage_records
  DROP CONSTRAINT IF EXISTS usage_records_billing_chk;

ALTER TABLE usage_records
  DROP COLUMN IF EXISTS billing;
