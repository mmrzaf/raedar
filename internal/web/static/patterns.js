// The patterns page: week heatmap, day profile and long-term trend.
import { fetchPatterns } from "./api.js";
import { drawChart, palette } from "./charts.js";
import { formatValue } from "./format.js";

const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const LEVELS = 7;
const pad = (value) => String(value).padStart(2, "0");

const cell = (className, text) => {
  const element = document.createElement("span");
  if (className) element.className = className;
  if (text !== undefined) element.textContent = text;
  return element;
};

// valuesOf returns the non-null cells of the heatmap with their position.
const valuesOf = (rows) => rows.flatMap((row, day) => row.flatMap((value, hour) => (
  value === null ? [] : [{ day, hour, value }]
)));

const meanOf = (values) => {
  const present = values.filter((value) => value !== null && value !== undefined);
  return present.length === 0 ? null : present.reduce((total, value) => total + value, 0) / present.length;
};

// levelOf stretches the data over the ramp instead of starting at zero. A
// metric that sits between 40% and 60% all week still shows where it moves.
const levelOf = (value, min, max) => {
  if (max <= min) return Math.floor((LEVELS - 1) / 2);
  return Math.round(((value - min) / (max - min)) * (LEVELS - 1));
};

function drawHeatmap(card, rows, unit) {
  const heatmap = card.querySelector(".heatmap");
  const values = valuesOf(rows);
  const peak = values.reduce((best, entry) => (best === null || entry.value > best.value ? entry : best), null);
  const min = values.reduce((low, entry) => (low === null || entry.value < low.value ? entry : low), null);
  const average = values.length === 0 ? null : values.reduce((total, entry) => total + entry.value, 0) / values.length;

  const nodes = [cell("day")];
  for (let hour = 0; hour < 24; hour++) {
    const label = cell("hour", hour % 3 === 0 ? pad(hour) : "");
    label.dataset.hour = String(hour);
    nodes.push(label);
  }
  rows.forEach((row, day) => {
    const label = cell("day", DAYS[day]);
    label.dataset.day = String(day);
    nodes.push(label);
    row.forEach((value, hour) => {
      const square = cell(value === null ? "cell cell-empty" : "cell");
      const when = `${DAYS[day]} ${pad(hour)}:00`;
      square.dataset.day = String(day);
      square.dataset.hour = String(hour);
      square.setAttribute("role", "img");
      square.setAttribute("aria-label", value === null ? `${when}, no data` : `${when}, ${formatValue(unit, value)}`);
      if (value !== null) {
        square.dataset.level = String(levelOf(value, min.value, peak.value));
        square.dataset.value = String(value);
        if (peak !== null && peak.value > min.value && value === peak.value && day === peak.day && hour === peak.hour) {
          square.classList.add("cell-peak");
        }
      }
      nodes.push(square);
    });
  });
  heatmap.replaceChildren(...nodes);

  card.querySelector(".legend-min").textContent = min === null ? "no data" : `low ${formatValue(unit, min.value)}`;
  card.querySelector(".legend-max").textContent = peak === null ? "" : `${formatValue(unit, peak.value)} high`;
  setupHeatTip(card, unit, average);
  return { values, peak, min, average };
}

// setupHeatTip shows one cell's value and row and column next to the pointer.
function setupHeatTip(card, unit, average) {
  const heatmap = card.querySelector(".heatmap");
  const tip = card.querySelector(".heat-tip");
  const clear = () => {
    tip.hidden = true;
    for (const label of heatmap.querySelectorAll(".is-active")) label.classList.remove("is-active");
  };
  heatmap.addEventListener("pointermove", (event) => {
    const square = event.target.closest(".cell");
    if (!square) {
      clear();
      return;
    }
    for (const label of heatmap.querySelectorAll(".is-active")) label.classList.remove("is-active");
    heatmap.querySelector(`.day[data-day="${square.dataset.day}"]`)?.classList.add("is-active");
    heatmap.querySelector(`.hour[data-hour="${square.dataset.hour}"]`)?.classList.add("is-active");
    if (!square.dataset.value) {
      tip.hidden = true;
      return;
    }
    const value = Number(square.dataset.value);
    const head = document.createElement("div");
    head.className = "tip-time";
    head.textContent = `${DAYS[Number(square.dataset.day)]} ${pad(square.dataset.hour)}:00`;
    const row = document.createElement("div");
    row.className = "tip-row";
    const swatch = document.createElement("span");
    swatch.className = "tip-swatch";
    swatch.style.background = getComputedStyle(square).backgroundColor;
    const name = document.createElement("span");
    name.textContent = "average";
    const number = document.createElement("span");
    number.className = "tip-value";
    number.textContent = formatValue(unit, value);
    row.append(swatch, name, number);
    const nodes = [head, row];
    if (average !== null && average > 0) {
      const note = document.createElement("div");
      note.className = "tip-note";
      const ratio = value / average;
      note.textContent = Math.abs(ratio - 1) >= 0.05 ? `${ratio.toFixed(1)}× the week average` : "close to the week average";
      nodes.push(note);
    }
    tip.replaceChildren(...nodes);
    tip.hidden = false;
    const bounds = card.getBoundingClientRect();
    const x = event.clientX - bounds.left;
    const y = event.clientY - bounds.top;
    const flip = x + tip.offsetWidth + 24 > card.clientWidth;
    tip.style.left = `${Math.max(8, flip ? x - tip.offsetWidth - 14 : x + 14)}px`;
    tip.style.top = `${Math.max(8, Math.min(y - 12, card.clientHeight - tip.offsetHeight - 8))}px`;
  });
  heatmap.addEventListener("pointerleave", clear);
}

