// The patterns page: week heatmap, day profile and long-term trend.
import { fetchPatterns } from "./api.js";
import { drawChart, palette } from "./charts.js";
import { formatValue } from "./format.js";

const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const pad = (value) => String(value).padStart(2, "0");

const cell = (className, text) => {
  const element = document.createElement("span");
  if (className) element.className = className;
  if (text !== undefined) element.textContent = text;
  return element;
};

function drawHeatmap(heatmap, rows, unit) {
  const peak = Math.max(0, ...rows.flat().filter((value) => value !== null));
  const nodes = [cell("day")];
  for (let hour = 0; hour < 24; hour++) nodes.push(cell("hour", hour % 3 === 0 ? pad(hour) : ""));
  rows.forEach((row, day) => {
    nodes.push(cell("day", DAYS[day]));
    row.forEach((value, hour) => {
      const square = cell(value === null ? "cell cell-empty" : "cell");
      const when = `${DAYS[day]} ${pad(hour)}:00`;
      square.title = value === null ? `${when} — no data` : `${when} — ${formatValue(unit, value)}`;
      if (value !== null) square.style.setProperty("--level", String(peak > 0 ? Math.max(value / peak, 0.08) : 0.08));
      nodes.push(square);
    });
  });
  heatmap.replaceChildren(...nodes);
}

export async function drawPatterns() {
  const container = document.querySelector(".patterns[data-series]");
  if (!container || !container.dataset.series) return;
  const message = container.querySelector(".patterns-error");
  try {
    const patterns = await fetchPatterns(container.dataset.series, container.dataset.weeks);
    const unit = patterns.unit;
    drawHeatmap(container.querySelector(".heatmap"), patterns.heatmap, unit);
    const hours = (values) => values.map((value, hour) => [hour, value]);
    const colors = palette();
    drawChart(container.querySelector('[data-chart="profile"]'), [
      { label: "last 7 days", color: colors.series[0], points: hours(patterns.day_profile_recent) },
      { label: "before", color: colors.series[2], points: hours(patterns.day_profile_previous) },
    ].filter((entry) => entry.points.some((point) => point[1] !== null)), unit,
    { hourly: true, syncKey: "patterns", empty: "Not enough data for a day profile yet." });
    drawChart(container.querySelector('[data-chart="trend"]'), [
      { label: "daily", points: patterns.trend.map((entry) => [entry.ts, entry.value]) },
    ], unit, { empty: "No daily data yet." });
  } catch (error) {
    console.error("patterns:", error);
    message.hidden = false;
  }
}
