import { test } from "node:test";
import assert from "node:assert/strict";
import {
  createConnectionManager,
  withTimeout,
  disconnectQuietly,
} from "./connectionManager";
import { type ViamConnection } from "./viamClient";

// A deferred promise we can settle from the test body.
function defer<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function fakeConn(over: Partial<ViamConnection> = {}): ViamConnection {
  return {
    viamClient: {} as ViamConnection["viamClient"],
    robotClient: {
      disconnect: async () => {},
    } as unknown as ViamConnection["robotClient"],
    machineId: "machine-1",
    hostname: "host",
    isDev: false,
    ...over,
  };
}

// --- withTimeout ---

test("withTimeout resolves with the underlying value before the deadline", async () => {
  const value = await withTimeout(Promise.resolve(42), 1000, "x");
  assert.equal(value, 42);
});

test("withTimeout rejects with a labelled error when the promise hangs", async () => {
  await assert.rejects(
    () => withTimeout(new Promise(() => {}), 10, "dial"),
    /dial timed out after 10ms/
  );
});

// --- createConnectionManager ---

test("get() dials once and caches the result for subsequent calls", async () => {
  let dials = 0;
  const conn = fakeConn();
  const mgr = createConnectionManager(async () => {
    dials++;
    return conn;
  });

  assert.equal(await mgr.get("a"), conn);
  assert.equal(await mgr.get("a"), conn);
  assert.equal(dials, 1, "second get() should reuse the cached connection");
});

test("concurrent get()s for one partId share a single in-flight dial", async () => {
  let dials = 0;
  const d = defer<ViamConnection>();
  const mgr = createConnectionManager(() => {
    dials++;
    return d.promise;
  });

  const p1 = mgr.get("a");
  const p2 = mgr.get("a");
  assert.equal(dials, 1, "the two concurrent gets must not stack dials");

  const conn = fakeConn();
  d.resolve(conn);
  assert.equal(await p1, conn);
  assert.equal(await p2, conn);
});

test("distinct partIds dial independently", async () => {
  const conns: Record<string, ViamConnection> = {
    a: fakeConn({ machineId: "a" }),
    b: fakeConn({ machineId: "b" }),
  };
  const mgr = createConnectionManager(async (id) => conns[id]);

  assert.equal((await mgr.get("a")).machineId, "a");
  assert.equal((await mgr.get("b")).machineId, "b");
});

test("a failed dial is evicted so the next get() re-dials", async () => {
  let dials = 0;
  const conn = fakeConn();
  const mgr = createConnectionManager(async () => {
    dials++;
    if (dials === 1) throw new Error("boom");
    return conn;
  });

  await assert.rejects(() => mgr.get("a"), /boom/);
  // The rejected promise must not stay cached.
  assert.equal(await mgr.get("a"), conn);
  assert.equal(dials, 2);
});

test("invalidate() drops the cached connection, disconnects it, and forces a re-dial", async () => {
  let dials = 0;
  let disconnects = 0;
  const conns = [
    fakeConn({
      robotClient: {
        disconnect: async () => {
          disconnects++;
        },
      } as unknown as ViamConnection["robotClient"],
    }),
    fakeConn(),
  ];
  const mgr = createConnectionManager(async () => conns[dials++]);

  const first = await mgr.get("a");
  assert.equal(first, conns[0]);

  mgr.invalidate("a");
  // Let the fire-and-forget teardown run.
  await Promise.resolve();
  assert.equal(disconnects, 1, "invalidate() should disconnect the channel");

  assert.equal(await mgr.get("a"), conns[1]);
  assert.equal(dials, 2, "invalidate() should force the next get() to re-dial");
});

test("a late-rejecting dial does not evict a newer entry that replaced it", async () => {
  // Models the race the eviction guard exists for: get() #1 is still in flight
  // when it's invalidated and get() #2 dials a fresh entry; #1 then rejects and
  // must not delete #2's entry.
  const d1 = defer<ViamConnection>();
  const d2 = defer<ViamConnection>();
  const deferreds = [d1, d2];
  let i = 0;
  const mgr = createConnectionManager(() => deferreds[i++].promise);

  const p1 = mgr.get("a"); // entry = d1
  mgr.invalidate("a"); // evicts d1 (still pending)
  const p2 = mgr.get("a"); // entry = d2

  const fresh = fakeConn({ machineId: "fresh" });
  d1.reject(new Error("stale dial failed")); // must NOT touch d2's entry
  await assert.rejects(() => p1, /stale dial failed/);

  d2.resolve(fresh);
  assert.equal(await p2, fresh);
  // The entry survived #1's rejection, so this resolves from cache (no re-dial).
  assert.equal(await mgr.get("a"), fresh);
  assert.equal(i, 2, "only d1 and d2 should have been dialed");
});

test("closeAll() tears down every cached connection", async () => {
  let disconnects = 0;
  const mkConn = () =>
    fakeConn({
      robotClient: {
        disconnect: async () => {
          disconnects++;
        },
      } as unknown as ViamConnection["robotClient"],
    });
  const conns: Record<string, ViamConnection> = { a: mkConn(), b: mkConn() };
  const mgr = createConnectionManager(async (id) => conns[id]);

  await mgr.get("a");
  await mgr.get("b");
  mgr.closeAll();
  await Promise.resolve();
  assert.equal(disconnects, 2);

  // After closeAll the pool is empty, so this re-dials rather than reusing.
  let redials = 0;
  const mgr2 = createConnectionManager(async () => {
    redials++;
    return fakeConn();
  });
  await mgr2.get("a");
  mgr2.closeAll();
  await mgr2.get("a");
  assert.equal(redials, 2);
});

// --- disconnectQuietly ---

test("disconnectQuietly skips dev-mode connections", () => {
  let called = false;
  const conn = fakeConn({
    isDev: true,
    robotClient: {
      disconnect: async () => {
        called = true;
      },
    } as unknown as ViamConnection["robotClient"],
  });
  disconnectQuietly(conn);
  assert.equal(called, false);
});

test("disconnectQuietly swallows a rejected disconnect", async () => {
  const conn = fakeConn({
    robotClient: {
      disconnect: async () => {
        throw new Error("already gone");
      },
    } as unknown as ViamConnection["robotClient"],
  });
  // Must not throw synchronously or reject an unhandled rejection.
  assert.doesNotThrow(() => disconnectQuietly(conn));
  await Promise.resolve();
});
