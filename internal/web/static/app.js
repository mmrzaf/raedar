// Entry point: each page draws only the parts it contains.
import { drawAppCharts, drawHostCharts, drawIncidentChart, drawSparklines } from "./pages.js";
import { drawPatterns } from "./patterns.js";
import { setupLogs } from "./logs.js";

// The theme button cycles system -> light -> dark. Charts take their colors
// when they are drawn, so a change reloads the page.
const themeButton = document.querySelector(".theme-toggle");
if (themeButton) {
  const order = [null, "light", "dark"];
  themeButton.addEventListener("click", () => {
    const current = document.documentElement.dataset.theme || null;
    const next = order[(order.indexOf(current) + 1) % order.length];
    try {
      if (next) localStorage.setItem("raedar-theme", next);
      else localStorage.removeItem("raedar-theme");
    } catch (_) { /* the choice lasts until the reload only */ }
    if (next) document.documentElement.dataset.theme = next;
    else delete document.documentElement.dataset.theme;
    location.reload();
  });
  themeButton.title = `Theme: ${document.documentElement.dataset.theme || "system"}`;
}

// Selects that carry data-autosubmit reload their page when changed.
for (const select of document.querySelectorAll("select[data-autosubmit]")) {
  select.addEventListener("change", () => select.form.submit());
}

// A page that asks for it (the overview) reloads itself while it is visible.
const refresh = Number(document.querySelector("[data-refresh]")?.dataset.refresh);
if (refresh > 0) {
  setInterval(() => { if (!document.hidden) location.reload(); }, refresh * 1000);
}

drawSparklines();
setupLogs();
drawAppCharts();
drawHostCharts();
drawIncidentChart();
drawPatterns();
