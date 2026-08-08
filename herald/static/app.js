"use strict";

const PAGE_SIZE = 50;
const state = {
  activeKind: "paper",
  entries: [],
  sources: [],
  status: "unread",
  bucket: "relevant",
  category: "all",
  search: "",
  selectedId: null,
  selectedDetail: null,
  references: [],
  nextCursor: null,
  profile: null,
  relevanceHealth: null,
  stats: { total: 0, statuses: {}, categories: {}, relevance: {} },
};

const elements = {};
let toastTimer;
let detailRequest = 0;
let rescorePollTimer;
let searchTimer;

document.addEventListener("DOMContentLoaded", () => {
  Object.assign(elements, {
    list: document.querySelector("#entry-list"),
    empty: document.querySelector("#empty-state"),
    resultCount: document.querySelector("#result-count"),
    title: document.querySelector("#inbox-title"),
    statusNav: document.querySelector("#status-nav"),
    kindNav: document.querySelector("#kind-nav"),
    relevanceNav: document.querySelector("#relevance-nav"),
    categoryNav: document.querySelector("#category-nav"),
    categorySelect: document.querySelector("#category-select"),
    search: document.querySelector("#search-input"),
    reload: document.querySelector("#reload-button"),
    reader: document.querySelector("#reader-pane"),
    readerContent: document.querySelector("#reader-content"),
    toast: document.querySelector("#toast"),
    rankingBanner: document.querySelector("#ranking-banner"),
    pagination: document.querySelector("#pagination"),
    loadMore: document.querySelector("#load-more"),
    profileDialog: document.querySelector("#profile-dialog"),
    importDialog: document.querySelector("#import-dialog"),
  });

  document.querySelector("#today").innerHTML = formatToday();
  elements.kindNav.addEventListener("click", changeWorkspace);
  elements.statusNav.addEventListener("click", changeStatusFilter);
  elements.relevanceNav.addEventListener("click", changeBucketFilter);
  elements.list.addEventListener("click", previewEntryFromEvent);
  elements.list.addEventListener("dblclick", openPaperFromEvent);
  elements.categoryNav.addEventListener("click", changeCategoryFromButton);
  elements.categorySelect.addEventListener("change", changeCategoryFromSelect);
  elements.search.addEventListener("input", handleSearch);
  elements.reload.addEventListener("click", refreshFeeds);
  elements.loadMore.addEventListener("click", loadMoreEntries);
  document.querySelector("#clear-filters").addEventListener("click", clearFilters);
  document.querySelector("#read-action").addEventListener("click", toggleRead);
  document.querySelector("#keep-action").addEventListener("click", () => actOnSelected("keep"));
  document.querySelector("#discard-action").addEventListener("click", () => actOnSelected("discard"));
  document.querySelector("#summarize-action").addEventListener("click", summarizeSelected);
  document.querySelector("#export-action").addEventListener("click", exportSelected);
  document.querySelector("#profile-button").addEventListener("click", openProfile);
  document.querySelector("#import-button").addEventListener("click", openImport);
  document.querySelector("#profile-form").addEventListener("submit", saveProfile);
  document.querySelector("#rescore-button").addEventListener("click", rescorePapers);
  document.querySelector("#import-form").addEventListener("submit", importPaper);
  document.querySelector("#reference-list").addEventListener("click", addReferenceFromEvent);
  document.querySelectorAll("[data-close-dialog]").forEach((button) => button.addEventListener("click", () => {
    document.querySelector(`#${button.dataset.closeDialog}`).close();
  }));
  document.querySelector("#mobile-back").addEventListener("click", () => elements.reader.classList.remove("mobile-open"));
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

function entriesPath(cursor = null) {
  const params = new URLSearchParams({ kind: state.activeKind, limit: String(PAGE_SIZE) });
  if (state.bucket) params.set("bucket", state.bucket);
  if (state.status !== "all") params.set("status", state.status);
  if (state.category !== "all") params.set("category", state.category);
  if (state.search) params.set("q", state.search);
  if (cursor) params.set("cursor", cursor);
  return `/api/entries?${params}`;
}

async function loadData({ preserveSelection = true } = {}) {
  setLoading(true);
  try {
    const [page, sources, stats, profile, health] = await Promise.all([
      api(entriesPath()),
      api("/api/sources"),
      api("/api/stats"),
      api(`/api/profiles/${state.activeKind}`),
      api("/api/relevance/health"),
    ]);
    state.entries = page.entries;
    state.nextCursor = page.next_cursor;
    state.sources = sources;
    state.stats = stats;
    state.profile = profile;
    state.relevanceHealth = health;
    renderChrome();
    renderCategories();
    renderCounts();
    renderSourceHealth();
    renderNavigation();
    renderRankingBanner();
    renderList();
    if (!preserveSelection || !state.selectedId || !state.entries.some((entry) => entry.id === state.selectedId)) clearSelection();
    else renderReader();
  } catch (error) {
    showToast(error.message, true);
    elements.list.hidden = false;
    elements.list.innerHTML = `<div class="empty-state"><h2>Could not load the inbox</h2><p>${escapeHtml(error.message)}</p></div>`;
  } finally {
    setLoading(false);
  }
}

async function loadMoreEntries() {
  if (!state.nextCursor) return;
  const button = elements.loadMore;
  button.disabled = true;
  button.textContent = "Loading…";
  try {
    const page = await api(entriesPath(state.nextCursor));
    const known = new Set(state.entries.map((entry) => entry.id));
    state.entries.push(...page.entries.filter((entry) => !known.has(entry.id)));
    state.nextCursor = page.next_cursor;
    renderList();
  } catch (error) { showToast(error.message, true); }
  finally {
    button.disabled = false;
    button.textContent = "Load more";
  }
}

function setLoading(loading) {
  elements.reload.classList.toggle("loading", loading);
  elements.reload.disabled = loading;
}

function filteredEntries() {
  return state.entries.filter((entry) => {
    if (!state.search) return true;
    const ranking = rankingFor(entry);
    const haystack = [
      entry.title, entry.summary, entry.content, entry.author,
      entry.source_title, entry.source_category,
      ...(ranking?.explanation?.matched_interests || []),
    ].join(" ").toLowerCase();
    return haystack.includes(state.search);
  });
}

function handleSearch(event) {
  state.search = event.target.value.trim().toLowerCase();
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => loadData({ preserveSelection: false }), 180);
}

function renderList() {
  const entries = filteredEntries();
  const loadedLabel = state.search ? `${entries.length} matching loaded` : `${state.entries.length} loaded`;
  elements.resultCount.textContent = loadedLabel;
  elements.title.textContent = filterTitle();
  setText("#sort-label", state.bucket ? "Highest score first" : "Newest first");
  elements.list.hidden = entries.length === 0;
  elements.empty.hidden = entries.length !== 0;
  elements.pagination.hidden = !state.nextCursor;
  elements.list.innerHTML = entries.map((entry) => {
    const ranking = rankingFor(entry);
    const reasons = relevanceReasons(ranking).slice(0, 3);
    const score = ranking ? `<span class="score-badge ${ranking.bucket || entry.relevance_bucket}">${formatScore(ranking.score)}</span>` : "";
    const reasonMarkup = reasons.length
      ? `<span class="card-reasons">${reasons.map((reason) => `<span>${escapeHtml(reason)}</span>`).join("")}</span>`
      : "";
    const provenance = ranking
      ? `<span class="ranking-mini" title="Scored ${escapeAttribute(longDateTime(ranking.scored_at))}">${escapeHtml(modelLabel(ranking.model))}</span>`
      : "";
    return `
      <button class="entry-card ${entry.content_kind === "news" ? "news-card" : "paper-card"} ${entry.status === "unread" ? "unread" : ""} ${entry.id === state.selectedId ? "selected" : ""}"
        id="entry-${entry.id}" type="button" data-entry-id="${entry.id}" aria-pressed="${entry.id === state.selectedId ? "true" : "false"}"
        title="Single-click to preview; double-click to open ${arxivPdfUrl(entry.url) ? "the PDF" : entry.content_kind === "news" ? "the announcement" : "the original"}">
        <span class="card-top"><span class="card-category">${escapeHtml(entry.source_category)}</span><span class="card-top-right">${score}<time>${escapeHtml(relativeDate(entry.published_at))}</time></span></span>
        <h2>${escapeHtml(entry.title)}</h2>
        <span class="card-summary">${escapeHtml(entry.summary || entry.content || "No summary yet.")}</span>
        ${reasonMarkup}
        <span class="card-foot"><span>${escapeHtml(entry.source_title)}</span>${provenance}${entry.status !== "unread" ? `<span class="status-mini ${entry.status}">${escapeHtml(entry.status)}</span>` : ""}<span class="open-cue">Preview · double-click ${arxivPdfUrl(entry.url) ? "PDF" : entry.content_kind === "news" ? "announcement" : "original"} ↗</span></span>
      </button>`;
  }).join("");
}

function renderCategories() {
  const categories = [...new Set([
    ...state.sources.filter((source) => (source.content_kind || "paper") === state.activeKind).map((source) => source.category),
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
  const workspaceStats = state.stats.workspaces?.[state.activeKind] || {};
  ["all", "unread", "read", "kept", "discarded"].forEach((status) => {
    const count = status === "all" ? (workspaceStats.total || 0) : (workspaceStats.statuses?.[status] || 0);
    document.querySelector(`[data-count="${status}"]`).textContent = count;
  });
  const relevance = state.stats.relevance?.[state.activeKind] || {};
  document.querySelector('[data-count="relevant"]').textContent = relevance.relevant || 0;
  document.querySelector('[data-count="filtered"]').textContent = relevance.filtered || 0;
}

function renderNavigation() {
  elements.kindNav.querySelectorAll("[data-kind]").forEach((button) => {
    const active = button.dataset.kind === state.activeKind;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", String(active));
  });
  elements.statusNav.querySelectorAll("[data-status]").forEach((button) => {
    button.classList.toggle("active", button.dataset.status === state.status);
  });
  elements.relevanceNav.querySelectorAll("[data-bucket]").forEach((button) => {
    button.classList.toggle("active", button.dataset.bucket === state.bucket);
  });
}

function renderChrome() {
  const label = state.activeKind === "paper" ? "Papers" : "News";
  setText("#queue-eyebrow", `${state.activeKind === "paper" ? "Paper" : "News"} queue`);
  setText("#search-label", `Search ${label.toLowerCase()}`);
  elements.search.placeholder = `Search ${label.toLowerCase()}`;
  document.querySelector("#import-button").hidden = state.activeKind !== "paper";
  document.querySelector("#profile-button").title = `Tune ${label.toLowerCase()} relevance`;
}

function renderSourceHealth() {
  const sources = state.sources.filter((source) => (source.content_kind || "paper") === state.activeKind);
  const failing = sources.filter((source) => source.refresh_error);
  setText("#source-health-summary", failing.length ? `${failing.length} issue${failing.length === 1 ? "" : "s"}` : `${sources.length} healthy`);
  document.querySelector("#source-health-list").innerHTML = sources.length
    ? sources.map((source) => `<div class="source-health-item ${source.refresh_error ? "error" : ""}">
        <span>${escapeHtml(source.title)}</span><small>${source.refresh_error ? escapeHtml(source.refresh_error) : source.refresh_succeeded_at ? `Updated ${escapeHtml(relativeDate(source.refresh_succeeded_at))}` : "Awaiting first refresh"}</small>
      </div>`).join("")
    : "<p>No sources configured.</p>";
}

function renderRankingBanner() {
  const job = state.relevanceHealth?.jobs?.[state.activeKind];
  const ranked = state.stats.relevance?.[state.activeKind] || {};
  const pending = ranked.pending || 0;
  if (job?.state === "running") {
    elements.rankingBanner.hidden = false;
    elements.rankingBanner.innerHTML = `<span class="spinner"></span><span><strong>Relevance is updating</strong> Existing scores stay available while Herald works locally.</span>`;
  } else if (job?.state === "failed") {
    elements.rankingBanner.hidden = false;
    elements.rankingBanner.innerHTML = `<span>!</span><span><strong>Rescore failed</strong> ${escapeHtml(job.error || "Unknown ranking error")}</span>`;
  } else if (pending > 0) {
    elements.rankingBanner.hidden = false;
    const noun = state.activeKind === "paper" ? "paper" : "news item";
    elements.rankingBanner.innerHTML = `<span>○</span><span><strong>${pending} ${noun}${pending === 1 ? " is" : "s are"} awaiting a score.</strong> Use Tune filter to rescore.</span>`;
  } else {
    elements.rankingBanner.hidden = true;
    elements.rankingBanner.innerHTML = "";
  }
}

async function selectEntry(id) {
  if (!state.entries.some((entry) => entry.id === id)) return;
  state.selectedId = id;
  state.selectedDetail = null;
  state.references = [];
  syncSelectedCard();
  renderReader(true);
  elements.reader.classList.add("mobile-open");
  const requestId = ++detailRequest;
  try {
    const [entry, references] = await Promise.all([
      api(`/api/entries/${id}`),
      state.activeKind === "paper" ? api(`/api/entries/${id}/references`) : Promise.resolve({ references: [] }),
    ]);
    if (requestId !== detailRequest || state.selectedId !== id) return;
    state.selectedDetail = entry;
    state.references = references.references || [];
    state.entries = state.entries.map((item) => item.id === id ? mergeEntry(item, entry) : item);
    renderList();
    renderReader();
  } catch (error) {
    if (requestId === detailRequest) showToast(error.message, true);
  }
}

function previewEntryFromEvent(event) {
  const card = event.target.closest("[data-entry-id]");
  if (card) selectEntry(Number(card.dataset.entryId));
}

function openPaperFromEvent(event) {
  const card = event.target.closest("[data-entry-id]");
  if (!card) return;
  const entry = state.entries.find((item) => item.id === Number(card.dataset.entryId));
  if (entry) window.open(arxivPdfUrl(entry.url) || entry.url, "_blank", "noopener,noreferrer");
}

function clearSelection() {
  detailRequest += 1;
  state.selectedId = null;
  state.selectedDetail = null;
  state.references = [];
  syncSelectedCard();
  renderReader();
}

function syncSelectedCard() {
  elements.list.querySelectorAll("[data-entry-id]").forEach((card) => {
    const selected = Number(card.dataset.entryId) === state.selectedId;
    card.classList.toggle("selected", selected);
    card.setAttribute("aria-pressed", String(selected));
  });
}

function renderReader(loading = false) {
  const entry = selectedEntry();
  if (!entry) {
    elements.readerContent.hidden = true;
    elements.reader.classList.remove("mobile-open");
    return;
  }
  elements.readerContent.hidden = false;
  elements.readerContent.classList.toggle("detail-loading", loading);
  setText("#reader-category", entry.source_category);
  setText("#reader-source", entry.source_title);
  setText("#reader-title", entry.title);
  setText("#reader-author", entry.author || (entry.content_kind === "news" ? "Official announcement" : "Unknown author"));
  const date = document.querySelector("#reader-date");
  date.textContent = longDate(entry.published_at);
  date.dateTime = entry.published_at || "";
  setText("#reader-fact-date", longDate(entry.published_at));
  setText("#reader-fact-source", entry.source_title);
  setText("#reader-summary", entry.summary || "A summary has not been generated for this entry yet.");
  const provenance = summaryProvenance(entry);
  const provenanceElement = document.querySelector("#reader-summary-provider");
  provenanceElement.textContent = provenance.label;
  provenanceElement.className = `summary-provenance ${provenance.kind}`;
  setText("#reader-excerpt", entry.content || "The feed did not provide an article excerpt.");
  const link = document.querySelector("#reader-link");
  link.href = entry.url;
  const pdfUrl = arxivPdfUrl(entry.url);
  setText("#reader-fact-destination", pdfUrl ? "Direct arXiv PDF" : entry.content_kind === "news" ? "Official announcement" : "Original article");
  const pdfLink = document.querySelector("#reader-pdf");
  pdfLink.hidden = !pdfUrl;
  pdfLink.href = pdfUrl || "#";
  link.innerHTML = pdfUrl ? "View abstract <span>↗</span>" : entry.content_kind === "news" ? "Read announcement <span>↗</span>" : "Read original <span>↗</span>";
  const status = document.querySelector("#reader-status");
  status.textContent = entry.status;
  status.className = `status-chip ${entry.status}`;
  const readButton = document.querySelector("#read-action");
  readButton.textContent = entry.status === "unread" ? "✓ Mark read" : "○ Mark unread";
  document.querySelector("#keep-action").classList.toggle("active", entry.status === "kept");
  document.querySelector("#discard-action").classList.toggle("active", entry.status === "discarded");
  renderRelevance(entry);
  renderMetadata(entry);
  renderReferences(entry, loading);
  renderExport(entry);
}

function renderRelevance(entry) {
  const ranking = rankingFor(entry);
  const section = document.querySelector("#reader-relevance-section");
  section.hidden = !ranking;
  if (!ranking) return;
  setText("#reader-score", `${formatScore(ranking.score)} / 100 · ${sentenceCase(ranking.bucket)} filter`);
  setText("#reader-ranking-provenance", `${modelLabel(ranking.model)} · ${longDateTime(ranking.scored_at)}`);
  const reasons = relevanceReasons(ranking);
  document.querySelector("#reader-reasons").innerHTML = reasons.length
    ? reasons.map((reason) => `<li>${escapeHtml(reason)}</li>`).join("")
    : "<li>No individual match signals were recorded.</li>";
}

function renderMetadata(entry) {
  const panel = document.querySelector(".metadata-panel");
  panel.hidden = false;
  setText("#metadata-heading", entry.content_kind === "news" ? "Announcement metadata" : "Research metadata");
  const enrichment = entry.enrichment_status || "pending";
  const provider = entry.enrichment_provider ? ` via ${entry.enrichment_provider}` : "";
  setText("#reader-enrichment", entry.content_kind === "news" ? "Official source" : `${sentenceCase(enrichment)}${provider}`);
  const enrichmentError = document.querySelector("#reader-enrichment-error");
  enrichmentError.hidden = !entry.enrichment_error;
  enrichmentError.textContent = entry.enrichment_error || "";
  const exportState = entry.obsidian_export?.state || (entry.exported_path ? "synced" : "not synced");
  const exportLabel = exportState === "synced" && entry.exported_path
    ? `Obsidian · ${entry.exported_path}`
    : `Obsidian · ${sentenceCase(exportState)}`;
  setText("#reader-obsidian-state", exportLabel);
}

function renderReferences(entry, loading) {
  const section = document.querySelector("#references-section");
  if (loading) {
    section.hidden = false;
    setText("#reference-count", "Loading references…");
    document.querySelector("#reference-list").innerHTML = "";
    return;
  }
  const isPaper = entry.content_kind === "paper";
  section.hidden = !isPaper || state.references.length === 0;
  if (section.hidden) return;
  setText("#reference-count", `${state.references.length} cited paper${state.references.length === 1 ? "" : "s"}`);
  document.querySelector("#reference-list").innerHTML = state.references.map((reference) => {
    const title = reference.target_title || reference.cited_title || reference.external_id || "Untitled reference";
    const url = reference.target_url || reference.cited_url;
    const identity = reference.external_scheme && reference.external_id
      ? `${reference.external_scheme.toUpperCase()} · ${reference.external_id}`
      : reference.provider || "External reference";
    const inHerald = Boolean(reference.cited_entry_id);
    return `<article class="reference-item">
      <div><strong>${escapeHtml(title)}</strong><small>${escapeHtml(identity)}${inHerald ? ` · In Herald (${escapeHtml(reference.cited_status || "unread")})` : ""}</small></div>
      <div class="reference-actions">
        ${url ? `<a href="${escapeAttribute(url)}" target="_blank" rel="noopener noreferrer">Open ↗</a>` : ""}
        ${inHerald ? `<button type="button" data-preview-cited="${reference.cited_entry_id}">In Herald</button>` : `<button type="button" data-add-reference="${reference.id}">Add to Herald</button>`}
      </div>
    </article>`;
  }).join("");
}

function renderExport(entry) {
  const button = document.querySelector("#export-action");
  const exportState = entry.obsidian_export?.state || (entry.exported_path ? "synced" : null);
  button.disabled = entry.status !== "kept";
  button.textContent = entry.status !== "kept"
    ? "Keep to save in Obsidian"
    : exportState === "synced"
      ? "Synced to Obsidian"
      : exportState === "conflict"
        ? "Resolve or retry Obsidian"
        : "Retry Obsidian sync";
  button.title = entry.status === "kept"
    ? (entry.obsidian_export?.error || "Reconcile this note with your vault")
    : "Keeping an entry saves it to Obsidian automatically";
  button.dataset.exported = exportState === "synced" ? "true" : "false";
}

async function refreshFeeds() {
  setLoading(true);
  try {
    const result = await api("/api/refresh", { method: "POST", body: "{}" });
    await loadData();
    const message = result.errors
      ? `Added ${result.created} entries; ${result.errors} sources could not be reached`
      : `Added ${result.created} new entries`;
    showToast(message, result.errors > 0 && result.created === 0);
  } catch (error) { showToast(error.message, true); }
  finally { setLoading(false); }
}

async function summarizeSelected() {
  const entry = selectedEntry();
  if (!entry) return;
  const button = document.querySelector("#summarize-action");
  button.disabled = true;
  button.textContent = "Working…";
  try {
    const result = await api(`/api/entries/${entry.id}/summarize`, { method: "POST", body: "{}" });
    updateSelected(mergeEntry(entry, result.entry));
    showToast(result.provider === "ollama" ? "Summary generated locally" : "Summary generated with offline fallback");
  } catch (error) { showToast(error.message, true); }
  finally {
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
    const result = await api(`/api/entries/${entry.id}/obsidian/retry`, { method: "POST", body: "{}" });
    updateSelected(mergeEntry(entry, result.entry));
    showToast(result.entry.exported_path ? `Synced to ${result.entry.exported_path}` : "Obsidian operation completed");
  } catch (error) { showToast(error.message, true); }
  finally { button.disabled = selectedEntry()?.status !== "kept"; }
}

async function actOnSelected(action) {
  const entry = selectedEntry();
  if (!entry) return;
  try {
    const updated = await api(`/api/entries/${entry.id}/action`, { method: "POST", body: JSON.stringify({ action }) });
    updateSelected(mergeEntry(entry, updated));
    const syncState = updated.obsidian_export?.state;
    const message = action === "keep"
      ? (syncState === "synced" ? "Kept and synced to Obsidian" : "Kept; Obsidian sync needs attention")
      : action === "discard" ? "Moved to discarded" : action === "read" ? "Marked as read" : "Returned to unread";
    showToast(message, action === "keep" && !["synced", "pending"].includes(syncState));
    await refreshStats();
    if (state.status !== "all" && updated.status !== state.status) {
      state.entries = state.entries.filter((item) => item.id !== updated.id);
      clearSelection();
      renderList();
    }
  } catch (error) { showToast(error.message, true); }
}

function updateSelected(updated) {
  state.selectedDetail = mergeEntry(state.selectedDetail || {}, updated);
  state.entries = state.entries.map((item) => item.id === updated.id ? mergeEntry(item, updated) : item);
  renderList();
  renderReader();
}

async function refreshStats() {
  try {
    state.stats = await api("/api/stats");
    renderCounts();
  } catch { /* The action succeeded; stale counts are safer than a false error. */ }
}

function toggleRead() {
  const entry = selectedEntry();
  if (entry) actOnSelected(entry.status === "unread" ? "read" : "unread");
}

function changeStatusFilter(event) {
  const button = event.target.closest("[data-status]");
  if (!button) return;
  state.status = button.dataset.status;
  state.search = "";
  elements.search.value = "";
  loadData({ preserveSelection: false });
}

function changeWorkspace(event) {
  const button = event.target.closest("[data-kind]");
  if (!button || button.dataset.kind === state.activeKind) return;
  state.activeKind = button.dataset.kind;
  state.status = "unread";
  state.bucket = "relevant";
  state.category = "all";
  state.search = "";
  elements.search.value = "";
  clearSelection();
  loadData({ preserveSelection: false });
}

function changeBucketFilter(event) {
  const button = event.target.closest("[data-bucket]");
  if (!button) return;
  state.bucket = button.dataset.bucket;
  state.search = "";
  elements.search.value = "";
  loadData({ preserveSelection: false });
}

function changeCategoryFromButton(event) {
  const button = event.target.closest("[data-category]");
  if (button) setCategory(button.dataset.category === state.category ? "all" : button.dataset.category);
}

function changeCategoryFromSelect(event) { setCategory(event.target.value); }

function setCategory(category) {
  state.category = category;
  elements.categorySelect.value = category;
  loadData({ preserveSelection: false });
}

function clearFilters() {
  state.status = "unread";
  state.bucket = "relevant";
  state.category = "all";
  state.search = "";
  elements.search.value = "";
  loadData({ preserveSelection: false });
}

async function openProfile() {
  try {
    state.profile = await api(`/api/profiles/${state.activeKind}`);
    setText("#profile-kind-label", `${state.activeKind === "paper" ? "Paper" : "News"} relevance`);
    fillProfileForm(state.profile);
    await updateProfileJobState();
    elements.profileDialog.showModal();
  } catch (error) { showToast(error.message, true); }
}

function fillProfileForm(profile) {
  document.querySelector("#profile-interests").value = profile.interests.join("\n");
  document.querySelector("#profile-exclusions").value = profile.exclusions.join("\n");
  document.querySelector("#profile-includes").value = profile.include_phrases.join("\n");
  document.querySelector("#profile-never").value = profile.never_show_phrases.join("\n");
  document.querySelector("#selectivity-slider").value = String({ broad: 0, balanced: 1, focused: 2 }[profile.selectivity] ?? 1);
  setText("#profile-threshold", profile.threshold == null ? "Not scored" : `${formatScore(profile.threshold)} / 100`);
}

async function saveProfile(event) {
  event.preventDefault();
  const button = document.querySelector("#save-profile");
  button.disabled = true;
  button.textContent = "Saving…";
  const payload = {
    interests: linesFrom("#profile-interests"),
    exclusions: linesFrom("#profile-exclusions"),
    include_phrases: linesFrom("#profile-includes"),
    never_show_phrases: linesFrom("#profile-never"),
    selectivity: ["broad", "balanced", "focused"][Number(document.querySelector("#selectivity-slider").value)] || "balanced",
  };
  try {
    const result = await api(`/api/profiles/${state.activeKind}`, { method: "PUT", body: JSON.stringify(payload) });
    state.profile = result.profile;
    fillProfileForm(result.profile);
    showToast(`Profile saved; ${state.activeKind} scores are updating`);
    pollRescore();
  } catch (error) { showToast(error.message, true); }
  finally {
    button.disabled = false;
    button.textContent = "Save and rescore";
  }
}

async function rescorePapers() {
  const button = document.querySelector("#rescore-button");
  button.disabled = true;
  try {
    await api(`/api/profiles/${state.activeKind}/rescore`, { method: "POST", body: "{}" });
    showToast(`${state.activeKind === "paper" ? "Paper" : "News"} rescore started`);
    pollRescore();
  } catch (error) { showToast(error.message, true); button.disabled = false; }
}

function pollRescore() {
  clearTimeout(rescorePollTimer);
  updateProfileJobState().then((running) => {
    if (running) rescorePollTimer = setTimeout(pollRescore, 850);
    else loadData();
  });
}

async function updateProfileJobState() {
  try {
    state.relevanceHealth = await api("/api/relevance/health");
    const job = state.relevanceHealth.jobs?.[state.activeKind];
    const label = job?.state === "running" ? "Updating locally…" : job?.state === "failed" ? `Failed: ${job.error}` : job?.state === "complete" ? "Scores up to date" : "Ready to score";
    setText("#profile-job-state", label);
    document.querySelector("#rescore-button").disabled = job?.state === "running";
    renderRankingBanner();
    return job?.state === "running";
  } catch (error) {
    setText("#profile-job-state", error.message);
    return false;
  }
}

function openImport() {
  if (state.activeKind !== "paper") return;
  document.querySelector("#paper-input").value = "";
  document.querySelector("#import-result").hidden = true;
  elements.importDialog.showModal();
  setTimeout(() => document.querySelector("#paper-input").focus(), 0);
}

async function importPaper(event) {
  event.preventDefault();
  const input = document.querySelector("#paper-input");
  const button = document.querySelector("#submit-import");
  const resultElement = document.querySelector("#import-result");
  button.disabled = true;
  button.textContent = "Finding paper…";
  resultElement.hidden = true;
  try {
    const result = await api("/api/import/paper", { method: "POST", body: JSON.stringify({ input: input.value.trim() }) });
    resultElement.hidden = false;
    resultElement.className = "form-result success";
    resultElement.textContent = result.created ? `Added “${result.entry.title}” to Unread.` : `“${result.entry.title}” is already in Herald.`;
    state.activeKind = "paper";
    state.status = "unread";
    state.bucket = "relevant";
    state.category = "all";
    state.search = "";
    elements.search.value = "";
    await loadData({ preserveSelection: false });
    const imported = state.entries.find((entry) => entry.id === result.entry.id);
    if (imported) await selectEntry(imported.id);
    else await previewDetachedEntry(result.entry.id);
    showToast(result.created ? "Paper added to Unread" : "Paper already exists; opened its preview");
  } catch (error) {
    resultElement.hidden = false;
    resultElement.className = "form-result error";
    resultElement.textContent = error.message;
  } finally {
    button.disabled = false;
    button.textContent = "Add to Herald";
  }
}

async function addReferenceFromEvent(event) {
  const previewButton = event.target.closest("[data-preview-cited]");
  if (previewButton) {
    const citedId = Number(previewButton.dataset.previewCited);
    const local = state.entries.some((entry) => entry.id === citedId);
    if (local) selectEntry(citedId);
    else previewDetachedEntry(citedId);
    return;
  }
  const button = event.target.closest("[data-add-reference]");
  if (!button) return;
  button.disabled = true;
  button.textContent = "Adding…";
  try {
    const result = await api(`/api/references/${button.dataset.addReference}/add`, { method: "POST", body: "{}" });
    showToast(result.import.created ? "Cited paper added to Unread" : "Cited paper was already in Herald");
    const payload = await api(`/api/entries/${state.selectedId}/references`);
    state.references = payload.references || [];
    renderReferences(selectedEntry(), false);
    await refreshStats();
  } catch (error) {
    showToast(error.message, true);
    button.disabled = false;
    button.textContent = "Add to Herald";
  }
}

async function previewDetachedEntry(entryId) {
  const requestId = ++detailRequest;
  state.selectedId = entryId;
  state.selectedDetail = null;
  state.references = [];
  syncSelectedCard();
  elements.readerContent.hidden = true;
  elements.reader.classList.add("mobile-open");
  try {
    const [entry, references] = await Promise.all([
      api(`/api/entries/${entryId}`),
      api(`/api/entries/${entryId}/references`),
    ]);
    if (requestId !== detailRequest || state.selectedId !== entryId) return;
    state.selectedDetail = entry;
    state.references = references.references || [];
    renderReader();
  } catch (error) {
    showToast(error.message, true);
    clearSelection();
  }
}

function handleKeyboard(event) {
  if (event.key === "/" && !["INPUT", "TEXTAREA"].includes(document.activeElement?.tagName)) {
    event.preventDefault();
    elements.search.focus();
  }
}

function selectedEntry() {
  const listed = state.entries.find((entry) => entry.id === state.selectedId);
  if (state.selectedDetail?.id === state.selectedId) return mergeEntry(listed || {}, state.selectedDetail);
  return listed;
}

function rankingFor(entry) {
  if (entry?.relevance) return entry.relevance;
  if (entry?.relevance_score == null) return null;
  return {
    score: entry.relevance_score,
    bucket: entry.relevance_bucket,
    components: entry.relevance_components || {},
    explanation: entry.relevance_explanation || {},
    model: entry.relevance_model,
    scored_at: entry.relevance_scored_at,
  };
}

function relevanceReasons(ranking) {
  if (!ranking) return [];
  const explanation = ranking.explanation || {};
  const components = ranking.components || {};
  const reasons = [];
  (explanation.never_show_matches || []).forEach((value) => reasons.push(`Filtered by “${value}”`));
  (explanation.matched_interests || []).forEach((value) => reasons.push(`Matches “${value}”`));
  (explanation.include_matches || []).forEach((value) => reasons.push(`Includes “${value}”`));
  if ((components.feedback_affinity || 0) > 0.5) reasons.push("Similar to papers you kept");
  if ((components.feedback_affinity || 0) < -0.5) reasons.push("Similar to papers you discarded");
  if ((components.source_affinity || 0) > 0.5) reasons.push("From a source you often keep");
  if (!reasons.length && explanation.decision) reasons.push(sentenceCase(explanation.decision));
  return [...new Set(reasons)].slice(0, 3);
}

function mergeEntry(base, update) { return { ...(base || {}), ...(update || {}) }; }
function linesFrom(selector) { return document.querySelector(selector).value.split("\n").map((line) => line.trim()).filter(Boolean); }
function formatScore(value) { return Math.round(Number(value) * 10) / 10; }
function modelLabel(model) {
  if (!model) return "Ranking model unknown";
  if (String(model).toLowerCase().includes("tfidf")) return "Local TF-IDF";
  return `Local ${model}`;
}
function sentenceCase(value) {
  const text = String(value || "").replaceAll("_", " ").replaceAll("-", " ");
  return text ? text[0].toUpperCase() + text.slice(1) : "";
}
function arxivPdfUrl(value) {
  try {
    const url = new URL(value);
    if (!["arxiv.org", "www.arxiv.org", "export.arxiv.org"].includes(url.hostname)) return null;
    let identifier = url.pathname.startsWith("/abs/") ? url.pathname.slice(5) : url.pathname.startsWith("/pdf/") ? url.pathname.slice(5) : "";
    identifier = identifier.replace(/\.pdf$/, "").replace(/^\/+|\/+$/g, "");
    return identifier ? `https://arxiv.org/pdf/${identifier}` : null;
  } catch { return null; }
}
function summaryProvenance(entry) {
  if (entry.summary_provider === "ollama") return { label: `AI-generated locally${entry.summary_model ? ` · ${entry.summary_model}` : ""}`, kind: "ai" };
  if (entry.summary_provider === "extractive") return { label: "Non-AI · extracted from feed abstract", kind: "non-ai" };
  if (entry.summary_provider === "fallback") return { label: "Non-AI fallback · local AI unavailable", kind: "non-ai" };
  if (entry.summary_provider === "demo") return { label: "Demo summary", kind: "demo" };
  if (entry.summary_provider === "unknown") return { label: "Origin unknown · created before provenance tracking", kind: "unknown" };
  return { label: "Not generated", kind: "unknown" };
}
function filterTitle() {
  const noun = state.activeKind === "paper" ? "papers" : "news";
  if (state.category !== "all") return state.category;
  if (state.bucket) return `${state.bucket === "relevant" ? "Relevant" : "Filtered"} ${noun}`;
  return { all: `All ${noun}`, unread: "Unread", read: "Read", kept: "Kept", discarded: "Discarded" }[state.status];
}
function setText(selector, value) { document.querySelector(selector).textContent = value || ""; }
function escapeHtml(value) { const node = document.createElement("div"); node.textContent = String(value ?? ""); return node.innerHTML; }
function escapeAttribute(value) { return escapeHtml(value).replaceAll('"', "&quot;"); }

function showToast(message, error = false) {
  clearTimeout(toastTimer);
  elements.toast.textContent = message;
  elements.toast.className = `toast show${error ? " error" : ""}`;
  toastTimer = setTimeout(() => { elements.toast.className = "toast"; }, 3000);
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
function longDateTime(value) {
  if (!value) return "time unavailable";
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value));
}
function formatToday() {
  const date = new Date();
  return `<strong>${new Intl.DateTimeFormat(undefined, { weekday: "long" }).format(date)}</strong>${new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(date)}`;
}
