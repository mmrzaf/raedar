// Value formatting for chart axes, tooltips and the heatmap. The units are the
// ones the series API reports: count, percent, ms, bytes, millicores and load.

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB"];

// trim formats a number with at most `digits` decimals and no trailing zeros.
const trim = (value, digits) => {
  const text = value.toFixed(digits);
  return text.includes(".") ? text.replace(/\.?0+$/, "") : text;
};

const bytes = (value) => {
  let scaled = Math.abs(value);
  let unit = 0;
  while (scaled >= 1024 && unit < BYTE_UNITS.length - 1) {
    scaled /= 1024;
    unit++;
  }
  const sign = value < 0 ? "-" : "";
  return `${sign}${trim(scaled, unit === 0 ? 0 : 1)} ${BYTE_UNITS[unit]}`;
};

const milliseconds = (value) => {
  if (value < 1000) return `${trim(value, 0)} ms`;
  if (value < 10000) return `${trim(value / 1000, 1)} s`;
  return `${trim(value / 1000, 0)} s`;
};

const count = (value) => {
  const size = Math.abs(value);
  if (size >= 1e6) return `${trim(value / 1e6, 1)}M`;
  if (size >= 1e3) return `${trim(value / 1e3, 1)}k`;
  return trim(value, size < 10 && !Number.isInteger(value) ? 1 : 0);
};

const FORMATTERS = {
  count,
  bytes,
  ms: milliseconds,
  percent: (value) => `${trim(value, value < 10 ? 1 : 0)}%`,
  millicores: (value) => `${trim(value / 1000, 2)} cores`,
  load: (value) => trim(value, 2),
};

// formatValue renders a value of a series unit; null stays an em dash.
export function formatValue(unit, value) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—";
  return (FORMATTERS[unit] || count)(value);
}

// formatAxis renders an axis tick, which only needs to be short and exact.
export function formatAxis(unit, value) {
  return formatValue(unit, value);
}
