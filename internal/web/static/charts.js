// Chart drawing on top of uPlot, which is loaded as a global by its script.
import { formatAxis, formatValue } from "./format.js";

const pad = (value) => String(value).padStart(2, "0");

const cssVariable = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

// palette returns the series colors of the current theme.
export const palette = () => ({
  text: cssVariable("--text"),
  muted: cssVariable("--muted"),
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

// MAX_MARKED_POINTS is how many points a series may have and still get a
// marker on each; denser series read better as a plain line.
const MAX_MARKED_POINTS = 25;

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

// drawChart draws one chart in target. Each series is {label, color, dash,
// points: [[seconds or hour, value|null], ...]} and unit is the series unit.
// options: height, fill, hourly (x is the hour of day), ceiling (percent top),
// range ([from, to] seconds of the time axis), syncKey (charts with the same
// key share a cursor).
export function drawChart(target, series, unit, options = {}) {
  if (!series.length || !hasData(series)) {
    showEmpty(target, options.empty || "No data in this range.");
    return null;
  }
  target.replaceChildren();
  const colors = palette();
  const height = options.height || 180;
  const width = () => target.clientWidth || 600;
  const data = align(series);
  const axisStyle = { stroke: colors.muted, grid: { stroke: colors.grid, width: 1 }, ticks: { stroke: colors.grid, width: 1 } };
  const toDate = zonedDate();
  const xAxis = options.hourly
    ? {
      ...axisStyle,
      splits: () => Array.from({ length: 8 }, (_, index) => index * 3),
      values: (chart, ticks) => ticks.map((hour) => `${pad(hour)}:00`),
    }
    : {
      ...axisStyle,
      values: (chart, ticks) => {
        const format = timeLabel(chart.scales.x.max - chart.scales.x.min);
        return ticks.map((tick) => format(toDate(tick)));
      },
    };
  const single = series.length === 1;
  const readout = document.createElement("div");
  readout.className = "chart-readout";
  target.append(readout);
  const span = options.hourly ? [0, 23] : xRange(data, options.range);
  const chart = new uPlot({
    width: width(),
    height,
    tzDate: toDate,
    cursor: { sync: { key: options.syncKey || "raedar" }, points: { size: 6 } },
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
          : (chart, value) => (value === null ? "" : uPlot.fmtDate("{YYYY}-{MM}-{DD} {HH}:{mm}")(toDate(value))),
      },
      ...series.map((entry, index) => {
        const stroke = entry.color || colors.series[index % colors.series.length];
        return {
          label: entry.label || `series ${index + 1}`,
          stroke,
          width: 1.5,
          fill: options.fill ? `${stroke}33` : undefined,
          dash: entry.dash ? [4, 4] : undefined,
          spanGaps: false,
          points: { show: entry.points.length <= MAX_MARKED_POINTS, size: 4 },
          value: (chart, value) => formatValue(unit, value),
        };
      }),
    ],
    legend: { show: !single, live: true },
    hooks: {
      setCursor: [(chart) => {
        const index = chart.cursor.idx;
        if (index == null) {
          readout.textContent = "";
          return;
        }
        const when = chart.series[0].value(chart, data[0][index]);
        readout.textContent = single ? `${when} · ${formatValue(unit, data[1][index])}` : when;
      }],
    },
  }, data, target);
  new ResizeObserver(() => chart.setSize({ width: width(), height })).observe(target);
  charts.set(target, chart);
  return chart;
}
