import { describe, expect, it } from "vitest";

import {
  WorkerStatus,
  type Worker,
} from "@/api/gen/orchicon/api/v1/worker_pb";
import {
  BulkUpdateWorkerModelSkipReason,
} from "@/api/gen/orchicon/api/v1/worker_service_pb";
import {
  buildBulkPreview,
  SKIP_REASON_LABEL,
} from "./bulkChangeWorkerModel";

function makeWorker(id: string, opts: Partial<Worker> = {}): Worker {
  return {
    id,
    tenantId: "tnt_test",
    name: `Worker ${id}`,
    slug: id,
    description: "",
    purpose: "",
    status: WorkerStatus.PUBLISHED,
    currentVersion: 1,
    createdBy: "",
    version: 1,
    createdAt: undefined,
    updatedAt: undefined,
    ...opts,
  } as Worker;
}

describe("buildBulkPreview", () => {
  it("returns zero counts when no workers are loaded yet", () => {
    const preview = buildBulkPreview(undefined, ["w1", "w2"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({});
  });

  it("counts a published worker with a version as updatable", () => {
    const workers = [makeWorker("w1")];
    const preview = buildBulkPreview(workers, ["w1"]);
    expect(preview.updatable).toBe(1);
    expect(preview.skipped).toEqual({});
  });

  it("counts a draft worker (currentVersion=1 but status=draft) as updatable", () => {
    const workers = [
      makeWorker("w1", { status: WorkerStatus.DRAFT }),
    ];
    const preview = buildBulkPreview(workers, ["w1"]);
    expect(preview.updatable).toBe(1);
    expect(preview.skipped).toEqual({});
  });

  it("skips deprecated workers with the deprecated reason", () => {
    const workers = [
      makeWorker("w1", { status: WorkerStatus.DEPRECATED }),
    ];
    const preview = buildBulkPreview(workers, ["w1"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({ deprecated: 1 });
  });

  it("skips retired workers with the retired reason", () => {
    const workers = [
      makeWorker("w1", { status: WorkerStatus.RETIRED }),
    ];
    const preview = buildBulkPreview(workers, ["w1"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({ retired: 1 });
  });

  it("skips workers with no published version", () => {
    const workers = [
      makeWorker("w1", { currentVersion: 0 }),
    ];
    const preview = buildBulkPreview(workers, ["w1"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({ "no published version": 1 });
  });

  it("skips ids that are not in the loaded list with the not-found reason", () => {
    const workers = [makeWorker("w1")];
    const preview = buildBulkPreview(workers, ["w_ghost"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({ "not found": 1 });
  });

  it("mixes updatable + multiple skip reasons in one batch", () => {
    const workers = [
      makeWorker("a"), // updatable
      makeWorker("b", { status: WorkerStatus.DEPRECATED }),
      makeWorker("c", { status: WorkerStatus.RETIRED }),
      makeWorker("d", { currentVersion: 0 }),
    ];
    const preview = buildBulkPreview(workers, ["a", "b", "c", "d", "ghost"]);
    expect(preview.updatable).toBe(1);
    expect(preview.skipped).toEqual({
      deprecated: 1,
      retired: 1,
      "no published version": 1,
      "not found": 1,
    });
  });

  it("counts multiple skipped workers of the same reason together", () => {
    const workers = [
      makeWorker("b1", { status: WorkerStatus.DEPRECATED }),
      makeWorker("b2", { status: WorkerStatus.DEPRECATED }),
    ];
    const preview = buildBulkPreview(workers, ["b1", "b2"]);
    expect(preview.updatable).toBe(0);
    expect(preview.skipped).toEqual({ deprecated: 2 });
  });
});

describe("SKIP_REASON_LABEL", () => {
  it("maps every server-side skip reason to a user-facing label", () => {
    expect(SKIP_REASON_LABEL[BulkUpdateWorkerModelSkipReason.NOT_FOUND]).toBe(
      "not found",
    );
    expect(
      SKIP_REASON_LABEL[BulkUpdateWorkerModelSkipReason.DEPRECATED],
    ).toBe("deprecated");
    expect(SKIP_REASON_LABEL[BulkUpdateWorkerModelSkipReason.RETIRED]).toBe(
      "retired",
    );
    expect(
      SKIP_REASON_LABEL[
        BulkUpdateWorkerModelSkipReason.NO_PUBLISHED_VERSION
      ],
    ).toBe("no published version");
    expect(
      SKIP_REASON_LABEL[BulkUpdateWorkerModelSkipReason.UNSPECIFIED],
    ).toBe("skipped");
  });
});
