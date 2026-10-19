// Raedar's charts and live logs. uPlot is loaded as a global by its script.
const colors = () => {
  const style = getComputedStyle(document.documentElement);
  const value = (name) => style.getPropertyValue(name).trim();
  return {
    text: value("--text"), muted: value("--muted"), grid: value("--grid"),
    colors: [value("--series-2xx"), value("--series-3xx"), value("--series-4xx"), value("--series-5xx"),
      value("--series-a"), value("--series-b"), value("--series-c")],
  };
};

const number = (value) => {
  if (value >= 1e9) return (value / 1e9).toFixed(1) + " GB";
  if (value >= 1e6) return (value / 1e6).toFixed(1) + " MB";
  if (value >= 1e3) return (value / 1e3).toFixed(1) + "k";
  return String(Math.round(value));
};

async function fetchSeries(keys, from, to) {
  const params = new URLSearchParams({ keys: keys.join(","), from: String(from), to: String(to) });
  const response = await fetch("/api/series?" + params);
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

function drawChart(target, series, unit, options = {}) {
  const { text, muted } = colors();
  const labels = ["time", ...series.map((entry, index) => entry.label || `series ${index + 1}`)];
  const data = [series[0]?.points.map((point) => point[0]) || []];
  for (const entry of series) {
    data.push(entry.points.map((point) => point[1]));
  }
  const chart = new uPlot({
    width: target.clientWidth || 600,
    height: options.height || 180,
    cursor: { sync: { key: options.syncKey || "raedar" }, points: { size: 6 } },
    scales: { x: { time: true } },
    axes: [
      { stroke: muted, grid: { stroke: text ? text + "11" : undefined }, ticks: { stroke: text ? text + "22" : undefined } },
      { stroke: muted, grid: { stroke: text ? text + "11" : undefined }, ticks: { stroke: text ? text + "22" : undefined },
        values: (self, ticks) => ticks.map((tick) => unit === "bytes" ? number(tick) : unit === "percent" ? tick + "%" : number(tick)) },
    ],
    series: [{}, ...series.map((entry) => ({
      label: entry.label,
      stroke: entry.color || colors().colors[0],
      width: 1.5,
      fill: options.fill ? (entry.color || colors().colors[0]) + "33" : undefined,
      dash: entry.dash ? [4, 4] : undefined,
      points: { show: false },
    }))],
    legend: { show: series.length > 1 },
  }, data, target);
  const resize = () => chart.setSize({ width: target.clientWidth || 600, height: options.height || 180 });
  new ResizeObserver(resize).observe(target);
  return chart;
}

async function drawAppCharts() {
  const container = document.querySelector(".charts[data-app]");
  if (!container) return;
  const app = container.dataset.app;
  const from = Number(container.dataset.from);
  const to = Number(container.dataset.to);
  const { colors: palette } = colors();
  const target = (name) => container.querySelector(`[data-chart="${name}"]`);
  try {
    const requests = await fetchSeries([`app:${app}:2xx`, `app:${app}:3xx`, `app:${app}:4xx`, `app:${app}:5xx`], from, to);
    const requestSeries = requests.series.map((entry, index) => ({ ...entry, label: entry.key.split(":")[2], color: palette[index] }));
    drawChart(target("requests"), requestSeries, "count", { fill: true });
    const errors = await fetchSeries([`app:${app}:error_rate`], from, to);
    drawChart(target("errors"), errors.series, "percent");
    const latency = await fetchSeries([`app:${app}:p50`, `app:${app}:p95`, `app:${app}:p99`], from, to);
    const latencySeries = latency.series.map((entry, index) => ({ ...entry, label: entry.key.split(":")[2], color: palette[4 + index] }));
    drawChart(target("latency"), latencySeries, "ms");
    const cpu = await fetchSeries([`slot:${app} / api #1:cpu`], from, to).catch(() => null);
    if (cpu) drawChart(target("cpu"), cpu.series, "millicores");
    const memory = await fetchSeries([`slot:${app} / api #1:memory`], from, to).catch(() => null);
    if (memory) drawChart(target("memory"), memory.series, "bytes");
    const network = await fetchSeries([`slot:${app} / api #1:net_rx`, `slot:${app} / api #1:net_tx`], from, to).catch(() => null);
    if (network) drawChart(target("network"), network.series, "bytes");
  } catch (error) {
    console.error("charts:", error);
    container.insertAdjacentHTML("afterbegin", '<p class="empty">Chart data is unavailable for this range. Pick a shorter range or check the database.</p>');
  }
}

async function drawHostCharts() {
  const container = document.querySelector(".charts[data-host]");
  if (!container) return;
  const from = Number(container.dataset.from);
  const to = Number(container.dataset.to);
  try {
    drawChart(container.querySelector('[data-chart="host-cpu"]'), (await fetchSeries(["host:cpu"], from, to)).series, "percent");
    drawChart(container.querySelector('[data-chart="host-load"]'), (await fetchSeries(["host:load1"], from, to)).series, "load");
    drawChart(container.querySelector('[data-chart="host-memory"]'), (await fetchSeries(["host:memory"], from, to)).series, "bytes");
    drawChart(container.querySelector('[data-chart="host-swap"]'), (await fetchSeries(["host:swap"], from, to)).series, "bytes");
    for (const element of container.querySelectorAll("[data-chart^='disk-']")) {
      const disk = element.dataset.chart.slice(5);
      drawChart(element, (await fetchSeries([`disk:${disk}:used`], from, to)).series, "percent");
    }
  } catch (error) {
    console.error("charts:", error);
  }
}

async function drawIncidentChart() {
  const container = document.querySelector(".charts[data-key]");
  if (!container) return;
  try {
    const series = await fetchSeries([container.dataset.key], Number(container.dataset.from), Number(container.dataset.to));
    drawChart(container.querySelector('[data-chart="incident"]'), series.series, "ms");
  } catch (error) {
    console.error("charts:", error);
  }
}

async function drawPatterns() {
  const container = document.querySelector(".patterns[data-key]");
  if (!container || !container.dataset.key) return;
  try {
    const params = new URLSearchParams({ key: container.dataset.key, weeks: container.dataset.weeks });
    const response = await fetch("/api/patterns?" + params);
    if (!response.ok) throw new Error(await response.text());
    const patterns = await response.json();
    const heatmap = container.querySelector(".heatmap");
    const days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
    const max = Math.max(1, ...patterns.heatmap.flat());
    patterns.heatmap.forEach((row, day) => {
      heatmap.insertAdjacentHTML("beforeend", `<span class="day">${days[day]}</span>`);
      row.forEach((value) => {
        const alpha = Math.max(0.05, value / max);
        heatmap.insertAdjacentHTML("beforeend", `<span title="${days[day]} ${String(row.indexOf(value)).padStart(2, "0")}:00 — ${number(value)}" style="background: color-mix(in srgb, var(--selected) ${Math.round(alpha * 100)}%, var(--grid))"></span>`);
      });
    });
    const profile = patterns.day_profile.map((value, hour) => [hour, value]);
    drawChart(container.querySelector('[data-chart="profile"]'), [{ key: "profile", label: "average", points: profile }], "count");
    drawChart(container.querySelector('[data-chart="trend"]'), [{ key: "trend", label: "trend", points: patterns.trend.map((entry) => [new Date(entry.day + "T00:00:00Z").getTime() / 1000, entry.value]) }], "count");
  } catch (error) {
    console.error("patterns:", error);
  }
}

function drawSparklines() {
  for (const element of document.querySelectorAll(".spark[data-values]")) {
    const values = element.dataset.values.split(",").filter((value) => value !== "").map(Number);
    if (!values.length) continue;
    const max = Math.max(1, ...values);
    const width = 64;
    const height = 16;
    const points = values.map((value, index) => `${(index / Math.max(1, values.length - 1)) * width},${height - (value / max) * height}`).join(" ");
    element.innerHTML = `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" aria-hidden="true"><polyline fill="none" stroke="var(--muted)" stroke-width="1" points="${points}"/></svg>`;
  }
}

function setupLogs() {
  const drawer = document.querySelector(".logs-drawer");
  if (!drawer) return;
  const containers = [...document.querySelectorAll(".charts [data-chart]")];
  const select = drawer.querySelector(".logs-container");
  const output = drawer.querySelector(".logs-output");
  const filter = drawer.querySelector(".logs-filter");
  const follow = drawer.querySelector(".logs-follow");
  let source = null;
  let lines = [];
  const render = () => {
    const needle = filter.value.toLowerCase();
    output.textContent = lines.filter((line) => !needle || line.toLowerCase().includes(needle)).join("\n");
  };
  const stop = () => { if (source) { source.close(); source = null; } };
  const open = (at) => {
    stop();
    lines = [];
    output.textContent = "";
    drawer.hidden = false;
    const since = Math.floor(at - 120);
    const until = Math.floor(at + 120);
    const show = (stream) => {
      const params = new URLSearchParams({ container: select.value, since: String(since), until: String(until), follow: String(stream && follow.checked) });
      if (!stream || !follow.checked) {
        fetch("/api/logs?" + params).then((response) => response.text()).then((text) => {
          lines = text.split("\n").filter(Boolean);
          render();
        });
        return;
      }
      source = new EventSource("/api/logs?" + params);
      source.onmessage = (event) => {
        try {
          const data = JSON.parse(event.data);
          lines.push(data.line);
          render();
          output.scrollTop = output.scrollHeight;
        } catch (_) { /* ignore malformed events */ }
      };
    };
    show(false);
    show(true);
  };
  for (const chart of containers) {
    chart.addEventListener("click", (event) => {
      const rect = chart.getBoundingClientRect();
      const ratio = Math.min(1, Math.max(0, (event.clientX - rect.left) / rect.width));
      const from = Number(document.querySelector(".charts").dataset.from);
      const to = Number(document.querySelector(".charts").dataset.to);
      open(from + ratio * (to - from));
    });
  }
  const app = document.querySelector(".charts[data-app]");
  if (app) {
    fetch(`/api/events?app=${encodeURIComponent(app.dataset.app)}&from=${Math.floor(Date.now() / 1000) - 86400}&to=${Math.floor(Date.now() / 1000)}`)
      .then((response) => response.json())
      .catch(() => null);
  }
  select.addEventListener("change", () => { if (!drawer.hidden) open(Number(drawer.dataset.at || 0)); });
  filter.addEventListener("input", render);
  drawer.querySelector(".logs-close").addEventListener("click", () => { stop(); drawer.hidden = true; });
  window.addEventListener("beforeunload", stop);
}

drawSparklines();
drawAppCharts();
drawHostCharts();
drawIncidentChart();
drawPatterns();
setupLogs();
