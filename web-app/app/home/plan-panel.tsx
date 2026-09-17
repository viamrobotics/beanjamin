"use client";

import { useEffect, useRef, useState } from "react";
import * as VIAM from "@viamrobotics/sdk";
import {
  type PlanRequestFile,
  loadPlanRequestsForOrder,
  getBinarySignedUrl,
} from "./data";

type PlanState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ready"; files: PlanRequestFile[] };

// Plans issued outside any brew step carry no step_ tag.
const NO_STEP_LABEL = "Outside a step";

const CHIP_BASE = "px-2.5 py-1 text-xs rounded-full border transition-colors";

function blockClass(file: PlanRequestFile, selected: boolean): string {
  if (selected) return "bg-neutral-900 border-neutral-900 text-white";
  return file.ok
    ? "bg-blue-100 border-blue-300 text-blue-900 hover:bg-blue-200"
    : "bg-red-700 border-red-800 text-white hover:bg-red-800";
}

/** Groups plans into their step, in the order the steps first ran. */
function lanesOf(files: PlanRequestFile[]): { step: string; plans: PlanRequestFile[] }[] {
  const lanes: { step: string; plans: PlanRequestFile[] }[] = [];
  for (const f of files) {
    const step = f.step || NO_STEP_LABEL;
    const lane = lanes.find((l) => l.step === step);
    if (lane) lane.plans.push(f);
    else lanes.push({ step, plans: [f] });
  }
  return lanes;
}

