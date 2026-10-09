"use strict";

const $ = (sel) => document.querySelector(sel);

async function api(method, path, body) {
  const resp = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || `HTTP ${resp.status}`);
    err.fields = data.fields || {};
    throw err;
  }
  return data;
}

function toast(msg, isError = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.className = isError ? "error" : "";
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (el.hidden = true), 3500);
}

function badge(check) {
  if (!check) return '<span class="badge unknown">—</span>';
  const up = !check.error && check.status_code >= 200 && check.status_code < 400;
  return `<span class="badge ${up ? "up" : "down"}">${up ? "UP" : "DOWN"}</span>`;
}

function esc(s) {
  const d = document.createElement("div");
  d.textContent = s ?? "";
  return d.innerHTML;
}

const fmtTime = (t) => (t ? new Date(t).toLocaleString() : "—");

async function loadSites() {
  const sites = await api("GET", "/api/sites");
  const tbody = $("#sites");
  tbody.innerHTML = "";
  $("#empty").hidden = sites.length > 0;

  for (const s of sites) {
    const c = s.last_check;
    const tr = document.createElement("tr");
    tr.className = "clickable";
    tr.innerHTML = `
      <td>${badge(c)}</td>
      <td>${esc(s.name)}</td>
      <td class="url">${esc(s.url)}</td>
      <td>${c ? c.status_code || "—" : "—"}</td>
      <td>${c ? c.latency_ms + " мс" : "—"}</td>
      <td>${fmtTime(c?.checked_at)}</td>
      <td><button class="danger" title="Удалить">✕</button></td>`;
    tr.title = c?.error || "";
    tr.addEventListener("click", () => showHistory(s));
    tr.querySelector("button").addEventListener("click", (e) => {
      e.stopPropagation();
      deleteSite(s);
    });
    tbody.appendChild(tr);
  }
}

async function loadLastRun() {
  const r = await api("GET", "/api/checks/last");
  $("#last-run").textContent = r
    ? `Последняя проверка: ${fmtTime(r.finished_at)} — всего ${r.total}, доступно ${r.up}, недоступно ${r.down} (${r.duration_ms} мс)`
    : "Проверок ещё не было.";
}

async function deleteSite(s) {
  if (!confirm(`Удалить «${s.name}»?`)) return;
  try {
    await api("DELETE", `/api/sites/${s.id}`);
    toast("Сайт удалён");
    $("#history").hidden = true;
    await loadSites();
  } catch (e) {
    toast(e.message, true);
  }
}

async function showHistory(s) {
  try {
    const checks = await api("GET", `/api/sites/${s.id}/checks?limit=20`);
    $("#history-title").textContent = s.name;
    $("#history-rows").innerHTML = checks.length
      ? checks.map((c) => `
          <tr>
            <td>${badge(c)}</td>
            <td>${c.status_code || "—"}</td>
            <td>${c.latency_ms} мс</td>
            <td>${esc(c.error)}</td>
            <td>${fmtTime(c.checked_at)}</td>
          </tr>`).join("")
      : '<tr><td colspan="5" class="muted">Проверок ещё не было</td></tr>';
    $("#history").hidden = false;
  } catch (e) {
    toast(e.message, true);
  }
}

$("#add-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = e.target;
  form.querySelectorAll(".field-error").forEach((el) => (el.textContent = ""));
  form.querySelectorAll("input").forEach((el) => el.classList.remove("invalid"));

  try {
    await api("POST", "/api/sites", { name: form.elements["name"].value, url: form.elements["url"].value });
    form.reset();
    toast("Сайт добавлен");
    await loadSites();
  } catch (err) {
    // Field errors come from the server-side validator.
    for (const [field, msg] of Object.entries(err.fields)) {
      const el = form.querySelector(`.field-error[data-field="${field}"]`);
      if (el) el.textContent = msg;
      form.elements[field]?.classList.add("invalid");
    }
    if (!Object.keys(err.fields).length) toast(err.message, true);
  }
});

$("#run-btn").addEventListener("click", async (e) => {
  const btn = e.target;
  btn.disabled = true;
  btn.textContent = "Проверяю…";
  try {
    const r = await api("POST", "/api/checks/run");
    toast(`Готово: ${r.up} из ${r.total} доступны`);
    await Promise.all([loadSites(), loadLastRun()]);
  } catch (err) {
    toast(err.message, true);
  } finally {
    btn.disabled = false;
    btn.textContent = "Проверить все";
  }
});

$("#history-close").addEventListener("click", () => ($("#history").hidden = true));

// Initial load and periodic refresh, so results of the background scheduler show up.
const refresh = () => Promise.all([loadSites(), loadLastRun()]).catch((e) => toast(e.message, true));
refresh();
setInterval(refresh, 15000);
