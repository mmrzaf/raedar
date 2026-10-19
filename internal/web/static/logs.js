import { charts as drawn } from "./charts.js";

// The log drawer. Clicking a chart opens the logs of the clicked moment: a
// window of two minutes on each side, or, with Follow on, a live stream that
// starts at the window's beginning.

const WINDOW_SECONDS = 120;

export function setupLogs() {
  const drawer = document.querySelector(".logs-drawer");
  const charts = document.querySelector(".charts[data-app]");
  if (!drawer || !charts) return;
  const select = drawer.querySelector(".logs-container");
  const output = drawer.querySelector(".logs-output");
  const filter = drawer.querySelector(".logs-filter");
  const follow = drawer.querySelector(".logs-follow");
  const status = drawer.querySelector(".logs-status");
  let stream = null;
  let lines = [];
  let moment = null;

  const render = () => {
    const needle = filter.value.toLowerCase();
    output.textContent = lines.filter((line) => !needle || line.toLowerCase().includes(needle)).join("\n");
    if (stream) output.scrollTop = output.scrollHeight;
  };
  const stop = () => {
    if (stream) stream.close();
    stream = null;
  };
  const params = (extra) => new URLSearchParams({
    container: select.value, since: String(Math.floor(moment - WINDOW_SECONDS)), ...extra,
  });

  const load = async () => {
    stop();
    lines = [];
    status.textContent = "Loading…";
    render();
    if (follow.checked) {
      stream = new EventSource(`/api/logs?${params({ follow: "true" })}`);
      stream.onopen = () => { status.textContent = "Live"; };
      stream.onmessage = (event) => {
        lines.push(JSON.parse(event.data).line);
        render();
      };
      // The browser would reconnect and replay the stream from the start.
      stream.onerror = () => {
        stop();
        status.textContent = "The stream ended.";
      };
      return;
    }
    try {
      const response = await fetch(`/api/logs?${params({ until: String(Math.floor(moment + WINDOW_SECONDS)) })}`);
      if (!response.ok) throw new Error((await response.text()).trim());
      lines = (await response.text()).split("\n").filter(Boolean);
      status.textContent = lines.length ? "" : "No log lines in this window.";
    } catch (error) {
      status.textContent = `Logs are unavailable: ${error.message}`;
    }
    render();
  };

  const open = (seconds) => {
    moment = seconds;
    drawer.hidden = false;
    load();
  };

  // A click opens the logs, but a drag across the chart is a zoom.
  for (const chart of charts.querySelectorAll("[data-chart]")) {
    let pressedAt = null;
    chart.addEventListener("pointerdown", (event) => { pressedAt = event.clientX; });
    chart.addEventListener("pointerup", (event) => {
      if (pressedAt === null || Math.abs(event.clientX - pressedAt) > 4) return;
      const instance = drawn.get(chart);
      const plot = chart.querySelector(".u-over");
      if (!instance || !plot) return;
      const rect = plot.getBoundingClientRect();
      if (event.clientX < rect.left || event.clientX > rect.right) return;
      open(instance.posToVal(event.clientX - rect.left, "x"));
    });
  }
  select.addEventListener("change", () => { if (moment !== null) load(); });
  follow.addEventListener("change", () => { if (moment !== null) load(); });
  filter.addEventListener("input", render);
  drawer.querySelector(".logs-close").addEventListener("click", () => {
    stop();
    drawer.hidden = true;
  });
  window.addEventListener("pagehide", stop);
}
