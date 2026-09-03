"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import {
  formatCapture,
  manifestAge,
  type FrameGroup,
  type MachineManifest,
  type PoseValue,
} from "./lib/calibration";
import { MANIFEST } from "./lib/calibrationManifest";
import { getFramePose } from "./lib/viamClient";
import { useViamConnection } from "./lib/useViamConnection";

const LIVE_POLL_MS = 1000;

function CopyButton({
  label,
  text,
  title,
}: {
  label: string;
  text: string;
  title?: string;
}) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  return (
    <button
      onClick={async () => {
        try {
          // Undefined outside a secure context — an operator page served over
          // plain http would otherwise silently copy nothing.
          await navigator.clipboard.writeText(text);
          setState("copied");
          setTimeout(() => setState("idle"), 1200);
        } catch {
          setState("failed");
          setTimeout(() => setState("idle"), 3000);
        }
      }}
      title={title}
      className={`px-2 py-1 text-xs rounded border transition-colors whitespace-nowrap ${
        state === "failed"
          ? "border-red-200 bg-red-50 text-red-600"
          : "border-neutral-200 bg-white text-neutral-700 hover:bg-neutral-100"
      }`}
    >
      {state === "copied"
        ? "Copied"
        : state === "failed"
          ? "Copy failed"
          : label}
    </button>
  );
}

// --- live frame readout ---

type LiveFrames = Record<string, PoseValue | undefined>;

/**
 * Polls where the machine currently believes each frame is. This is the number
 * an operator is actually after: jog the arm, read the frame, paste it into one
 * of the poses listed below — the loop that otherwise runs through
 * `viam robot part motion get-pose` and a manual transcription.
 */
function useLiveFrames(
  partId: string,
  frames: string[],
): { poses: LiveFrames; error: string | null } {
  const { conn, error: connError } = useViamConnection(partId);
  const [poses, setPoses] = useState<LiveFrames>({});
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!conn) return;
    let cancelled = false;

    const read = async () => {
      // Each read is one RPC per frame against a real robot. An operator page
      // left open in a background tab would otherwise poll it forever.
      if (document.hidden) return;

      const results = await Promise.all(
        frames.map(async (frame) => {
          try {
            return { frame, pose: await getFramePose(conn, frame) };
          } catch (e) {
            // One unresolvable frame shouldn't blank the others.
            return { frame, err: e instanceof Error ? e.message : String(e) };
          }
        }),
      );
      if (cancelled) return;

      setPoses(Object.fromEntries(results.map((r) => [r.frame, r.pose])));
      // First failure in frame order, so the message doesn't depend on which
      // request happened to settle last.
      const failed = results.find((r) => r.err);
      setError(failed ? `${failed.frame}: ${failed.err}` : null);
    };

    read();
    const timer = setInterval(read, LIVE_POLL_MS);
    // Refresh on return rather than making the operator wait out a tick.
    const onVisible = () => {
      if (!document.hidden) read();
    };
    document.addEventListener("visibilitychange", onVisible);

    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
    // frames must be referentially stable — MachineView memoizes it.
  }, [conn, frames]);

  return { poses, error: connError ?? error };
}

function LiveFrameBar({
  frames,
  poses,
  error,
}: {
  frames: string[];
  poses: LiveFrames;
  error: string | null;
}) {
  return (
    <section className="mb-5 border border-neutral-200 rounded-lg bg-white overflow-hidden">
      <header className="flex flex-wrap items-center gap-x-2 gap-y-1 px-3 py-1.5 bg-neutral-900 text-white">
        <span className="text-xs font-medium">Live frame positions</span>
        <span className="text-[11px] text-neutral-400">
          where the arm is right now — jog it, then copy into a pose below
        </span>
        {error && (
          <span className="ml-auto text-[11px] text-red-300">{error}</span>
        )}
      </header>
      {/* Side by side: the full JSON is tall, and pushing the pose lists off
          screen would separate the reading from the names it pastes into. */}
      <div className="grid gap-3 p-3 sm:grid-cols-2 lg:grid-cols-3">
        {frames.map((frame) => {
          const pose = poses[frame];
          // One string for both the preview and the clipboard, so what you read
          // is exactly what you paste.
          const json = pose && JSON.stringify(pose, null, 2);
          return (
            <div key={frame}>
              <div className="flex items-center gap-2 mb-1">
                <span className="shrink-0 px-2 py-0.5 text-xs font-mono rounded bg-neutral-900 text-white">
                  {frame}
                </span>
                {json ? (
                  <span className="ml-auto">
                    <CopyButton
                      label="Copy"
                      title={`Paste into a ${frame} pose`}
                      text={json}
                    />
                  </span>
                ) : (
                  <span className="text-sm text-neutral-400">reading…</span>
                )}
              </div>
              {json && (
                <pre className="font-mono text-xs text-neutral-900 bg-neutral-50 border border-neutral-200 rounded px-2 py-1.5 overflow-x-auto">
                  {json}
                </pre>
              )}
            </div>
          );
        })}
      </div>
    </section>
  );
}

// --- staleness ---

const AGE_STYLE = {
  warn: "border-amber-300 bg-amber-50 text-amber-900",
  stale: "border-red-300 bg-red-50 text-red-800",
} as const;