// averageProfile returns the mean of a day profile, ignoring hours without
// data.
const profileMean = (profile) => meanOf(profile || []);

// changeText phrases a difference; percentage points are not percentages.
const changeText = (unit, value) => (unit === "percent" ? `${formatValue("count", value)} pts` : formatValue(unit, value));

function drawSummary(container, summary, unit, weeks) {
  if (summary.peak === null || summary.min === null) return;
  const element = container.querySelector(".pattern-summary");
  const set = (name, value, note) => {
    element.querySelector(`[data-summary="${name}"]`).textContent = value;
    element.querySelector(`[data-summary="${name}-note"]`).textContent = note;
  };
  set("high", `${DAYS[summary.peak.day]} ${pad(summary.peak.hour)}:00`, `${formatValue(unit, summary.peak.value)} on average`);
  set("low", `${DAYS[summary.min.day]} ${pad(summary.min.hour)}:00`, `${formatValue(unit, summary.min.value)} on average`);

  const recent = profileMean(summary.patterns.day_profile_recent);
  if (recent === null) {
    set("recent", "—", "no data in the last week");
  } else {
    set("recent", formatValue(unit, recent), "average hour");
    const previous = profileMean(summary.patterns.day_profile_previous);
    const note = element.querySelector('[data-summary="recent-note"]');
    if (previous !== null) {
      const difference = recent - previous;
      const direction = difference > 0 ? "↑" : difference < 0 ? "↓" : "→";
      const period = weeks > 1 ? `the previous ${weeks - 1} week${weeks > 2 ? "s" : ""}` : "before";
      note.textContent = `${direction} ${changeText(unit, Math.abs(difference))} vs ${period}`;
    }
  }
  element.hidden = false;
}

// rollingAverage smooths daily values so that a slow change is visible under
// the weekday rhythm.
const rollingAverage = (points, window) => points.map(([stamp], index) => {
  if (index + 1 < window) return [stamp, null];
  const slice = points.slice(index + 1 - window, index + 1).map((point) => point[1]);
  return [stamp, slice.reduce((total, value) => total + value, 0) / slice.length];
});

export async function drawPatterns() {
  const container = document.querySelector(".patterns[data-series]");
  if (!container || !container.dataset.series) return;
  const message = container.querySelector(".patterns-error");
  try {
    const patterns = await fetchPatterns(container.dataset.series, container.dataset.weeks);
    const unit = patterns.unit;
    const weeks = Number(container.dataset.weeks) || 4;
    const summary = drawHeatmap(container.querySelector(".heatmap-card"), patterns.heatmap, unit);
    drawSummary(container, { ...summary, patterns }, unit, weeks);
    const hours = (values) => values.map((value, hour) => [hour, value]);
    const colors = palette();
    drawChart(container.querySelector('[data-chart="profile"]'), [
      { label: "last 7 days", color: colors.series[0], points: hours(patterns.day_profile_recent) },
      { label: "before", color: colors.muted, dash: true, points: hours(patterns.day_profile_previous) },
    ].filter((entry) => entry.points.some((point) => point[1] !== null)), unit,
    { hourly: true, syncKey: "patterns", empty: "Not enough data for a day profile yet." });
    const trend = patterns.trend.map((entry) => [entry.ts, entry.value]);
    const series = [{ label: "daily", points: trend }];
    if (trend.length > 10) {
      series.push({ label: "7-day average", color: colors.series[1], dash: true, points: rollingAverage(trend, 7) });
    }
    drawChart(container.querySelector('[data-chart="trend"]'), series, unit,
      { days: true, empty: "No daily data yet." });
  } catch (error) {
    console.error("patterns:", error);
    message.hidden = false;
  }
}
