// Mean drinks between failures (MDBF): a cycle-counted MTBF, where the cycle
// is a drink. Counting drinks rather than hours keeps a quiet week from
// looking reliable just because the machine brewed less.
//
// A failure is any order that did not complete, operator cancels included.
// Failures are grouped into incidents — a run of failures with no successful
// drink between them — so an operator retrying the same broken step counts
// once, and an incident only closes once a drink actually succeeds.

export const RELIABILITY_WINDOW_DAYS = 30;
// History fetched beyond the window, so an incident straddling the window
// start is recognised as one, and the streak since the last incident can
// reach back before the window.
export const RELIABILITY_HISTORY_DAYS = 90;

export interface OrderOutcome {
  robotId: string;
  at: Date;
  ok: boolean;
  failedStep: string;
}

export interface Incident {
  start: Date;
  /** Failed attempts in the incident, the first included. */
  failures: number;
  /** Step the incident's first failure stopped at. */
  failedStep: string;
  /** Successful drinks between the previous incident and this one. */
  drinksBefore: number;
  /** True when no earlier incident is in the fetched history. */
  drinksBeforeIsLowerBound: boolean;
}

export interface StepCount {
  step: string;
  count: number;
}

export interface MachineReliability {
  robotId: string;
  /** Successful drinks in the window. */
  drinks: number;
  /** Incidents that started in the window, oldest first. */
  incidents: Incident[];
  /** Failed attempts across those incidents. */
  failures: number;
  /** drinks / incidents; null when the window has no incident. */
  mdbf: number | null;
  drinksSinceLastIncident: number;
  /** True when no incident is in the fetched history at all. */
  sinceLastIsLowerBound: boolean;
  /** First-failure steps of the window's incidents, most frequent first. */
  topSteps: StepCount[];
}

export const UNKNOWN_STEP = "unknown step";

/** outcomes must belong to one machine and be sorted oldest first. */
export function computeReliability(
  robotId: string,
  outcomes: OrderOutcome[],
  windowStart: Date
): MachineReliability {
  const incidents: Incident[] = [];
  let drinks = 0;
  let sinceLast = 0;
  let sawIncident = false;
  let current: Incident | null = null;

  for (const o of outcomes) {
    const inWindow = o.at.getTime() >= windowStart.getTime();
    if (o.ok) {
      if (inWindow) drinks++;
      sinceLast++;
      current = null;
      continue;
    }
    if (current) {
      current.failures++;
      continue;
    }
    current = {
      start: o.at,
      failures: 1,
      failedStep: o.failedStep || UNKNOWN_STEP,
      drinksBefore: sinceLast,
      drinksBeforeIsLowerBound: !sawIncident,
    };
    sawIncident = true;
    sinceLast = 0;
    if (inWindow) incidents.push(current);
  }

  const stepCounts = new Map<string, number>();
  for (const i of incidents) {
    stepCounts.set(i.failedStep, (stepCounts.get(i.failedStep) ?? 0) + 1);
  }
  const topSteps = [...stepCounts.entries()]
    .map(([step, count]) => ({ step, count }))
    .sort((a, b) => b.count - a.count || a.step.localeCompare(b.step));

  return {
    robotId,
    drinks,
    incidents,
    failures: incidents.reduce((n, i) => n + i.failures, 0),
    mdbf: incidents.length > 0 ? drinks / incidents.length : null,
    drinksSinceLastIncident: sinceLast,
    sinceLastIsLowerBound: !sawIncident,
    topSteps,
  };
}

/** Groups outcomes by machine; each group keeps the input order. */
export function groupByRobot(
  outcomes: OrderOutcome[]
): Map<string, OrderOutcome[]> {
  const byRobot = new Map<string, OrderOutcome[]>();
  for (const o of outcomes) {
    const list = byRobot.get(o.robotId);
    if (list) list.push(o);
    else byRobot.set(o.robotId, [o]);
  }
  return byRobot;
}
