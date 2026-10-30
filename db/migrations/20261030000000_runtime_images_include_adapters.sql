-- include_adapters: install the runtime CLI adapters into a GENERATED image.
--
-- REPORTED AS A DESIGN GAP: an operator creating a runtime image in the console got a toolchain image with
-- no adapter in it, sent work to it, and found out when the first session could not spawn. Nobody creating
-- "a runtime image" is asking for one that cannot run any work.
--
-- DEFAULT TRUE for that reason, and NOT NULL so there is no third state to reason about. An image that
-- deliberately has no adapter (a shell-only toolchain, a build image) sets it false — a decision rather than
-- an omission.
ALTER TABLE runtime_images
  ADD COLUMN IF NOT EXISTS include_adapters boolean NOT NULL DEFAULT true;
