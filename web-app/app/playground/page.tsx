"use client";

import { useState } from "react";

const COWORKER_NAMES = [
  "Kristina", "Nick", "John", "Sean", "Olivia", "Steven", "Sebastian",
  "Sagie", "Alex", "Michael", "Grant", "Clint", "Robin", "Bill", "Martha",
  "Ian", "Lillian", "Devin", "Shannon", "Kate", "Matthew", "Nonofo", "Brad",
  "Adrienne", "Ana", "Sagar", "Sarah", "Josh", "Kristina", "Milo", "Kevin",
  "Aiden", "Luke", "Eshe", "Messias", "Nicole", "Robert", "Cheuk", "Cody",
  "Suraj", "Ulas", "Vaisali", "Bashar", "Lie", "Joseph", "Evan", "Gloria",
  "Nat", "Steve", "Dan", "Christopher", "Dhruv", "Vijay", "Julie",
  "Nicholas", "Khari", "Petr", "Naveed", "Owen", "Travis", "Eugene",
  "Vignesh", "Andrew", "Jalen", "Allison",
];

interface Result {
  original: string;
  misspelled?: string;
  pronunciation?: string;
  chaos?: "low" | "medium" | "high";
  strategy?: string;
  loading: boolean;
  error?: string;
}

export default function Home() {
  const [results, setResults] = useState<Result[]>([]);
  const [customName, setCustomName] = useState("");
  const [running, setRunning] = useState(false);
  const [chaosLevel, setChaosLevel] = useState<"low" | "medium" | "high">("medium");

  async function misspellOne(name: string): Promise<Result> {
    try {
      const res = await fetch("/api/misspell", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name, chaos: chaosLevel }),
      });
      const data = await res.json();
      if (data.error) return { original: name, loading: false, error: data.error };
      return { original: name, misspelled: data.misspelled, pronunciation: data.pronunciation, chaos: data.chaos, strategy: data.strategy, loading: false };
    } catch (e) {
      return { original: name, loading: false, error: String(e) };
    }
  }

  function pickRandom(n: number) {
    const shuffled = [...COWORKER_NAMES].sort(() => Math.random() - 0.5);
    return shuffled.slice(0, n);
  }

  async function runRandom() {
    setRunning(true);
    const names = pickRandom(10);
    setResults(names.map((name) => ({ original: name, loading: true })));

    // Run in batches of 5
    const BATCH = 5;
    for (let i = 0; i < names.length; i += BATCH) {
      const batch = names.slice(i, i + BATCH);
      const batchResults = await Promise.all(batch.map(misspellOne));

      setResults((prev) => {
        const next = [...prev];
        batchResults.forEach((r, j) => {
          next[i + j] = r;
        });
        return next;
      });
    }
    setRunning(false);
  }

  async function runSingle() {
    if (!customName.trim()) return;
    const placeholder: Result = { original: customName.trim(), loading: true };
    setResults((prev) => [placeholder, ...prev]);
    const result = await misspellOne(customName.trim());
    setResults((prev) => [result, ...prev.slice(1)]);
    setCustomName("");
  }

  return (
    <main style={{ maxWidth: 800, margin: "0 auto", padding: "2rem", fontFamily: "system-ui, sans-serif" }}>
      <h1 style={{ fontSize: "1.5rem", marginBottom: "0.25rem" }}>☕ Barista Misspell Playground</h1>
      <p style={{ color: "#666", marginBottom: "2rem" }}>
        Test how the barista misspells your coworkers&apos; names
      </p>

      <div style={{ display: "flex", gap: "0.5rem", marginBottom: "0.75rem", alignItems: "center" }}>
        <span style={{ fontSize: "0.85rem", color: "#666" }}>Chaos:</span>
        {(["low", "medium", "high"] as const).map((level) => (
          <button
            key={level}
            onClick={() => setChaosLevel(level)}
            style={{
              padding: "4px 12px",
              borderRadius: 99,
              border: "none",
              fontSize: "0.8rem",
              cursor: "pointer",
              background: chaosLevel === level
                ? (level === "high" ? "#fee2e2" : level === "medium" ? "#fef3c7" : "#ecfdf5")
                : "#f3f3f3",
              color: chaosLevel === level
                ? (level === "high" ? "#991b1b" : level === "medium" ? "#92400e" : "#065f46")
                : "#999",
              fontWeight: chaosLevel === level ? 600 : 400,
            }}
          >
            {level}
          </button>
        ))}
      </div>

      <div style={{ display: "flex", gap: "0.5rem", marginBottom: "1.5rem" }}>
        <input
          type="text"
          value={customName}
          onChange={(e) => setCustomName(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && runSingle()}
          placeholder="Try a name..."
          style={{
            padding: "0.5rem 0.75rem",
            border: "1px solid #ccc",
            borderRadius: 6,
            fontSize: "1rem",
            flex: 1,
          }}
        />
        <button onClick={runSingle} style={btnStyle}>
          Misspell
        </button>
        <button onClick={runRandom} disabled={running} style={{ ...btnStyle, background: running ? "#999" : "#8B4513" }}>
          {running ? `Running...` : `Random 10`}
        </button>
      </div>

      {results.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ borderBottom: "2px solid #333", textAlign: "left" }}>
              <th style={thStyle}>Original</th>
              <th style={thStyle}>On the cup</th>
              <th style={thStyle}>Called out as</th>
              <th style={thStyle}>Chaos</th>
              <th style={{ ...thStyle, color: "#888", fontWeight: "normal", fontSize: "0.85rem" }}>Strategy</th>
            </tr>
          </thead>
          <tbody>
            {results.map((r, i) => (
              <tr key={`${r.original}-${i}`} style={{ borderBottom: "1px solid #eee" }}>
                <td style={tdStyle}>{r.original}</td>
                <td style={tdStyle}>
                  {r.loading ? (
                    <span style={{ color: "#999" }}>...</span>
                  ) : r.error ? (
                    <span style={{ color: "red", fontSize: "0.8rem" }}>Error</span>
                  ) : (
                    <span style={{ fontFamily: "'Just Me Again Down Here', cursive", fontSize: "1.6rem" }}>
                      {r.misspelled}
                    </span>
                  )}
                </td>
                <td style={{ ...tdStyle, color: "#666", fontStyle: "italic", fontSize: "0.9rem" }}>
                  {r.loading ? "" : r.pronunciation}
                </td>
                <td style={tdStyle}>
                  {!r.loading && r.chaos && (
                    <span style={{
                      fontSize: "0.75rem",
                      padding: "2px 8px",
                      borderRadius: 99,
                      background: r.chaos === "high" ? "#fee2e2" : r.chaos === "medium" ? "#fef3c7" : "#ecfdf5",
                      color: r.chaos === "high" ? "#991b1b" : r.chaos === "medium" ? "#92400e" : "#065f46",
                    }}>
                      {r.chaos}
                    </span>
                  )}
                </td>
                <td style={{ ...tdStyle, color: "#888", fontSize: "0.85rem" }}>{r.strategy}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}

const btnStyle: React.CSSProperties = {
  padding: "0.5rem 1rem",
  background: "#8B4513",
  color: "white",
  border: "none",
  borderRadius: 6,
  fontSize: "0.9rem",
  cursor: "pointer",
};

const thStyle: React.CSSProperties = { padding: "0.5rem 0.75rem" };
const tdStyle: React.CSSProperties = { padding: "0.5rem 0.75rem" };