"use client";
// AI Control: autonomy approvals, guardrail policy editor, kill switch.
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ModelEval, type Policy } from "@/lib/api";

const CLASSES = ["compressor_failure", "door_left_open", "sensor_drift", "power_outage"];

export default function AiControlPage() {
  const qc = useQueryClient();
  const policy = useQuery({ queryKey: ["policy"], queryFn: api.policy });
  const approvals = useQuery({ queryKey: ["approvals"], queryFn: api.approvals });
  const [draft, setDraft] = useState<Policy | null>(null);

  useEffect(() => {
    if (policy.data && !draft) setDraft(policy.data);
  }, [policy.data, draft]);

  const save = async () => {
    if (!draft) return;
    await api.putPolicy(draft);
    qc.invalidateQueries({ queryKey: ["policy"] });
  };

  const approved = new Set((approvals.data ?? []).map((a) => a.faultClass));
  const evalQ = useQuery({ queryKey: ["modelEval"], queryFn: api.modelEval, staleTime: 60_000 });

  return (
    <div className="grid max-w-4xl gap-4 lg:grid-cols-2">
      <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
        <h2 className="mb-3 text-sm font-semibold">Autonomy approvals</h2>
        <p className="mb-3 text-[12px] text-[#94a3b8]">
          The AI proposes on unknown classes; an approved class lets the agent
          remediate autonomously — inside the guardrails.
        </p>
        <ul className="space-y-2">
          {CLASSES.map((c) => {
            const on = approved.has(c);
            return (
              <li key={c} className="flex items-center justify-between rounded border border-[#475569]/40 px-3 py-2">
                <span className="font-mono text-[12px]">{c}</span>
                <button
                  onClick={async () => {
                    if (on) await api.revoke(c);
                    else await api.approve(c);
                    qc.invalidateQueries({ queryKey: ["approvals"] });
                  }}
                  aria-pressed={on}
                  className={`cursor-pointer rounded px-2.5 py-1 text-[11px] font-semibold transition-colors ${
                    on ? "bg-[#22c55e] text-[#0f172a]" : "border border-[#475569] text-[#94a3b8] hover:bg-white/10"
                  }`}
                >
                  {on ? "AUTO ENABLED" : "PROPOSE ONLY"}
                </button>
              </li>
            );
          })}
        </ul>
      </section>

      <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
        <h2 className="mb-3 text-sm font-semibold">Guardrail policy</h2>
        {draft && (
          <div className="space-y-3 text-[13px]">
            <label className="block">
              <span className="text-[#94a3b8]">min confidence: {draft.minConfidence.toFixed(2)}</span>
              <input
                type="range" min={0.5} max={1} step={0.01}
                value={draft.minConfidence}
                onChange={(e) => setDraft({ ...draft, minConfidence: Number(e.target.value) })}
                className="w-full accent-[#22c55e]"
              />
            </label>
            <label className="block">
              <span className="text-[#94a3b8]">max ETA to alarm (min): {draft.maxEtaAlarmMinutes}</span>
              <input
                type="range" min={2} max={30} step={1}
                value={draft.maxEtaAlarmMinutes}
                onChange={(e) => setDraft({ ...draft, maxEtaAlarmMinutes: Number(e.target.value) })}
                className="w-full accent-[#22c55e]"
              />
            </label>
            <label className="block">
              <span className="text-[#94a3b8]">fleet circuit breaker (devices in ALARM): {draft.fleetCircuitBreaker}</span>
              <input
                type="range" min={1} max={6} step={1}
                value={draft.fleetCircuitBreaker}
                onChange={(e) => setDraft({ ...draft, fleetCircuitBreaker: Number(e.target.value) })}
                className="w-full accent-[#22c55e]"
              />
            </label>
            <label className="flex items-center justify-between rounded border border-[#ef4444]/50 bg-[#ef4444]/10 px-3 py-2">
              <span className="font-semibold text-[#ef4444]">Kill switch (suspend all auto-actions)</span>
              <input
                type="checkbox"
                checked={draft.killSwitch}
                onChange={(e) => setDraft({ ...draft, killSwitch: e.target.checked })}
                className="h-4 w-4 accent-[#ef4444]"
              />
            </label>
            <button
              onClick={save}
              className="w-full cursor-pointer rounded bg-[#22c55e] py-2 text-sm font-semibold text-[#0f172a] transition-opacity hover:opacity-90"
            >
              Save policy
            </button>
            <p className="text-[11px] text-[#94a3b8]">
              Stored in MongoDB (ai_policy) — the agent re-reads it on every
              decision; changes take effect within one evaluation cycle.
            </p>
          </div>
        )}
      </section>

      <ModelHealthCard eval={evalQ.data} isLoading={evalQ.isLoading} />
    </div>
  );
}

function ModelHealthCard({ eval: e, isLoading }: { eval?: ModelEval; isLoading: boolean }) {
  return (
    <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4 lg:col-span-2">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-semibold">Model Health</h2>
        {e && (
          <span className="font-mono text-[11px] text-[#94a3b8]">
            {e.windows} held-out windows · macro accuracy{" "}
            <span className={e.accuracy >= 0.9 ? "text-[#22c55e]" : "text-[#f59e0b]"}>
              {(e.accuracy * 100).toFixed(1)}%
            </span>
          </span>
        )}
      </div>
      {isLoading && <p className="text-[12px] text-[#94a3b8]">evaluating classifier…</p>}
      {e && (
        <div className="grid gap-4 lg:grid-cols-[1fr_auto]">
          <table className="text-[12px]">
            <thead>
              <tr className="text-left text-[#94a3b8]">
                <th className="pr-4 font-medium">class</th>
                <th className="pr-4 font-medium">precision</th>
                <th className="pr-4 font-medium">recall</th>
                <th className="pr-4 font-medium">f1</th>
                <th className="font-medium">support</th>
              </tr>
            </thead>
            <tbody className="font-mono">
              {e.perClass.map((c) => (
                <tr key={c.class}>
                  <td className="pr-4">{c.class}</td>
                  <td className="pr-4">{(c.precision * 100).toFixed(0)}%</td>
                  <td className="pr-4">{(c.recall * 100).toFixed(0)}%</td>
                  <td className="pr-4">{(c.f1 * 100).toFixed(0)}%</td>
                  <td>{c.support}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div>
            <div className="mb-1 text-[11px] text-[#94a3b8]">
              confusion matrix — rows true, cols predicted
            </div>
            <table className="font-mono text-[11px]">
              <thead>
                <tr>
                  <th className="px-2 text-left text-[#94a3b8]">true＼pred</th>
                  {e.classes.map((c) => (
                    <th key={c} className="px-2 text-[#94a3b8]">
                      {c.replace("_", " ").slice(0, 9)}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {e.matrix.map((row, y) => (
                  <tr key={y}>
                    <td className="pr-2 text-[#94a3b8]">{e.classes[y].replace("_", " ")}</td>
                    {row.map((v, x) => (
                      <td
                        key={x}
                        className={`px-2 text-center ${
                          y === x ? "font-semibold text-[#22c55e]" : v ? "text-[#ef4444]" : "text-[#475569]"
                        }`}
                      >
                        {v}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      <p className="mt-2 text-[11px] text-[#94a3b8]">
        Held-out evaluation: fresh synthetic windows per class (distinct RNG seed from
        warm-start), classified by a fresh warm-started model — offline proxy for
        real-world accuracy. Rebuild the report from the evalmodel CLI.
      </p>
    </section>
  );
}
