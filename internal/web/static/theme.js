// Applies the saved theme before the page paints, so a dark theme does not
// flash light. Storage can be unavailable, in which case the system setting wins.
(function () {
  try {
    var theme = localStorage.getItem("raedar-theme");
    if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
  } catch (_) { /* the system setting applies */ }
})();
