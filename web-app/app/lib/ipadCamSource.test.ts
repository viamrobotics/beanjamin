import { test } from "node:test";
import assert from "node:assert/strict";
import {
  getSourceId,
  isLeaseError,
  isTokenError,
  parseIpadCamParam,
} from "./ipadCamSource";

test("parseIpadCamParam: off when absent, empty or 0", () => {
  for (const v of [null, "", "0"]) {
    assert.deepEqual(parseIpadCamParam(v), { enabled: false });
  }
});

test("parseIpadCamParam: 1 means on with no token", () => {
  assert.deepEqual(parseIpadCamParam("1"), { enabled: true });
});

test("parseIpadCamParam: any other value is the token", () => {
  assert.deepEqual(parseIpadCamParam("s3cret"), {
    enabled: true,
    token: "s3cret",
  });
});

test("rejection messages are recognised inside wrapped errors", () => {
  assert.ok(
    isLeaseError(
      "rpc error: code = Unknown desc = ipad-camera: in use by another source",
    ),
  );
  assert.ok(isTokenError("rpc error: desc = ipad-camera: invalid token"));
  assert.ok(!isLeaseError("ipad-camera: invalid token"));
  assert.ok(!isTokenError("ipad-camera: in use by another source"));
});

function memoryStore() {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
  };
}

test("getSourceId is stable across calls on the same store", () => {
  const store = memoryStore();
  const first = getSourceId(store);
  assert.ok(first.length > 0);
  assert.equal(getSourceId(store), first);
});

test("getSourceId differs between browsers", () => {
  assert.notEqual(getSourceId(memoryStore()), getSourceId(memoryStore()));
});

test("getSourceId still returns an id when storage throws", () => {
  const broken = {
    getItem: () => {
      throw new Error("blocked");
    },
    setItem: () => {
      throw new Error("blocked");
    },
  };
  assert.ok(getSourceId(broken).length > 0);
});
