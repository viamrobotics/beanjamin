"use client";

import { useEffect, useState } from "react";
import * as VIAM from "@viamrobotics/sdk";
import Cookies from "js-cookie";

interface Machine {
  id: string;
  name: string;
  locationName: string;
  online: boolean;
  lastOnline: Date | null;
}

async function listMachines(client: VIAM.ViamClient): Promise<Machine[]> {
  const summaries = await client.appClient.listMachineSummaries("e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb",["e6103e56-ad3a-42c6-ae5b-7cc9c310331d"], ["oeq47g5p1m"]);
  const machines: Machine[] = [];
  for (const location of summaries) {
    for (const m of location.machineSummaries) {
      const mainPart = m.partSummaries.find((p) => p.isMainPart) ?? m.partSummaries[0];
      machines.push({
        id: m.machineId,
        name: m.machineName,
        locationName: location.locationName,
        online: mainPart?.onlineState === VIAM.appApi.OnlineState.ONLINE,
        lastOnline: mainPart?.lastOnline?.toDate() ?? null,
      });
    }
  }
  machines.sort((a, b) => Number(b.online) - Number(a.online));
  return machines;
}

export default function Home() {
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const [machines, setMachines] = useState<Machine[]>([]);

  useEffect(() => {
    (async () => {
      try {
        const userTokenRaw = Cookies.get("userToken");
        if (!userTokenRaw) {
          throw new Error("No userToken cookie found");
        }
        const { access_token } = JSON.parse(userTokenRaw);

        const client = await VIAM.createViamClient({
          credentials: {
            type: "access-token",
            payload: access_token,
          },
        });

        const found = await listMachines(client);
        setMachines(found);
        setStatus("ready");
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setStatus("error");
      }
    })();
  }, []);

  if (status === "loading") return <p>Loading machines…</p>;
  if (status === "error") return <p>Error: {error}</p>;
  if (machines.length === 0) return <p>No machines found.</p>;

  return (
    <div style={{ padding: "1rem", fontFamily: "monospace" }}>
      <h1>Machines ({machines.length})</h1>
      <ul>
        {machines.map((m) => (
          <li key={m.id}>
            <span
              title={m.online ? "Online" : "Offline"}
              style={{
                display: "inline-block",
                width: "0.6em",
                height: "0.6em",
                borderRadius: "50%",
                backgroundColor: m.online ? "#22c55e" : "#9ca3af",
                marginRight: "0.5em",
                verticalAlign: "middle",
              }}
            />
            <strong>{m.name}</strong> — {m.locationName}{" "}
            <span style={{ color: "#888" }}>({m.id})</span>
            {m.lastOnline && (
              <span style={{ color: "#888", marginLeft: "0.5em" }}>
                · last online {m.lastOnline.toLocaleString()}
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}
