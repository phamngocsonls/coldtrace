// Ops Console API client — thin wrapper over the Go ops-api (:8092).
// All live updates arrive via SSE; REST is used for initial loads + actions.
export const OPS_API = process.env.NEXT_PUBLIC_OPS_API ?? "http://localhost:8092";

export type Reading = {
  ts: string;
  tempC?: number;
  powerDraw?: number;
  compressorRunning?: boolean;
  doorOpen?: boolean;
  seq?: number;
};

export type DeviceRow = {
  deviceId: string;
  site: string;
  zone: string;
  product: string;
  valueAtRisk: number;
  setpoint: number;
  reported: Reading | null;
  lastSeen: string | null;
  status: "OK" | "AT_RISK" | "ALARM" | "STALE";
};

export type CommandRow = {
  commandId: string;
  deviceId: string;
  action: string;
  status: "QUEUED" | "SENT" | "ACKED" | "FAILED" | "TIMEOUT";
  latencyMs?: number;
  ackDetail?: string;
  createdAt: string;
};

export type EventRow = {
  id?: string;
  type: string;
  deviceId?: string;
  message: string;
  actor: string;
  severity?: string;
  at: string;
};

export type Incident = {
  // The API serialises MongoDB's primary key as `_id`. Declaring it as `id`
  // type-checked fine but was always undefined at runtime, so every incident
  // link resolved to /incidents/undefined.
  _id: string;
  deviceId: string;
  faultClass: string;
  confidence: number;
  etaAlarmMin: number;
  decision: "PROPOSED" | "AUTO" | "HOLD";
  reason: string;
  status: "OPEN" | "RESOLVED";
  similar?: { deviceId: string; faultClass: string; score: number }[];
  topFactors?: TopFactor[];
  traceId?: string;
  rca?: RCA;
  acknowledged?: boolean;
  remediatedAt?: string | null;
  remediatedBy?: string;
  openedAt: string;
};

export type RCA = {
  summary: string;
  rootCause: string;
  evidence: string[];
  actions: string[];
  model: string;
  fallback?: boolean;
};

export type Policy = {
  minConfidence: number;
  maxEtaAlarmMinutes: number;
  autoCooldownMinutes: number;
  fleetCircuitBreaker: number;
  killSwitch: boolean;
};

export type TopFactor = {
  feature: string;
  z: number;
  weight: number;
  contribution: number;
  direction: "raises" | "lowers";
};

export type ModelEval = {
  windows: number;
  accuracy: number;
  classes: string[];
  matrix: number[][];
  perClass: {
    class: string;
    precision: number;
    recall: number;
    f1: number;
    support: number;
  }[];
};

export type SiteInsight = {
  site: string;
  devices: number;
  ok: number;
  atRisk: number;
  alarm: number;
  openIncidents: number;
  valueAtRisk: number;
  maxSlopeCpm: number;
  avgExcursionMin: number;
  mttrMin: number;
};

// One summary bucket from telemetry_hourly / telemetry_daily — written by
// the rollup's $group + $merge (hot -> warm -> cold), read by the tier
// chart. avg/min/max come straight off the bucket.
export type TierPoint = {
  deviceId: string;
  bucket: string;
  bucketLocal: string;
  unit: string;
  avgTempC: number;
  minTempC: number;
  maxTempC: number;
  avgPowerDraw: number;
  compressorDuty: number;
  doorEvents: number;
  readings: number;
};

export type AlertRule = {
  ruleId: string;
  deviceId?: string;
  metric: string;
  op: "gt" | "lt" | "eq";
  threshold: number;
  severity: "info" | "warning" | "critical";
  enabled: boolean;
  createdBy: string;
  createdAt: string;
};

