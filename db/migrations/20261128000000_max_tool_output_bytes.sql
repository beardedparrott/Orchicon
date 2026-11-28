-- The per-tool-result cap: how much of a single tool's output is kept in model context.
--
-- It was an env var only (ORCHICON_MAX_TOOL_OUTPUT_BYTES), honoured by the opencode adapter and — after
-- this cycle — hardcoded on the native path until it read the same var. That made the single largest lever
-- on per-turn cost unreachable from Settings: every byte kept here is re-sent on EVERY subsequent turn, so
-- an operator had no way to tune context growth without shell access to the control plane.
--
-- NULL means UNSET, and unset is not "zero": it falls back to the env var and then to the built-in default
-- (128 KiB). A stored 0 is an explicit operator value and is honoured as such.
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS max_tool_output_bytes bigint;
