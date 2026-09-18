// Popup: drives the fetch (delegated to the service worker), then builds the
// JSON/CSV downloads locally and optionally pushes straight to Daycore — local
// or a deployed domain — via POST /api/import/canvas with the X-Import-Token
// header. The target origin must be granted in Options first (see options.js).

const t = (key, subs) => chrome.i18n.getMessage(key, subs) || key;

const statusEl = document.getElementById("status");
const fetchBtn = document.getElementById("fetch");
const jsonBtn = document.getElementById("export-json");
const csvBtn = document.getElementById("export-csv");
const pushBtn = document.getElementById("push");

let exportData = null;

document.querySelectorAll("[data-i18n]").forEach((el) => {
  el.textContent = t(el.dataset.i18n);
});
document.getElementById("open-options").addEventListener("click", (e) => {
  e.preventDefault();
  chrome.runtime.openOptionsPage();
});

function setStatus(text, cls) {
  statusEl.textContent = text;
  statusEl.className = cls || "";
}

async function canvasBaseURL() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab || !tab.url) return null;
  let origin;
  try {
    origin = new URL(tab.url).origin;
  } catch (_e) {
    return null;
  }
  if (/https?:\/\/[^/]*\.instructure\.com$/.test(origin)) return origin;
  const { customDomains = [] } = await chrome.storage.sync.get("customDomains");
  const host = new URL(origin).hostname;
  if (customDomains.includes(host)) return origin;
  return null;
}

fetchBtn.addEventListener("click", async () => {
  const baseURL = await canvasBaseURL();
  if (!baseURL) {
    setStatus(t("statusNotCanvas"), "error");
    return;
  }
  setStatus(t("statusFetching"));
  fetchBtn.disabled = true;
  const res = await chrome.runtime.sendMessage({ type: "collect", baseURL });
  fetchBtn.disabled = false;
  if (!res || !res.ok) {
    setStatus(t("statusError", [res ? res.error : "no response"]), "error");
    return;
  }
  exportData = res.data;
  setStatus(t("statusDone", [String(exportData.courses.length), String(exportData.assignments.length)]), "ok");
  jsonBtn.disabled = csvBtn.disabled = pushBtn.disabled = false;
});

function download(filename, mime, content) {
  const url = URL.createObjectURL(new Blob([content], { type: mime }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 5000);
}

const stamp = () => new Date().toISOString().slice(0, 10);

jsonBtn.addEventListener("click", () => {
  if (!exportData) return;
  download(`daycore-canvas-${stamp()}.json`, "application/json", JSON.stringify(exportData, null, 2));
});

csvBtn.addEventListener("click", () => {
  if (!exportData) return;
  download(`daycore-assignments-${stamp()}.csv`, "text/csv",
    toCSV(
      ["title", "course", "dueAt", "points", "submitted", "graded", "score", "url"],
      exportData.assignments.map((a) => [
        a.title, courseName(a.courseCanvasId), a.dueAt || "", a.pointsPossible ?? "",
        a.submitted, a.graded, a.score ?? "", a.htmlUrl,
      ])
    ));
  download(`daycore-grades-${stamp()}.csv`, "text/csv",
    toCSV(
      ["course", "courseCode", "currentScore", "currentGrade"],
      exportData.courses.map((c) => [c.name, c.courseCode, c.currentScore ?? "", c.currentGrade ?? ""])
    ));
});

function courseName(canvasId) {
  const c = exportData.courses.find((x) => x.canvasId === canvasId);
  return c ? c.name : "";
}

function toCSV(header, rows) {
  const esc = (v) => {
    const s = String(v ?? "");
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  return [header, ...rows].map((r) => r.map(esc).join(",")).join("\n") + "\n";
}

pushBtn.addEventListener("click", async () => {
  if (!exportData) return;
  const { daycoreURL = DAYCORE_DEFAULT_URL, importToken = "" } =
    await chrome.storage.sync.get(["daycoreURL", "importToken"]);
  if (!importToken) {
    setStatus(t("pushNoToken"), "error");
    return;
  }

  const base = daycoreURL.replace(/\/+$/, "");
  let origin;
  try {
    origin = new URL(base).origin;
  } catch (_e) {
    setStatus(t("pushBadURL"), "error");
    return;
  }

  // Without a host permission for this origin the fetch dies on CORS with a
  // opaque "Failed to fetch" — check first and point at Options instead.
  const granted = await chrome.permissions
    .contains({ origins: [`${origin}/*`] })
    .catch(() => false);
  if (!granted) {
    setStatus(t("pushNoPerm", [origin]), "error");
    return;
  }

  pushBtn.disabled = true;
  try {
    const res = await fetch(`${base}/api/v2/import/canvas`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Import-Token": importToken },
      body: JSON.stringify(exportData),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(body.message || `HTTP ${res.status}`);
    }
    setStatus(t("pushOK", [String(body.courses ?? 0), String(body.assignments ?? 0)]), "ok");
  } catch (err) {
    setStatus(t("pushErr", [String(err.message || err)]), "error");
  } finally {
    pushBtn.disabled = false;
  }
});
