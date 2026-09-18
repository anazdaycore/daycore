// Options: the Daycore server URL + import token, and custom Canvas domains.
// Both need an optional host permission grant — the Canvas origins so
// background fetches carry the user's session cookies, and the Daycore origin
// so the popup's POST /api/import/canvas is not blocked by CORS (an extension
// page fetch only bypasses CORS for origins the user has granted).

const t = (key, subs) => chrome.i18n.getMessage(key, subs) || key;

document.querySelectorAll("[data-i18n]").forEach((el) => {
  el.textContent = t(el.dataset.i18n);
});

const urlEl = document.getElementById("daycore-url");
const tokenEl = document.getElementById("import-token");
const domainsEl = document.getElementById("domains");
const savedEl = document.getElementById("saved");
const saveBtn = document.getElementById("save");
const testBtn = document.getElementById("test");

const DEFAULT_URL = DAYCORE_DEFAULT_URL; // single source: config.js

chrome.storage.sync
  .get({ daycoreURL: DEFAULT_URL, importToken: "", customDomains: [] })
  .then(({ daycoreURL, importToken, customDomains }) => {
    urlEl.value = daycoreURL;
    tokenEl.value = importToken;
    domainsEl.value = customDomains.join("\n");
  });

function setStatus(text, cls) {
  savedEl.textContent = text;
  savedEl.className = cls || "";
}

// normalizeURL accepts "day.example.com", "https://day.example.com/api/" and
// the like, returning a bare origin (no trailing slash, no path) or null.
// A bare host defaults to https — a deployed Daycore should not be plain http.
function normalizeURL(raw) {
  let s = (raw || "").trim();
  if (!s) return null;
  if (!/^https?:\/\//i.test(s)) s = `https://${s}`;
  try {
    const u = new URL(s);
    if (u.protocol !== "http:" && u.protocol !== "https:") return null;
    return u.origin;
  } catch (_e) {
    return null;
  }
}

function requestOrigin(pattern) {
  return chrome.permissions.request({ origins: [pattern] }).catch(() => false);
}

saveBtn.addEventListener("click", async () => {
  const daycoreURL = normalizeURL(urlEl.value) || DEFAULT_URL;
  urlEl.value = daycoreURL; // show the user exactly what gets stored

  const domains = domainsEl.value
    .split("\n")
    .map((d) => d.trim().replace(/^https?:\/\//, "").replace(/\/.*$/, ""))
    .filter(Boolean);

  // Request the Daycore origin too — without it the push fails on CORS, by far
  // the most confusing failure mode once Daycore lives on a real domain.
  const denied = [];
  if (!(await requestOrigin(`${daycoreURL}/*`))) denied.push(daycoreURL);
  for (const host of domains) {
    if (!(await requestOrigin(`*://${host}/*`))) denied.push(host);
  }

  await chrome.storage.sync.set({
    daycoreURL,
    importToken: tokenEl.value.trim(),
    customDomains: domains,
  });

  setStatus(denied.length ? t("optPermDenied", [denied.join(", ")]) : t("optSaved"),
    denied.length ? "error" : "");
});

// Test hits the public GET /api/healthz, so the user learns immediately whether
// the URL, TLS, and host permission are all in order — separately from whether
// the import token is valid.
testBtn.addEventListener("click", async () => {
  const daycoreURL = normalizeURL(urlEl.value);
  if (!daycoreURL) {
    setStatus(t("optTestBadURL"), "error");
    return;
  }
  urlEl.value = daycoreURL;

  const pattern = `${daycoreURL}/*`;
  const has = await chrome.permissions.contains({ origins: [pattern] }).catch(() => false);
  if (!has && !(await requestOrigin(pattern))) {
    setStatus(t("optPermDenied", [daycoreURL]), "error");
    return;
  }

  saveBtn.disabled = testBtn.disabled = true;
  setStatus(t("optTesting"), "pending");
  try {
    const res = await fetch(`${daycoreURL}/api/healthz`, { headers: { Accept: "application/json" } });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const body = await res.json();
    if (!body.ok) throw new Error(body.error || "not healthy");
    setStatus(t("optTestOK", [String(body.version || "?"), String(body.db || "?")]), "");
  } catch (err) {
    setStatus(t("optTestErr", [String(err.message || err)]), "error");
  } finally {
    saveBtn.disabled = testBtn.disabled = false;
  }
});
