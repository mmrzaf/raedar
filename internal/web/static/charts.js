// Chart drawing on top of uPlot, which is loaded as a global by its script.
import { formatAxis, formatValue } from "./format.js";

const pad = (value) => String(value).padStart(2, "0");

const cssVariable = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// palette returns the series colors of the current theme.
export const palette = () => ({
  text: cssVariable("--text"),
  muted: cssVariable("--muted"),
  surface: cssVariable("--surface"),
  grid: cssVariable("--grid"),
  status: [cssVariable("--series-2xx"), cssVariable("--series-3xx"), cssVariable("--series-4xx"), cssVariable("--series-5xx")],
  series: ["--series-a", "--series-b", "--series-c", "--series-d", "--series-e", "--series-f"].map(cssVariable),
});

// timezone is the zone the server shows times in, so charts and tables agree.
const timezone = () => document.querySelector('meta[name="raedar-timezone"]')?.content || undefined;

const zonedDate = () => {
  const zone = timezone();
  return (seconds) => (zone ? uPlot.tzDate(new Date(seconds * 1e3), zone) : new Date(seconds * 1e3));
};

const DAY = 24 * 3600;

// MIN_AXIS_SIZE keeps the plot areas of charts on one page aligned when their
// labels are short.
const MIN_AXIS_SIZE = 64;

// fitToLabels sizes an axis to its longest label, so that labels such as
// "0.25 cores" are never cut off. uPlot calls it until the size settles.
function fitToLabels(chart, values, axisIndex, cycle) {
  const axis = chart.axes[axisIndex];
  if (cycle > 1) return axis._size;
  let size = axis.ticks.size + axis.gap;
  const longest = (values ?? []).reduce((best, value) => (value.length > best.length ? value : best), "");
  if (longest !== "") {
    chart.ctx.font = axis.font[0];
    size += chart.ctx.measureText(longest).width / devicePixelRatio;
  }
  return Math.max(MIN_AXIS_SIZE, Math.ceil(size));
}

// BINARY_STEPS are axis steps that read well in KB, MB and GB: 1, 2, 4 ... 512
// of each unit, since byte values are shown in powers of 1024.
const BINARY_STEPS = Array.from({ length: 5 }, (_, power) => [1, 2, 4, 8, 16, 32, 64, 128, 256, 512].map((step) => step * 1024 ** power)).flat();

// timeLabel picks the tick format from the visible span.
const timeLabel = (span) => {
  if (span <= 2 * DAY) return uPlot.fmtDate("{HH}:{mm}");
  return uPlot.fmtDate("{MMM} {D}");
};

// yRange pins percent charts to a sensible 0-based range instead of letting
// the data's own extremes decide.
const yRange = (unit, ceiling) => (chart, min, max) => {
  if (unit === "percent") {
    const top = ceiling ?? [5, 10, 25, 50, 100].find((step) => step >= max * 1.1) ?? 100;
    return [0, top];
  }
  if (unit === "count" || unit === "bytes" || unit === "ms" || unit === "millicores") {
    return [0, Math.max(max * 1.1, 1)];
  }
  return [Math.min(0, min), Math.max(max * 1.1, 1)];
};

// align puts the series on one shared x axis, as uPlot needs, filling the
// positions where a series has no point with null.
const align = (series) => {
  const stamps = [...new Set(series.flatMap((entry) => entry.points.map((point) => point[0])))].sort((a, b) => a - b);
  return [stamps, ...series.map((entry) => {
    const values = new Map(entry.points);
    return stamps.map((stamp) => (values.has(stamp) ? values.get(stamp) : null));
  })];
};

// xRange returns the visible time span: the requested one, or around the only
// timestamp so that a single point does not stretch the axis over years.
const xRange = (data, requested) => {
  if (requested) return requested;
  const stamps = data[0];
  return stamps.length === 1 ? [stamps[0] - 3600, stamps[0] + 3600] : null;
};

const hasData = (series) => series.some((entry) => entry.points.some((point) => point[1] !== null));

const showEmpty = (target, message) => {
  target.replaceChildren();
  const note = document.createElement("p");
  note.className = "empty";
  note.textContent = message;
  target.append(note);
};

// charts maps a chart's element to its uPlot instance, so other modules can
// translate pointer positions into times.
export const charts = new WeakMap();

// SPARSE_POINTS is how many points a series may have and still be drawn with a
// dot on each, so that one or two points do not vanish.
const SPARSE_POINTS = 14;

// withAlpha returns a #rrggbb color with an alpha channel.
const withAlpha = (hex, alpha) => `${hex}${Math.round(alpha * 255).toString(16).padStart(2, "0")}`;

// lastValue returns the newest value of a series that has one.
const lastValue = (points) => {
  for (let i = points.length - 1; i >= 0; i--) {
    if (points[i][1] !== null) return points[i][1];
  }
  return null;
};

