// The issues a customer can report about an order. Mirrors IssueKinds in
// coffee/order/issue.go, which refuses a key it does not know and a severity
// that disagrees with its own.

export type Severity = "minor" | "medium" | "critical";

export const SEVERITIES: Severity[] = ["minor", "medium", "critical"];

export const OTHER_ISSUE = "other";

export interface IssueKind {
  key: string;
  label: string;
  severity: Severity;
}

export const ISSUE_KINDS: IssueKind[] = [
  { key: "small_spill", label: "Small spill", severity: "minor" },
  { key: "large_spill", label: "Large spill", severity: "critical" },
  { key: "surprising_movements", label: "Surprising movements", severity: "minor" },
  { key: "light_collision", label: "Light collision", severity: "medium" },
  { key: "bad_collision", label: "Bad collision", severity: "critical" },
  { key: "empty_cup", label: "Empty cup", severity: "medium" },
  { key: "dropped_cup", label: "Dropped cup/glass", severity: "critical" },
];

/** "small_spill" -> "Small spill"; "other" and unknown keys -> "Other". */
export function issueLabel(key: string): string {
  return ISSUE_KINDS.find((k) => k.key === key)?.label ?? "Other";
}

export function severityLabel(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** Tailwind classes for a severity chip, shared by the kiosk and dashboard. */
export function severityChipClass(s: string): string {
  switch (s) {
    case "critical":
      return "bg-red-100 text-red-700 border-red-200";
    case "medium":
      return "bg-amber-100 text-amber-800 border-amber-200";
    default:
      return "bg-neutral-100 text-neutral-700 border-neutral-200";
  }
}