function StalenessBanner({ generatedAt }: { generatedAt: string }) {
  // Safe to read the clock during render: this view sits behind the
  // useSearchParams Suspense boundary in page.tsx, so the static export never
  // prerenders it and there is no server output to mismatch against.
  const age = manifestAge(generatedAt);

  return (
    <div
      className={`mb-5 px-3 py-2.5 rounded-lg border ${AGE_STYLE[age.level]}`}
    >
      <div className="text-sm font-medium">
        ⚠ Pose list captured {formatCapture(generatedAt, age)}
      </div>
      <div className="text-xs mt-0.5">
        This page cannot tell whether a pose has been added, removed, or renamed
        since — a list captured today is already wrong if someone edited a pose
        after it was generated. Re-run{" "}
        <span className="font-mono font-semibold">make web-app-manifest</span>{" "}
        after any pose change.
      </div>
    </div>
  );
}

// --- pose lists ---

function FrameCard({ group }: { group: FrameGroup }) {
  return (
    <section className="mb-6 border border-neutral-200 rounded-lg overflow-hidden bg-white">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 bg-neutral-50 border-b border-neutral-200">
        <span className="shrink-0 px-2 py-0.5 text-xs font-mono rounded bg-neutral-900 text-white">
          {group.frame}
        </span>
        <span className="shrink-0 text-sm text-neutral-500 font-mono">
          {group.switch}
        </span>
        <span className="text-xs text-neutral-400">
          {group.configure.length} to configure · {group.derived.length} derived
        </span>
      </header>

      <div className="px-3 py-2">
        <h3 className="text-[11px] uppercase tracking-wide text-neutral-400 mb-1">
          Set these against the {group.frame} frame
        </h3>
        <ul className="flex flex-wrap gap-1.5">
          {group.configure.map((pose) => (
            <li
              key={pose}
              className="px-2 py-1 rounded-md border border-neutral-300 bg-white font-mono text-sm text-neutral-900 shadow-sm"
            >
              {pose}
            </li>
          ))}
        </ul>
      </div>

      {group.derived.length > 0 && (
        <div className="px-3 py-2 border-t border-neutral-100 bg-neutral-50/60">
          <h3 className="text-[11px] uppercase tracking-wide text-neutral-400 mb-1">
            Follow another pose — change the baseline, not these
          </h3>
          <ul className="grid gap-x-6 gap-y-1 sm:grid-cols-2">
            {group.derived.map((d) => (
              <li key={d.pose} className="font-mono text-xs text-neutral-500">
                {d.pose}{" "}
                <span className="text-neutral-400">← {d.baseline}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

function MachineView({ manifest }: { manifest: MachineManifest }) {
  // One readout per distinct frame the machine's switches move.
  const frames = useMemo(
    () => [...new Set(manifest.frames.map((f) => f.frame))],
    [manifest],
  );
  const { poses, error } = useLiveFrames(manifest.partId, frames);

  return (
    <>
      <StalenessBanner generatedAt={manifest.generatedAt} />
      <LiveFrameBar frames={frames} poses={poses} error={error} />
      {manifest.frames.map((group) => (
        <FrameCard key={group.switch} group={group} />
      ))}
    </>
  );
}

export function Calibrate() {
  // Keyed on partId, not name: it's what the fleet dashboard already has for
  // each row, and it can't collide the way a machine name can.
  const requested = useSearchParams().get("partId");
  // Only fall back to the first machine when no machine was asked for. A
  // requested-but-missing partId must say so: quietly showing another
  // machine's poses is how someone calibrates the wrong robot.
  const manifest = requested
    ? MANIFEST.find((m) => m.partId === requested)
    : MANIFEST[0];

  if (!manifest) {
    return (
      <main className="max-w-3xl mx-auto p-6">
        <Link
          href="/"
          className="inline-block mb-3 text-sm text-neutral-500 hover:text-neutral-900 transition-colors"
        >
          ← Back to Fleet Dashboard
        </Link>
        <h1 className="text-xl font-semibold text-neutral-900">
          Pose calibration
        </h1>
        <p className="mt-2 text-sm text-neutral-500">
          No pose list for{" "}
          {requested ? (
            <span className="font-mono">{requested}</span>
          ) : (
            "any machine"
          )}
          . Generate one with{" "}
          <span className="font-mono">
            make web-app-manifest PART_IDS=&quot;{requested ?? "<partId>"}&quot;
          </span>
          .
        </p>
        {MANIFEST.length > 0 && (
          <p className="mt-3 text-sm text-neutral-500">
            Machines with a pose list:{" "}
            {MANIFEST.map((m) => (
              <Link
                key={m.partId}
                href={`/?view=calibrate&partId=${m.partId}`}
                className="text-blue-600 mr-2 hover:underline"
              >
                {m.machine}
              </Link>
            ))}
          </p>
        )}
      </main>
    );
  }

  return (
    <main className="max-w-5xl mx-auto p-6">
      <Link
        href="/"
        className="inline-block mb-3 text-sm text-neutral-500 hover:text-neutral-900 transition-colors"
      >
        ← Back to Fleet Dashboard
      </Link>
      <div className="flex flex-wrap items-baseline gap-3 mb-1">
        <h1 className="text-xl font-semibold text-neutral-900">
          Pose calibration
        </h1>
        {MANIFEST.map((m) => (
          <Link
            key={m.partId}
            href={`/?view=calibrate&partId=${m.partId}`}
            className={`text-sm ${
              m.machine === manifest.machine
                ? "text-neutral-900 font-medium"
                : "text-neutral-400 hover:text-neutral-900"
            }`}
          >
            {m.machine}
          </Link>
        ))}
      </div>
      <p className="text-sm text-neutral-500 mb-4">
        Which poses belong to which frame. Jog the arm, copy the live frame
        position, and paste it into one of the poses listed for that frame.
      </p>
      <MachineView manifest={manifest} />
    </main>
  );
}