// tooltip builds the floating readout of a chart and keeps it next to the
// cursor. It only follows the chart the pointer is over, even when the cursor
// is synced to other charts.
function tooltip(target) {
  const element = document.createElement("div");
  element.className = "chart-tip";
  element.hidden = true;
  target.append(element);
  let hovered = false;
  target.addEventListener("pointerenter", () => { hovered = true; });
  target.addEventListener("pointerleave", () => { hovered = false; element.hidden = true; });
  return {
    update(chart, data, series, unit, colors) {
      const index = chart.cursor.idx;
      if (!hovered || index == null || chart.cursor.left < 0) {
        element.hidden = true;
        return;
      }
      const head = document.createElement("div");
      head.className = "tip-time";
      head.textContent = chart.series[0].value(chart, data[0][index]);
      const rows = [head];
      series.forEach((entry, i) => {
        const value = data[i + 1][index];
        if (value === null || value === undefined) return;
        const row = document.createElement("div");
        row.className = "tip-row";
        const swatch = document.createElement("span");
        swatch.className = "tip-swatch";
        swatch.style.background = entry.color || colors.series[i % colors.series.length];
        const name = document.createElement("span");
        name.textContent = entry.label || "";
        const number = document.createElement("span");
        number.className = "tip-value";
        number.textContent = formatValue(unit, value);
        row.append(swatch, name, number);
        rows.push(row);
      });
      element.replaceChildren(...rows);
      element.hidden = false;
      const over = chart.over;
      const x = over.offsetLeft + chart.cursor.left;
      const y = over.offsetTop + chart.cursor.top;
      const flip = x + element.offsetWidth + 24 > target.clientWidth;
      element.style.left = `${Math.max(0, flip ? x - element.offsetWidth - 14 : x + 14)}px`;
      element.style.top = `${Math.max(0, Math.min(y - 12, target.clientHeight - element.offsetHeight))}px`;
    },
  };
}

// drawChart draws one chart in target. Each series is {label, color, dash,
// points: [[seconds or hour, value|null], ...]} and unit is the series unit.
// options: height, fill, hourly (x is the hour of day), ceiling (percent top),
// days (one point per day: dates instead of times), range ([from, to] seconds
// of the time axis), syncKey (charts with the same key share a cursor).
export function drawChart(target, series, unit, options = {}) {
  const headline = target.closest(".card")?.querySelector(".card-value");
  if (headline) headline.textContent = "";
  if (!series.length || !hasData(series)) {
    showEmpty(target, options.empty || "No data in this range.");
    return null;
  }
  target.replaceChildren();
  const colors = palette();
  const height = options.height || 200;
  const width = () => target.clientWidth || 600;
  const data = align(series);
  const font = `11px ${getComputedStyle(document.body).fontFamily}`;
  const axisStyle = { stroke: colors.muted, font, gap: 8, grid: { stroke: colors.grid, width: 1 }, ticks: { show: false }, border: { show: false } };
  const toDate = zonedDate();
  const xAxis = options.hourly
    ? {
      ...axisStyle,
      size: 30,
      grid: { show: false },
      splits: () => Array.from({ length: 8 }, (_, index) => index * 3),
      values: (chart, ticks) => ticks.map((hour) => `${pad(hour)}:00`),
    }
    : {
      ...axisStyle,
      size: 30,
      grid: { show: false },
      splits: options.days && data[0].length <= 10 ? () => data[0] : undefined,
      values: (chart, ticks) => {
        const format = options.days ? uPlot.fmtDate("{MMM} {D}") : timeLabel(chart.scales.x.max - chart.scales.x.min);
        return ticks.map((tick) => format(toDate(tick)));
      },
    };
  const single = series.length === 1;
  const tip = tooltip(target);
  const span = options.hourly ? [0, 23] : xRange(data, options.range);
  const chart = new uPlot({
    width: width(),
    height,
    padding: [12, 18, 0, 0],
    tzDate: toDate,
    cursor: {
      sync: { key: options.syncKey || "raedar" },
      points: { size: 9, width: 2, stroke: () => colors.surface, fill: (u, seriesIndex) => u.series[seriesIndex].stroke() },
      drag: { x: true, y: false },
    },
    scales: {
      x: options.hourly ? { time: false, range: span } : { time: true, range: span ? () => span : undefined },
      y: { range: yRange(unit, options.ceiling) },
    },
    axes: [xAxis, {
      ...axisStyle,
      size: fitToLabels,
      incrs: unit === "bytes" ? BINARY_STEPS : undefined,
      values: (chart, ticks) => ticks.map((tick) => formatAxis(unit, tick)),
    }],
    series: [
      {
        value: options.hourly
          ? (chart, value) => (value === null ? "" : `${pad(value)}:00`)
          : (chart, value) => (value === null ? "" : uPlot.fmtDate(options.days ? "{YYYY}-{MM}-{DD}" : "{YYYY}-{MM}-{DD} {HH}:{mm}")(toDate(value))),
      },
      ...series.map((entry, index) => {
        const stroke = entry.color || colors.series[index % colors.series.length];
        const wash = options.fill ? 0.2 : single ? 0.1 : 0;
        return {
          label: entry.label || `series ${index + 1}`,
          stroke,
          width: 2,
          fill: wash ? withAlpha(stroke, wash) : undefined,
          dash: entry.dash ? [5, 4] : undefined,
          spanGaps: false,
          points: { show: entry.points.length <= SPARSE_POINTS, size: 7, stroke: colors.surface, width: 2, fill: stroke },
          value: (chart, value) => formatValue(unit, value),
        };
      }),
    ],
    legend: { show: !single, live: false, markers: { width: 0, fill: (u, i) => u.series[i].stroke() } },
    hooks: { setCursor: [(chart) => tip.update(chart, data, series, unit, colors)] },
  }, data, target);
  if (headline && single) {
    const latest = lastValue(series[0].points);
    if (latest !== null) headline.textContent = formatValue(unit, latest);
  }
  new ResizeObserver(() => chart.setSize({ width: width(), height })).observe(target);
  charts.set(target, chart);
  return chart;
}
