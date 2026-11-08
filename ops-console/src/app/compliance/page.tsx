"use client";
// Compliance: HACCP-style excursion summary + CSV export for auditors.
import { useQuery } from "@tanstack/react-query";
import { OPS_API } from "@/lib/api";

type Row = {
  deviceId: string;
  excursionMinutesAboveMinus14C: number;
  worstTempC: number;
  samplesAboveThreshold: number;
};

// parseExcursionCSV turns the audit export into rows. An empty body is NOT
// "no excursions": the export always carries a header, so anything else means
// the request failed or returned something unexpected. Returning null lets the
// page say "could not load" instead of claiming the fleet is compliant.
function parseExcursionCSV(text: string): Row[] | null {
  const lines = text.trim().split("\n").filter(Boolean);
  if (lines.length === 0 || !lines[0].startsWith("deviceId,")) return null;
  return lines.slice(1).map((l) => {
    const [deviceId, m, worst, n] = l.split(",");
    return {
      deviceId,
      excursionMinutesAboveMinus14C: Number(m),
      worstTempC: Number(worst),
      samplesAboveThreshold: Number(n),
    };
  });
}

export default function CompliancePage() {
  const rows = useQuery({
    queryKey: ["compliance"],
    queryFn: () =>
      fetch(`${OPS_API}/api/compliance/excursions.csv`)
        // fetch() only rejects on network errors; a 500 would otherwise be
        // parsed as CSV and render as "fully compliant".
        .then((r) => {
          if (!r.ok) throw new Error(`export failed: HTTP ${r.status}`);
          return r.text();
        })
        .then(parseExcursionCSV)
        .then((parsed) => {
          if (parsed === null) throw new Error("export returned an unexpected body");
          return parsed;
        }),
    refetchInterval: 30000,
  });

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">Compliance — cold-chain excursions (24h)</h1>
        <a
          href={`${OPS_API}/api/compliance/excursions.csv`}
          className="rounded border border-[#475569] px-3 py-1.5 text-sm hover:bg-white/10"
        >
          Export CSV
        </a>
      </div>
      <p className="max-w-2xl text-[13px] text-[#94a3b8]">
        Minutes each freezer spent above the -14°C at-risk threshold, computed by a
        MongoDB aggregation over the time-series collection. This is the audit pack a
        food-safety inspector (or HACCP auditor) needs — beyond what any APM dashboard shows.
      </p>
      <table className="w-full max-w-3xl font-mono text-[13px]">
        <thead className="bg-[#1b2336] text-left text-[#94a3b8]">
          <tr>
            <th className="px-3 py-2">device</th>
            <th className="px-3 py-2">excursion minutes</th>
            <th className="px-3 py-2">worst °C</th>
            <th className="px-3 py-2">samples</th>
          </tr>
        </thead>
        <tbody>
          {(rows.data ?? []).map((r) => (
            <tr key={r.deviceId} className="border-t border-[#475569]/30 odd:bg-white/[0.02]">
              <td className="px-3 py-1.5">{r.deviceId}</td>
              <td className={`px-3 py-1.5 ${r.excursionMinutesAboveMinus14C > 0 ? "text-[#f59e0b]" : "text-[#22c55e]"}`}>
                {r.excursionMinutesAboveMinus14C.toFixed(1)}
              </td>
              <td className="px-3 py-1.5">{r.worstTempC.toFixed(1)}</td>
              <td className="px-3 py-1.5 text-[#94a3b8]">{r.samplesAboveThreshold}</td>
            </tr>
          ))}
          {rows.isLoading && (
            <tr>
              <td colSpan={4} className="px-3 py-6 text-center text-[#94a3b8]">
                loading the audit export…
              </td>
            </tr>
          )}
          {rows.isError && (
            <tr>
              <td colSpan={4} className="px-3 py-6 text-center text-[#ef4444]">
                could not load the audit export — compliance unknown, not
                verified: {(rows.error as Error).message}
              </td>
            </tr>
          )}
          {rows.data && rows.data.length === 0 && (
            <tr>
              <td colSpan={4} className="px-3 py-6 text-center text-[#22c55e]">
                zero excursions in the last 24h — fully compliant
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
