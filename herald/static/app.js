"use strict";

const state = {
  entries: [],
  sources: [],
  status: "all",
  category: "all",
  search: "",
  selectedId: null,
  stats: { total: 0, statuses: {}, categories: {} },
};

const elements = {};
let toastTimer;

document.addEventListener("DOMContentLoaded", () => {
  Object.assign(elements, {
    list: document.querySelector("#entry-list"),
    empty: document.querySelector("#empty-state"),
    resultCount: document.querySelector("#result-count"),
    title: document.querySelector("#inbox-title"),
    statusNav: document.querySelector("#status-nav"),
    categoryNav: document.querySelector("#category-nav"),
    categorySelect: document.querySelector("#category-select"),
    search: document.querySelector("#search-input"),
    reload: document.querySelector("#reload-button"),
    reader: document.querySelector("#reader-pane"),
    placeholder: document.querySelector("#reader-placeholder"),
    readerContent: document.querySelector("#reader-content"),
    toast: document.querySelector("#toast"),
  });

  document.querySelector("#today").innerHTML = formatToday();
  elements.statusNav.addEventListener("click", changeStatusFilter);
  elements.categoryNav.addEventListener("click", changeCategoryFromButton);
  elements.categorySelect.addEventListener("change", changeCategoryFromSelect);
  elements.search.addEventListener("input", (event) => {
    state.search = event.target.value.trim().toLowerCase();
    renderList();
  });
  elements.reload.addEventListener("click", refreshFeeds);
  document.querySelector("#clear-filters").addEventListener("click", clearFilters);
  document.querySelector("#read-action").addEventListener("click", toggleRead);
  document.querySelector("#keep-action").addEventListener("click", () => actOnSelected("keep"));
  document.querySelector("#discard-action").addEventListener("click", () => actOnSelected("discard"));
  document.querySelector("#summarize-action").addEventListener("click", summarizeSelected);
  document.querySelector("#export-action").addEventListener("click", exportSelected);
  document.querySelector("#mobile-back").addEventListener("click", () => elements.reader.classList.remove("mobile-open"));
  window.addEventListener("hashchange", selectEntryFromHash);
  document.addEventListener("keydown", handleKeyboard);
  loadData();
});

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: options.body ? { "Content-Type": "application/json", ...(options.headers || {}) } : options.headers,
  });
  let payload;
  try { payload = await response.json(); } catch { payload = null; }
  if (!response.ok) throw new Error(payload?.error || `Request failed (${response.status})`);
  return payload;
}

async function loadData() {
  elements.reload.classList.add("loading");
  elements.reload.disabled = true;
  try {
    const params = new URLSearchParams({ limit: "500" });
    if (state.status !== "all") params.set("status", state.status);
    if (state.category !== "all") params.set("category", state.category);
    const [entries, sources, stats] = await Promise.all([
      api(`/api/entries?${params}`),
      api("/api/sources"),
      api("/api/stats"),
    ]);
    state.entries = entries;
    state.sources = sources;
    state.stats = stats;
    renderCategories();
    renderCounts();
    renderList();
    const linkedEntryId = entryIdFromHash();
    if (linkedEntryId && state.entries.some((entry) => entry.id === linkedEntryId)) {
      selectEntry(linkedEntryId, false);
    } else if (state.selectedId && state.entries.some((entry) => entry.id === state.selectedId)) {
      renderReader();
    } else if (state.selectedId) {
      state.selectedId = null;
      renderReader();
    }
  } catch (error) {
    showToast(error.message, true);
    elements.list.innerHTML = `<div class="empty-state"><h2>Could not load the inbox</h2><p>${escapeHtml(error.message)}</p></div>`;
  } finally {
    elements.reload.classList.remove("loading");
    elements.reload.disabled = false;
  }
}

function filteredEntries() {
  return state.entries.filter((entry) => {
    if (state.status !== "all" && entry.status !== state.status) return false;
    if (state.category !== "all" && entry.source_category !== state.category) return false;
    if (state.search) {
      const haystack = [entry.title, entry.summary, entry.author, entry.source_title, entry.source_category].join(" ").toLowerCase();
      if (!haystack.includes(state.search)) return false;
    }
    return true;
  });
}

function renderList() {
  const entries = filteredEntries();
  elements.resultCount.textContent = `${entries.length} ${entries.length === 1 ? "entry" : "entries"}`;
  elements.title.textContent = filterTitle();
  elements.list.hidden = entries.length === 0;
  elements.empty.hidden = entries.length !== 0;
  elements.list.innerHTML = entries.map((entry) => `
    <a class="entry-card ${entry.status === "unread" ? "unread" : ""} ${entry.id === state.selectedId ? "selected" : ""}"
      id="entry-${entry.id}" href="#entry-${entry.id}" data-entry-id="${entry.id}" aria-current="${entry.id === state.selectedId ? "true" : "false"}">
      <span class="card-top"><span class="card-category">${escapeHtml(entry.source_category)}</span><time>${escapeHtml(relativeDate(entry.published_at))}</time></span>
      <h2>${escapeHtml(entry.title)}</h2>
      <span class="card-summary">${escapeHtml(entry.summary || entry.content || "No summary yet.")}</span>
      <span class="card-foot"><span>${escapeHtml(entry.source_title)}</span>${entry.status !== "unread" ? `<span class="status-mini ${entry.status}">${escapeHtml(entry.status)}</span>` : ""}<span class="open-cue">Open →</span></span>
    </a>`).join("");
}

