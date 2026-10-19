// Entry point: each page draws only the parts it contains.
import { drawAppCharts, drawHostCharts, drawIncidentChart, drawSparklines } from "./pages.js";
import { drawPatterns } from "./patterns.js";
import { setupLogs } from "./logs.js";

// Selects that carry data-autosubmit reload their page when changed.
for (const select of document.querySelectorAll("select[data-autosubmit]")) {
  select.addEventListener("change", () => select.form.submit());
}

drawSparklines();
setupLogs();
drawAppCharts();
drawHostCharts();
drawIncidentChart();
drawPatterns();
