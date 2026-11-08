// Datadog deep links for the Ops Console.
//
// The operator-facing site is app.<DD_SITE> (ap1.datadoghq.com →
// app.ap1.datadoghq.com). The trace page takes the W3C trace ID we emit on
// every incident, so the console can jump straight to the waterfall.
//
// NEXT_PUBLIC_DD_SITE is inlined at build time (Next.js public env).
const DD_SITE = process.env.NEXT_PUBLIC_DD_SITE || "datadoghq.com";

/** Datadog web app origin, e.g. https://app.ap1.datadoghq.com */
export function datadogAppURL(): string {
  return `https://app.${DD_SITE}`;
}

/** Trace waterfall for a W3C trace ID, or the trace search page when absent. */
export function datadogTraceURL(traceId?: string): string {
  const base = `${datadogAppURL()}/apm/trace`;
  return traceId ? `${base}/${traceId}` : base;
}