function renderCategories() {
  const categories = [...new Set([
    ...state.sources.map((source) => source.category),
    ...state.entries.map((entry) => entry.source_category),
  ])].filter(Boolean).sort((a, b) => a.localeCompare(b));
  if (state.category !== "all" && !categories.includes(state.category)) state.category = "all";
  elements.categoryNav.innerHTML = categories.map((category) => `
    <button class="${state.category === category ? "active" : ""}" data-category="${escapeAttribute(category)}" type="button">
      <span class="topic-dot"></span><span>${escapeHtml(category)}</span>
    </button>`).join("");
  elements.categorySelect.innerHTML = `<option value="all">All topics</option>` + categories.map((category) =>
    `<option value="${escapeAttribute(category)}" ${state.category === category ? "selected" : ""}>${escapeHtml(category)}</option>`).join("");
}

function renderCounts() {
  ["all", "unread", "read", "kept", "discarded"].forEach((status) => {
    const count = status === "all" ? state.stats.total : (state.stats.statuses[status] || 0);
    document.querySelector(`[data-count="${status}"]`).textContent = count;
  });
}

function selectEntry(id, updateHash = true) {
  if (!state.entries.some((entry) => entry.id === id)) return;
  state.selectedId = id;
  if (updateHash && window.location.hash !== `#entry-${id}`) {
    window.history.replaceState(null, "", `#entry-${id}`);
  }
  renderList();
  renderReader();
  elements.reader.classList.add("mobile-open");
}

