// The chart groups of the app, host and incident pages.
import { fetchSeries, seriesRef } from "./api.js";
import { drawChart, palette } from "./charts.js";

const range = (container) => [Number(container.dataset.from), Number(container.dataset.to)];

const chartIn = (container, name) => container.querySelector(`[data-chart="${name}"]`);

// settle runs the drawing jobs side by side and reports each failure on its own
// chart, so one failing query does not blank the others.
async function settle(jobs) {
  const results = await Promise.allSettled(jobs.map(({ run }) => run()));
  results.forEach((result, index) => {
    if (result.status === "fulfilled") return;
    console.error("chart:", result.reason);
    const target = jobs[index].target;
    target.replaceChildren(Object.assign(document.createElement("p"), {
      className: "empty", textContent: "This chart could not be loaded.",
    }));
  });
}

// draw loads series and draws them in target; decorate shapes each entry.
const draw = (target, refs, [from, to], options = {}, decorate = (entry) => entry) => ({
  target,
  run: async () => {
    const series = (await fetchSeries(refs, from, to)).map(decorate);
    drawChart(target, series, series[0]?.unit || "count", { range: [from, to], ...options });
  },
});

// slotsOf lists the slots of an app page: [{id, key}].
const slotsOf = (container) => [...container.querySelectorAll("[data-slot-id]")]
  .map((element) => ({ id: element.dataset.slotId, key: element.dataset.slotKey }));

export async function drawAppCharts() {
  const container = document.querySelector(".charts[data-app]");
  if (!container) return;
  const app = container.dataset.app;
  const span = range(container);
  const colors = palette();
  const slots = slotsOf(container);
  const appRefs = (metrics) => metrics.map((metric) => seriesRef("app", app, metric));
  const slotRefs = (metric) => slots.map((slot) => seriesRef("slot", slot.id, metric));
  const named = (entry) => ({ ...entry, label: entry.metric });
  const jobs = [
    draw(chartIn(container, "requests"), appRefs(["2xx", "3xx", "4xx", "5xx"]), span,
      { fill: true }, (entry, index) => ({ ...named(entry), color: colors.status[index] })),
    draw(chartIn(container, "errors"), appRefs(["error_rate"]), span, {}, (entry) => ({ ...entry, label: "5xx rate" })),
    draw(chartIn(container, "latency"), appRefs(["p50", "p95", "p99"]), span,
      {}, (entry, index) => ({ ...named(entry), color: colors.series[index] })),
  ];
  if (slots.length > 0) {
    jobs.push(
      draw(chartIn(container, "cpu"), slotRefs("cpu"), span),
      draw(chartIn(container, "memory"), slotRefs("memory"), span),
      {
        target: chartIn(container, "network"),
        run: async () => {
          const [received, sent] = await Promise.all([fetchSeries(slotRefs("net_rx"), ...span), fetchSeries(slotRefs("net_tx"), ...span)]);
          const series = [
            ...received.map((entry) => ({ ...entry, label: `${entry.label} in` })),
            ...sent.map((entry) => ({ ...entry, label: `${entry.label} out`, dash: true })),
          ];
          drawChart(chartIn(container, "network"), series, "bytes", { range: span });
        },
      });
  }
  await settle(jobs);
}

export async function drawHostCharts() {
  const container = document.querySelector(".charts[data-host]");
  if (!container) return;
  const span = range(container);
  const host = (metric) => [seriesRef("host", "", metric)];
  const jobs = [
    draw(chartIn(container, "host-cpu"), host("cpu"), span, { ceiling: 100 }),
    draw(chartIn(container, "host-load"), host("load1"), span),
    draw(chartIn(container, "host-memory"), host("memory"), span),
    draw(chartIn(container, "host-swap"), host("swap"), span),
  ];
  for (const element of container.querySelectorAll("[data-chart^='disk-']")) {
    jobs.push(draw(element, [seriesRef("disk", element.dataset.chart.slice("disk-".length), "used")], span, { ceiling: 100 }));
  }
  await settle(jobs);
}

export async function drawIncidentChart() {
  const container = document.querySelector(".charts[data-series]");
  if (!container) return;
  await settle([draw(chartIn(container, "incident"), [container.dataset.series], range(container))]);
}

// drawSparklines turns the data-values of .spark elements into tiny line charts.
export function drawSparklines() {
  const NS = "http://www.w3.org/2000/svg";
  for (const element of document.querySelectorAll(".spark[data-values]")) {
    const values = element.dataset.values.split(",").filter((value) => value !== "").map(Number);
    if (!values.length) continue;
    const width = 64;
    const height = 16;
    const peak = Math.max(1, ...values);
    const points = values.map((value, index) => `${(index / Math.max(1, values.length - 1)) * width},${height - (value / peak) * (height - 2) - 1}`);
    const svg = document.createElementNS(NS, "svg");
    svg.setAttribute("width", String(width));
    svg.setAttribute("height", String(height));
    svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
    svg.setAttribute("aria-hidden", "true");
    const line = document.createElementNS(NS, "polyline");
    line.setAttribute("fill", "none");
    line.setAttribute("stroke", "currentColor");
    line.setAttribute("stroke-width", "1");
    line.setAttribute("points", points.join(" "));
    svg.append(line);
    element.replaceChildren(svg);
  }
}
