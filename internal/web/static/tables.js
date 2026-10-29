// Client-side sorting for the data tables. Clicking a column header sorts the
// rows; clicking it again reverses the order. The order is kept for the
// session so that the overview's automatic reload does not lose it.

const EMPTY = new Set(["", "—", "–", "-", "never"]);
const DURATION = { ms: 0.001, s: 1, min: 60, h: 3600, d: 86400, day: 86400, days: 86400 };
const BYTES = { b: 1, kb: 1024, mb: 1024 ** 2, gb: 1024 ** 3, tb: 1024 ** 4 };
const BADGE_RANK = new Map([
  ["critical", 0], ["failed", 0],
  ["warning", 1], ["expiring", 1], ["pending", 1], ["held", 1], ["cooldown", 1],
  ["acked", 2], ["info", 2],
  ["ok", 3], ["sent", 3], ["running", 3], ["healthy", 3],
]);

// parseText turns a rendered cell into a number when it can. Units are
// converted to one base per kind, so bytes, durations and counts sort by their
// value rather than by their formatted text.
function parseText(text) {
  const raw = text.trim();
  if (EMPTY.has(raw)) return null;
  if (raw === "just now") return 0;
  const ratio = /^(\d+(?:\.\d+)?)\s*\/\s*(\d+(?:\.\d+)?)$/.exec(raw);
  if (ratio) {
    const total = Number(ratio[2]);
    return total > 0 ? Number(ratio[1]) / total : 0;
  }
  // The match runs to the end so that names such as "3d-printer", dates and
  // prose that happen to start with a number stay text.
  const match = /^[↑↓\s]*([<>]?)\s*(-?[\d.,]+)\s*([a-z%/]+)?\s*(?:ago)?$/i.exec(raw);
  if (!match) return raw;
  const value = Number(match[2].replace(/,/g, ""));
  if (!Number.isFinite(value)) return raw;
  const suffix = (match[3] || "").toLowerCase();
  if (BYTES[suffix] !== undefined) return value * BYTES[suffix];
  if (DURATION[suffix] !== undefined) return value * DURATION[suffix];
  if (suffix === "k") return value * 1000;
  if (suffix === "m") return value * 1e6;
  return value;
}

// valueOf reads a cell: an explicit data-sort wins, then the rank of a status
// badge, then the formatted text.
function valueOf(cell) {
  if (cell.dataset.sort !== undefined) {
    const raw = cell.dataset.sort.trim();
    if (raw === "") return null;
    const number = Number(raw);
    return Number.isFinite(number) ? number : raw;
  }
  const badge = cell.querySelector(".badge");
  if (badge) {
    for (const name of badge.classList) {
      if (name.startsWith("badge-") && BADGE_RANK.has(name.slice(6))) return BADGE_RANK.get(name.slice(6));
    }
  }
  return parseText(cell.textContent);
}

const compareText = (left, right) => String(left).localeCompare(String(right), undefined, { numeric: true, sensitivity: "base" });

// compare sorts numbers numerically and everything else as text. Missing
// values stay last in both directions.
function compare(left, right, direction) {
  if (left === null) return right === null ? 0 : 1;
  if (right === null) return -1;
  const order = direction === "asc" ? 1 : -1;
  if (typeof left === "number" && typeof right === "number") return order * (left - right);
  return order * compareText(left, right);
}

function storageKey(table, index) {
  return `raedar-sort:${location.pathname}:${table.dataset.table || index}`;
}

function remember(table, index, state) {
  if (!state) return;
  try {
    sessionStorage.setItem(storageKey(table, index), JSON.stringify(state));
  } catch (_) { /* sorting still works without a place to remember it */ }
}

function recall(table, index) {
  try {
    const raw = sessionStorage.getItem(storageKey(table, index));
    if (!raw) return null;
    const state = JSON.parse(raw);
    return Number.isInteger(state.column) && (state.direction === "asc" || state.direction === "desc") ? state : null;
  } catch (_) {
    return null;
  }
}

function install(table, index) {
  const header = table.tHead?.rows[0];
  const body = table.tBodies[0];
  if (!header || !body || header.cells.length === 0 || body.rows.length === 0) return;

  const rows = [...body.rows].map((row, order) => ({ row, order }));
  const columns = [...header.cells];

  const apply = (column, direction, save = true) => {
    const decorated = rows.map((entry) => ({
      ...entry,
      value: entry.row.cells[column] ? valueOf(entry.row.cells[column]) : null,
    }));
    const values = decorated.map((entry) => entry.value).filter((value) => value !== null);
    const numeric = values.length > 0 && values.every((value) => typeof value === "number");
    const asText = (value) => (value === null ? null : typeof value === "number" ? String(value) : value);
    decorated.sort((left, right) => {
      const result = numeric
        ? compare(left.value, right.value, direction)
        : compare(asText(left.value), asText(right.value), direction);
      return result || left.order - right.order;
    });
    body.replaceChildren(...decorated.map((entry) => entry.row));
    columns.forEach((cell, cellIndex) => {
      if (cell.classList.contains("sortable")) {
        cell.setAttribute("aria-sort", cellIndex === column ? (direction === "asc" ? "ascending" : "descending") : "none");
      }
    });
    if (save) remember(table, index, { column, direction });
  };

  columns.forEach((th, column) => {
    const label = th.textContent.trim();
    if (!label) return;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "sort-button";
    button.title = `Sort by ${label}`;
    while (th.firstChild) button.append(th.firstChild);
    th.append(button);
    th.classList.add("sortable");
    th.setAttribute("aria-sort", "none");
    button.addEventListener("click", () => {
      const ascending = th.getAttribute("aria-sort") !== "ascending";
      apply(column, ascending ? "asc" : "desc");
    });
  });

  const saved = recall(table, index);
  if (saved && saved.column < columns.length && columns[saved.column].classList.contains("sortable")) {
    apply(saved.column, saved.direction, false);
  }
}

export function setupTables() {
  [...document.querySelectorAll("table[data-sortable]")].forEach((table, index) => install(table, index));
}