function entryIdFromHash() {
  const match = window.location.hash.match(/^#entry-(\d+)$/);
  return match ? Number(match[1]) : null;
}

function selectEntryFromHash() {
  const entryId = entryIdFromHash();
  if (entryId) selectEntry(entryId, false);
}

function renderReader() {
  const entry = selectedEntry();
  if (!entry) {
    elements.placeholder.hidden = false;
    elements.readerContent.hidden = true;
    return;
  }
  elements.placeholder.hidden = true;
  elements.readerContent.hidden = false;
  setText("#reader-category", entry.source_category);
  setText("#reader-source", entry.source_title);
  setText("#reader-title", entry.title);
  setText("#reader-author", entry.author || "Unknown author");
  const date = document.querySelector("#reader-date");
  date.textContent = longDate(entry.published_at);
  date.dateTime = entry.published_at || "";
  setText("#reader-summary", entry.summary || "A summary has not been generated for this entry yet.");
  setText("#reader-excerpt", entry.content || "The feed did not provide an article excerpt.");
  const link = document.querySelector("#reader-link");
  link.href = entry.url;
  const status = document.querySelector("#reader-status");
  status.textContent = entry.status;
  status.className = `status-chip ${entry.status}`;
  const readButton = document.querySelector("#read-action");
  readButton.textContent = entry.status === "unread" ? "✓ Mark read" : "○ Mark unread";
  const keepButton = document.querySelector("#keep-action");
  const discardButton = document.querySelector("#discard-action");
  keepButton.classList.toggle("active", entry.status === "kept");
  discardButton.classList.toggle("active", entry.status === "discarded");
  const exportButton = document.querySelector("#export-action");
  exportButton.disabled = entry.status !== "kept";
  exportButton.title = entry.status === "kept" ? "Write this note to your vault" : "Keep the entry before exporting";
  exportButton.dataset.exported = entry.exported_path ? "true" : "false";
}

async function refreshFeeds() {
  elements.reload.classList.add("loading");
  elements.reload.disabled = true;
  try {
    const result = await api("/api/refresh", { method: "POST", body: "{}" });
    await loadData();
    const message = result.errors
      ? `Added ${result.created} entries; ${result.errors} sources could not be reached`
      : `Added ${result.created} new entries`;
    showToast(message, result.errors > 0 && result.created === 0);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    elements.reload.classList.remove("loading");
    elements.reload.disabled = false;
  }
}

async function summarizeSelected() {
  const entry = selectedEntry();
  if (!entry) return;
  const button = document.querySelector("#summarize-action");
  button.disabled = true;
  button.textContent = "Working…";
  try {
    const result = await api(`/api/entries/${entry.id}/summarize`, { method: "POST", body: "{}" });
    state.entries = state.entries.map((item) => item.id === result.entry.id ? result.entry : item);
    renderList();
    renderReader();
    showToast(result.provider === "ollama" ? "Summary generated locally" : "Summary generated with offline fallback");
  } catch (error) {
    showToast(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "Generate";
  }
}

async function exportSelected() {
  const entry = selectedEntry();
  if (!entry) return;
  const button = document.querySelector("#export-action");
  button.disabled = true;
  try {
    const result = await api(`/api/entries/${entry.id}/export`, { method: "POST", body: "{}" });
    state.entries = state.entries.map((item) => item.id === result.entry.id ? result.entry : item);
    renderReader();
    showToast(`Exported to ${result.entry.exported_path}`);
  } catch (error) {
    showToast(error.message, true);
  } finally {
    button.disabled = selectedEntry()?.status !== "kept";
  }
}

async function actOnSelected(action) {
  const entry = selectedEntry();
  if (!entry) return;
  try {
    const updated = await api(`/api/entries/${entry.id}/action`, { method: "POST", body: JSON.stringify({ action }) });
    state.entries = state.entries.map((item) => item.id === updated.id ? updated : item);
    renderCounts();
    renderList();
    renderReader();
    showToast(action === "keep" ? "Saved to your kept reading" : action === "discard" ? "Moved to discarded" : action === "read" ? "Marked as read" : "Returned to unread");
  } catch (error) { showToast(error.message, true); }
}

function toggleRead() {
  const entry = selectedEntry();
  if (entry) actOnSelected(entry.status === "unread" ? "read" : "unread");
}

function changeStatusFilter(event) {
  const button = event.target.closest("[data-status]");
  if (!button) return;
  state.status = button.dataset.status;
  elements.statusNav.querySelectorAll("[data-status]").forEach((item) => item.classList.toggle("active", item === button));
  loadData();
}

function changeCategoryFromButton(event) {
  const button = event.target.closest("[data-category]");
  if (!button) return;
  setCategory(button.dataset.category === state.category ? "all" : button.dataset.category);
}

function changeCategoryFromSelect(event) { setCategory(event.target.value); }

function setCategory(category) {
  state.category = category;
  elements.categorySelect.value = category;
  elements.categoryNav.querySelectorAll("[data-category]").forEach((button) => button.classList.toggle("active", button.dataset.category === category));
  loadData();
}

function clearFilters() {
  state.status = "all";
  state.category = "all";
  state.search = "";
  elements.search.value = "";
  elements.categorySelect.value = "all";
  elements.statusNav.querySelectorAll("[data-status]").forEach((button) => button.classList.toggle("active", button.dataset.status === "all"));
  elements.categoryNav.querySelectorAll("[data-category]").forEach((button) => button.classList.remove("active"));
  loadData();
}

function handleKeyboard(event) {
  if (event.key === "/" && document.activeElement !== elements.search) {
    event.preventDefault();
    elements.search.focus();
    return;
  }
  if (["INPUT", "SELECT", "TEXTAREA"].includes(document.activeElement?.tagName)) return;
  if (!["j", "k"].includes(event.key.toLowerCase())) return;
  const entries = filteredEntries();
  if (!entries.length) return;
  const current = entries.findIndex((entry) => entry.id === state.selectedId);
  const delta = event.key.toLowerCase() === "j" ? 1 : -1;
  const next = current < 0 ? 0 : Math.max(0, Math.min(entries.length - 1, current + delta));
  selectEntry(entries[next].id);
  document.querySelector(`[data-entry-id="${entries[next].id}"]`)?.scrollIntoView({ block: "nearest" });
}

function selectedEntry() { return state.entries.find((entry) => entry.id === state.selectedId); }
function filterTitle() {
  if (state.category !== "all") return state.category;
  return { all: "All entries", unread: "Unread", read: "Read", kept: "Kept", discarded: "Discarded" }[state.status];
}
function setText(selector, value) { document.querySelector(selector).textContent = value || ""; }
function escapeHtml(value) { const node = document.createElement("div"); node.textContent = String(value ?? ""); return node.innerHTML; }
function escapeAttribute(value) { return escapeHtml(value).replaceAll('"', "&quot;"); }

function showToast(message, error = false) {
  clearTimeout(toastTimer);
  elements.toast.textContent = message;
  elements.toast.className = `toast show${error ? " error" : ""}`;
  toastTimer = setTimeout(() => { elements.toast.className = "toast"; }, 2600);
}

function relativeDate(value) {
  if (!value) return "Recently";
  const date = new Date(value);
  const days = Math.floor((Date.now() - date.getTime()) / 86400000);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  if (days < 7) return `${days} days ago`;
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(date);
}
function longDate(value) {
  if (!value) return "Date unavailable";
  return new Intl.DateTimeFormat(undefined, { year: "numeric", month: "long", day: "numeric" }).format(new Date(value));
}
function formatToday() {
  const date = new Date();
  return `<strong>${new Intl.DateTimeFormat(undefined, { weekday: "long" }).format(date)}</strong>${new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(date)}`;
}