// ---- endpoints ----

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${OPS_API}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-Operator": "ops-console",
      ...init?.headers,
    },
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${res.status} ${text}`);
  }
  return res.json() as Promise<T>;
}

export const api = {
  fleet: () => req<DeviceRow[]>("/api/fleet"),
  device: (id: string) => req<Record<string, unknown>>(`/api/devices/${id}`),
  series: (id: string, windowMin: number) =>
    req<{ ts: string; temp: number | null }[]>(
      `/api/devices/${id}/series?window=${windowMin}`,
    ),
  // Warm/cold tier history: hourly rollups (telemetry_hourly, 7d retention)
  // or daily (telemetry_daily, 365d) — the downsampled summary that outlives
  // the 24h TTL on raw readings. bucketLocal is the bucket rendered on a
  // Vietnamese wall clock (server-side $dateToString, tz-aware).
  history: (id: string, tier: "hourly" | "daily" = "hourly", days = 7) =>
    req<TierPoint[]>(`/api/devices/${id}/history?tier=${tier}&days=${days}`),
  command: (id: string, action: string, args?: unknown) =>
    req<{ commandId: string }>(`/api/devices/${id}/commands`, {
      method: "POST",
      body: JSON.stringify({ action, args }),
    }),
  events: (limit = 50) => req<EventRow[]>(`/api/events?limit=${limit}`),
  incidents: (status?: string) =>
    req<Incident[]>(`/api/incidents${status ? `?status=${status}` : ""}`),
  modelEval: () => req<ModelEval>("/api/model/eval"),
  siteInsights: () => req<SiteInsight[]>("/api/insights/site"),
  rules: () => req<AlertRule[]>("/api/rules"),
  createRule: (r: Omit<AlertRule, "ruleId" | "enabled" | "createdBy" | "createdAt">) =>
    req<AlertRule>("/api/rules", { method: "POST", body: JSON.stringify(r) }),
  deleteRule: (ruleId: string) =>
    req<{ deleted: boolean }>(`/api/rules/${ruleId}`, { method: "DELETE" }),
  ackIncident: (id: string) =>
    req<{ acked: boolean }>(`/api/incidents/${id}/ack`, { method: "POST" }),
  resolveIncident: (id: string) =>
    req<{ resolved: boolean }>(`/api/incidents/${id}/resolve`, { method: "POST" }),
  feedbackIncident: (id: string, resolvedClass: string) =>
    req<{ learned: boolean }>(`/api/incidents/${id}/feedback`, {
      method: "POST",
      body: JSON.stringify({ resolvedClass }),
    }),
  policy: () => req<Policy>("/api/policy"),
  putPolicy: (p: Partial<Policy>) =>
    req<{ updated: boolean }>("/api/policy", {
      method: "PUT",
      body: JSON.stringify(p),
    }),
  // The API returns JSON null when no class is approved, not [].
  approvals: () => req<{ faultClass: string; approvedBy: string }[] | null>("/api/approvals"),
  approve: (faultClass: string) =>
    req<{ approved: boolean }>(`/api/approvals/${faultClass}`, { method: "PUT" }),
  revoke: (faultClass: string) =>
    req<{ revoked: boolean }>(`/api/approvals/${faultClass}`, { method: "DELETE" }),
  rca: (incidentId: string) => req<RCA>(`/api/ai/rca/${incidentId}`, { method: "POST" }),
  inject: (deviceId: string, fault: string) =>
    req<{ commandId: string }>("/api/demo/inject", {
      method: "POST",
      body: JSON.stringify({ deviceId, fault }),
    }),
  reset: () => req<{ commandId: string }>("/api/demo/reset", { method: "POST" }),
  chatStream: (
    messages: { role: string; content: string }[],
    onDelta: (t: string) => void,
  ): Promise<void> =>
    fetch(`${OPS_API}/api/ai/chat`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ messages }),
    }).then(async (res) => {
      if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error(`${res.status} ${text}`);
      }
      const reader = res.body?.getReader();
      if (!reader) return;
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const line = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          if (line.startsWith("data: ") && !line.includes("[DONE]")) {
            try {
              const j = JSON.parse(line.slice(6)) as { delta?: string; error?: string };
              if (j.delta) onDelta(j.delta);
              if (j.error) onDelta(`\n[error: ${j.error}]`);
            } catch {
              /* skip partial */
            }
          }
        }
      }
    }),
};

// SSE: subscribe to live change events from the ops-api hub.
export type ChangeEvent = {
  source: "device_state" | "ai_incidents" | "events";
  op: string;
  deviceId?: string;
  doc?: unknown;
  at: string;
};

export function openStream(onEvent: (e: ChangeEvent) => void): EventSource {
  const es = new EventSource(`${OPS_API}/api/stream`);
  es.onmessage = (m) => {
    try {
      onEvent(JSON.parse(m.data) as ChangeEvent);
    } catch {
      /* ignore malformed */
    }
  };
  return es;
}
