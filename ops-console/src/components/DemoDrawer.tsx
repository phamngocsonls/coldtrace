"use client";
// Demo Control drawer: drive the live demo from the UI (inject faults,
// reset fleet). Every judge-visible moment starts here.
import { useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { SlidersHorizontal, X } from "@phosphor-icons/react";
import { api } from "@/lib/api";

const FAULTS = [
  { id: "compressor_failure", label: "Compressor failure" },
  { id: "door_left_open", label: "Door left open" },
  { id: "sensor_drift", label: "Sensor drift" },
  { id: "power_outage", label: "Power outage" },
];

export function DemoDrawer() {
  const [open, setOpen] = useState(false);
  const [device, setDevice] = useState("FZ-003");
  const [fault, setFault] = useState(FAULTS[0].id);
  const [busy, setBusy] = useState(false);
  const qc = useQueryClient();
  const ref = useRef<HTMLDivElement>(null);

  const inject = async () => {
    setBusy(true);
    try {
      await api.inject(device, fault);
      qc.invalidateQueries();
    } finally {
      setBusy(false);
    }
  };
  const reset = async () => {
    setBusy(true);
    try {
      await api.reset();
      qc.invalidateQueries();
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="inline-flex cursor-pointer items-center gap-1.5 rounded bg-[#22c55e] px-3 py-1.5 text-sm font-semibold text-[#0f172a] transition-opacity hover:opacity-90"
      >
        <SlidersHorizontal weight="bold" className="h-4 w-4" aria-hidden /> Demo Control
      </button>
      {open && (
        <div
          className="fixed inset-0 z-30 bg-black/40"
          onClick={(e) => e.target === ref.current && setOpen(false)}
          ref={ref}
        >
          <aside
            role="dialog"
            aria-label="Demo control"
            className="glass absolute inset-y-0 right-0 flex w-80 flex-col gap-4 p-4"
          >
            <div className="flex items-center justify-between">
              <h2 className="font-semibold">Demo Control</h2>
              <button
                onClick={() => setOpen(false)}
                aria-label="Close"
                className="cursor-pointer rounded p-1 hover:bg-white/10"
              >
                <X className="h-4 w-4" aria-hidden />
              </button>
            </div>
            <label className="text-sm">
              <span className="mb-1 block text-[#94a3b8]">Device</span>
              <input
                value={device}
                onChange={(e) => setDevice(e.target.value.toUpperCase())}
                className="w-full rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono"
              />
            </label>
            <fieldset className="text-sm">
              <legend className="mb-1 text-[#94a3b8]">Fault to inject</legend>
              {FAULTS.map((f) => (
                <label key={f.id} className="flex cursor-pointer items-center gap-2 py-0.5">
                  <input
                    type="radio"
                    name="fault"
                    checked={fault === f.id}
                    onChange={() => setFault(f.id)}
                  />
                  {f.label}
                </label>
              ))}
            </fieldset>
            <button
              onClick={inject}
              disabled={busy}
              className="cursor-pointer rounded bg-[#ef4444] px-3 py-2 text-sm font-semibold text-white transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              {busy ? "injecting…" : "Inject fault"}
            </button>
            <button
              onClick={reset}
              disabled={busy}
              className="cursor-pointer rounded border border-[#475569] px-3 py-2 text-sm transition-colors hover:bg-white/10 disabled:opacity-50"
            >
              Reset fleet (clear all faults)
            </button>
            <p className="mt-auto text-[11px] leading-relaxed text-[#94a3b8]">
              Injections publish set_fault commands over MQTT; the AI agent and the
              command lifecycle react exactly as they would in production.
            </p>
          </aside>
        </div>
      )}
    </>
  );
}