async function downloadPlan(
  client: VIAM.ViamClient,
  file: PlanRequestFile
): Promise<void> {
  const url = await getBinarySignedUrl(client, file.binaryDataId);
  const a = document.createElement("a");
  a.href = url;
  a.download = file.fileName;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

function DownloadButton({
  file,
  viamClient,
}: {
  file: PlanRequestFile;
  viamClient: VIAM.ViamClient | null;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <button
      disabled={!viamClient || busy}
      onClick={() => {
        if (!viamClient) return;
        setBusy(true);
        downloadPlan(viamClient, file)
          .catch((e) => console.error("failed to download plan request:", e))
          .finally(() => setBusy(false));
      }}
      className="px-2 py-1 text-xs rounded-md border border-neutral-200 bg-white hover:bg-neutral-100 transition-colors disabled:text-neutral-400 disabled:cursor-not-allowed"
    >
      {busy ? "…" : "Download"}
    </button>
  );
}

function PlanFlow({
  files,
  selected,
  onPick,
}: {
  files: PlanRequestFile[];
  selected: number | null;
  onPick: (file: PlanRequestFile) => void;
}) {
  return (
    <div className="rounded-md border border-neutral-200 bg-white p-3">
      <div className="flex flex-col gap-2">
        {lanesOf(files).map((lane) => (
          <div key={lane.step} className="flex items-center gap-3">
            <span className="w-40 shrink-0 text-right text-xs">{lane.step}</span>
            <div className="flex grow flex-wrap gap-1">
              {lane.plans.map((p) => (
                <button
                  key={p.binaryDataId}
                  onClick={() => onPick(p)}
                  title={`#${p.seq} · ${p.motion || "plan"} · ${
                    p.ok ? "planned OK" : "planning failed"
                  }`}
                  className={`min-w-[26px] h-6 px-1 rounded font-mono text-[10px] font-semibold border ${blockClass(
                    p,
                    p.seq === selected
                  )}`}
                >
                  {p.seq}
                </button>
              ))}
            </div>
            <span className="w-16 shrink-0 text-right text-xs text-neutral-500">
              {lane.plans.length} plans
            </span>
          </div>
        ))}
      </div>
      <p className="mt-3 pt-2 border-t border-neutral-100 text-xs text-neutral-500">
        Numbered in the order they were requested. Pick one to jump to its file.
      </p>
    </div>
  );
}

function PlanTable({
  files,
  selected,
  viamClient,
}: {
  files: PlanRequestFile[];
  selected: number | null;
  viamClient: VIAM.ViamClient | null;
}) {
  const selectedRef = useRef<HTMLTableRowElement>(null);

  useEffect(() => {
    if (selected === null) return;
    selectedRef.current?.scrollIntoView({ block: "nearest" });
  }, [selected]);

  return (
    <div className="max-h-80 overflow-y-auto rounded-md border border-neutral-200 bg-white">
      <table className="w-full border-collapse text-xs">
        <thead>
          <tr className="text-left text-neutral-500">
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200 w-10">
              #
            </th>
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200">
              File
            </th>
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200 w-24">
              Motion
            </th>
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200 w-16">
              Result
            </th>
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200 w-24">
              Captured
            </th>
            <th className="sticky top-0 z-10 bg-neutral-50 px-2 py-1.5 font-medium border-b border-neutral-200 w-24" />
          </tr>
        </thead>
        <tbody>
          {files.map((f) => (
            <tr
              key={f.binaryDataId}
              ref={f.seq === selected ? selectedRef : undefined}
              className={`border-t border-neutral-100 ${
                f.seq === selected
                  ? "bg-neutral-100"
                  : f.ok
                    ? ""
                    : "bg-red-50"
              }`}
            >
              <td className="px-2 py-1.5 font-mono text-neutral-500">{f.seq}</td>
              <td
                className="px-2 py-1.5 font-mono truncate max-w-0"
                title={f.fileName}
              >
                {f.fileName}
              </td>
              <td className="px-2 py-1.5 font-mono text-neutral-600">
                {f.motion || "—"}
              </td>
              <td
                className={`px-2 py-1.5 font-semibold ${
                  f.ok ? "text-green-700" : "text-red-700"
                }`}
              >
                {f.ok ? "planned" : "failed"}
              </td>
              <td className="px-2 py-1.5 text-neutral-500">{f.at || "—"}</td>
              <td className="px-2 py-1.5">
                <DownloadButton file={f} viamClient={viamClient} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function PlanPanel({
  orderId,
  viamClient,
}: {
  orderId: string;
  viamClient: VIAM.ViamClient | null;
}) {
  const [open, setOpen] = useState(false);
  const [state, setState] = useState<PlanState>({ kind: "idle" });
  const [stepFilter, setStepFilter] = useState<string | null>(null);
  const [failuresOnly, setFailuresOnly] = useState(false);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<number | null>(null);

  // Fetched when the section is first opened, not with the row: an order's
  // plans are a separate query and most rows are opened to watch a clip.
  const toggle = () => {
    setOpen((o) => !o);
    if (open || state.kind !== "idle" || !viamClient) return;
    setState({ kind: "loading" });
    loadPlanRequestsForOrder(viamClient, orderId)
      .then((files) => setState({ kind: "ready", files }))
      .catch((e) => {
        console.error("failed to load plan requests:", e);
        setState({
          kind: "error",
          message: e instanceof Error ? e.message : String(e),
        });
      });
  };

  const all = state.kind === "ready" ? state.files : [];
  const failureCount = all.filter((f) => !f.ok).length;
  const stepCount = lanesOf(all).length;

  const term = search.trim().toLowerCase();
  const shown = all.filter((f) => {
    if (stepFilter !== null && (f.step || NO_STEP_LABEL) !== stepFilter)
      return false;
    if (failuresOnly && f.ok) return false;
    if (
      term &&
      !f.fileName.toLowerCase().includes(term) &&
      !f.motion.toLowerCase().includes(term)
    )
      return false;
    return true;
  });

  // Picking a block narrows to its step and drops every other filter, so the
  // row it points at is always one that is actually rendered.
  const pick = (file: PlanRequestFile) => {
    setStepFilter(file.step || NO_STEP_LABEL);
    setFailuresOnly(false);
    setSearch("");
    setSelected(file.seq);
  };

  return (
    <div className="rounded-md border border-neutral-200 bg-white">
      <div className="flex items-center gap-2 px-3 py-2">
        <button
          onClick={toggle}
          aria-expanded={open}
          className="flex items-center gap-2 text-sm text-neutral-900"
        >
          <span className="text-[10px] text-neutral-500">
            {open ? "▼" : "▶"}
          </span>
          <span className="font-semibold">Motion plans</span>
        </button>
        {state.kind === "ready" && (
          <span className="text-xs text-neutral-500">
            {all.length} requests across {stepCount}{" "}
            {stepCount === 1 ? "step" : "steps"}
            {failureCount > 0 && (
              <>
                {" · "}
                <span className="text-red-700 font-semibold">
                  {failureCount} planning{" "}
                  {failureCount === 1 ? "failure" : "failures"}
                </span>
              </>
            )}
          </span>
        )}
      </div>

      {open && (
        <div className="px-3 pb-3 flex flex-col gap-3">
          {state.kind === "loading" && (
            <p className="text-sm text-neutral-500 m-0">Loading plan requests…</p>
          )}
          {state.kind === "error" && (
            <p className="text-sm text-red-700 m-0">Error: {state.message}</p>
          )}
          {state.kind === "ready" && all.length === 0 && (
            <p className="text-sm text-neutral-500 m-0">
              No plan files for this order — check that{" "}
              <code className="font-mono">save_motion_requests_dir</code> is set
              on this machine.
            </p>
          )}
          {state.kind === "ready" && all.length > 0 && (
            <>
              <PlanFlow files={all} selected={selected} onPick={pick} />

              <div className="flex items-center gap-2 flex-wrap">
                {stepFilter !== null && (
                  <span className="inline-flex items-center gap-1.5 px-2.5 py-1 text-xs rounded-full bg-neutral-900 text-white">
                    {stepFilter}
                    <button
                      onClick={() => {
                        setStepFilter(null);
                        setSelected(null);
                      }}
                      aria-label="Clear step filter"
                      className="leading-none"
                    >
                      ×
                    </button>
                  </span>
                )}
                {failureCount > 0 && (
                  <button
                    onClick={() => setFailuresOnly((v) => !v)}
                    aria-pressed={failuresOnly}
                    className={`${CHIP_BASE} ${
                      failuresOnly
                        ? "border-red-700 bg-red-700 text-white"
                        : "border-red-700 bg-white text-red-700 hover:bg-red-50"
                    }`}
                  >
                    Failures only ({failureCount})
                  </button>
                )}
                <label htmlFor="plan-search" className="sr-only">
                  Filter plan files
                </label>
                <input
                  id="plan-search"
                  type="text"
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  placeholder="filename or motion label…"
                  className="grow min-w-[12rem] max-w-xs px-2 py-1 text-xs border border-neutral-200 rounded-md"
                />
              </div>

              <PlanTable
                files={shown}
                selected={selected}
                viamClient={viamClient}
              />

              <p className="text-xs text-neutral-500 m-0">
                Showing {shown.length} of {all.length}
                {stepFilter !== null && (
                  <>
                    {" · filtered to "}
                    <strong className="text-neutral-900">{stepFilter}</strong>
                  </>
                )}
              </p>
            </>
          )}
        </div>
      )}
    </div>
  );
}
