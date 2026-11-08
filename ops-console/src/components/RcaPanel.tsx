"use client";
// RCA panel: "Explain" asks the AI agent for a grounded root-cause
// analysis; suggested actions are executed ONLY by human click (the agent
// never acts on its own).
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api, type RCA } from "@/lib/api";
import { Sparkle } from "@phosphor-icons/react";

export function RcaPanel({ incidentID, initial }: { incidentID: string; initial: RCA | null }) {
  const [rca, setRca] = useState<RCA | null>(initial);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const qc = useQueryClient();

  const explain = async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await api.rca(incidentID);
      setRca(r);
      qc.invalidateQueries({ queryKey: ["incidents"] });
    } catch (e) {
      setErr(e instanceof Error ? e.message : "explain failed");
    } finally {
      setLoading(false);
    }
  };

  const execute = async (action: string) => {
    // extract the deviceId from the incident context via the incident id
    const inc = await fetch(`${process.env.NEXT_PUBLIC_OPS_API ?? "http://localhost:8092"}/api/incidents`)
      .then((r) => r.json())
      .then((list: { _id: string; deviceId: string }[]) =>
        list.find((x) => x._id === incidentID),
      );
    if (!inc) return;
    if (action === "reboot") await api.command(inc.deviceId, "reboot");
    else if (action === "defrost") await api.command(inc.deviceId, "defrost");
    else if (action === "compressor_restart") await api.command(inc.deviceId, "compressor_restart");
    qc.invalidateQueries();
  };

  return (
    <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold">AI agent root cause</h2>
        <button
          onClick={explain}
          disabled={loading}
          className="inline-flex cursor-pointer items-center gap-1.5 rounded bg-[#38bdf8]/20 px-3 py-1 text-[12px] font-medium text-[#38bdf8] transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          <Sparkle weight="fill" className="h-3.5 w-3.5" aria-hidden />
          {loading ? "analyzing…" : rca ? "Re-explain" : "Explain"}
        </button>
      </div>

      {!rca && !err && (
        <p className="text-[12px] text-[#94a3b8]">
          Ask the AI agent for a root-cause analysis grounded in MongoDB facts
          (twin, stats, command history, similar incidents).
        </p>
      )}
      {err && <p className="text-[12px] text-[#ef4444]">{err}</p>}

      {rca && (
        <div className="space-y-2 text-[13px]">
          <p>{rca.summary}</p>
          <p className="text-[#94a3b8]">{rca.rootCause}</p>
          {rca.evidence?.length > 0 && (
            <ul className="list-inside list-disc font-mono text-[11px] text-[#94a3b8]">
              {rca.evidence.map((e, k) => (
                <li key={k}>{e}</li>
              ))}
            </ul>
          )}
          {rca.fallback && (
            <p className="rounded bg-[#f59e0b]/10 px-2 py-1 text-[11px] text-[#f59e0b]">
              deterministic fallback (LLM unavailable) — data is still real
            </p>
          )}
          {rca.actions?.length > 0 && rca.actions[0] !== "none" && (
            <div className="flex flex-wrap items-center gap-2 pt-1">
              <span className="text-[11px] uppercase tracking-wide text-[#94a3b8]">
                suggested actions (human executes):
              </span>
              {rca.actions.map((a) => (
                <button
                  key={a}
                  onClick={() => execute(a)}
                  className="cursor-pointer rounded border border-[#22c55e]/50 px-2.5 py-1 font-mono text-[11px] text-[#22c55e] transition-colors hover:bg-[#22c55e]/10"
                >
                  execute {a}
                </button>
              ))}
            </div>
          )}
          <p className="text-[10px] text-[#94a3b8]">model: {rca.model}</p>
        </div>
      )}
    </section>
  );
}
