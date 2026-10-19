// Calls to the JSON API.

async function getJSON(path, params) {
  const response = await fetch(`${path}?${params}`);
  if (!response.ok) throw new Error(`${path}: ${(await response.text()).trim() || response.status}`);
  return response.json();
}

// seriesRef builds the reference of a series: a metric of a subject. The subject
// is the app, route or disk name, the slot ID for a slot, and empty for the host.
export const seriesRef = (kind, subject, metric) => [kind, subject, metric].map(encodeURIComponent).join("/");

// fetchSeries reads series by reference for a range of Unix seconds.
export async function fetchSeries(refs, from, to) {
  const params = new URLSearchParams({ from: String(from), to: String(to) });
  for (const ref of refs) params.append("series", ref);
  return (await getJSON("/api/series", params)).series;
}

export const fetchPatterns = (series, weeks) => getJSON("/api/patterns", new URLSearchParams({ series, weeks }));
