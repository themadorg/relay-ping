const WEBXDC_BUILD_MODE = false; // set to true at WebXDC build time to disable websocket support
const meta = document.getElementById("meta");
const logs = document.getElementById("logs");
const tbl = document.getElementById("tbl");
const stats = document.getElementById("stats");
const scoreBtn = document.getElementById("scoreBtn");
const scorePanel = document.getElementById("scorePanel");
const scoreModalBackdrop = document.getElementById("scoreModalBackdrop");
const closeScoreModalBtn = document.getElementById("closeScoreModalBtn");
const statsBreakdownBackdrop = document.getElementById("statsBreakdownBackdrop");
const statsBreakdownTitle = document.getElementById("statsBreakdownTitle");
const statsBreakdownPanel = document.getElementById("statsBreakdownPanel");
const closeStatsBreakdownBtn = document.getElementById("closeStatsBreakdownBtn");
const errorModalBackdrop = document.getElementById("errorModalBackdrop");
const errorModalTitle = document.getElementById("errorModalTitle");
const errorModalBadge = document.getElementById("errorModalBadge");
const errorModalBody = document.getElementById("errorModalBody");
const copyErrorModalBtn = document.getElementById("copyErrorModalBtn");
const closeErrorModalBtn = document.getElementById("closeErrorModalBtn");
let finalResult = null;
let lastMatrixResult = null;
let activeLogStream = null;

const webxdcMode = WEBXDC_BUILD_MODE || typeof window.webxdc !== "undefined" || new URLSearchParams(location.search).get("webxdc") === "1";
const websocketEnabled = !webxdcMode;
if (!websocketEnabled) {
  meta.textContent = "Waiting for data...";
  scoreBtn.disabled = true;
}

const dropZone = document.getElementById("dropZone");
const fileInput = document.getElementById("fileInput");
const loadBtn = document.getElementById("loadBtn");
const fileLoader = document.getElementById("fileLoader");

if (!websocketEnabled) {
  fileLoader.style.display = "flex";
} else {
  fileLoader.style.display = "none";
}
loadBtn.style.display = websocketEnabled ? "none" : "";

function hideDropZone() {
  dropZone.style.display = "none";
}

function isGzipBuffer(buf) {
  const v = new Uint8Array(buf);
  return v.length >= 2 && v[0] === 0x1f && v[1] === 0x8b;
}

async function arrayBufferToUtf8Text(buf) {
  if (isGzipBuffer(buf)) {
    if (typeof DecompressionStream === "undefined") {
      throw new Error("browser cannot decompress gzip (need DecompressionStream)");
    }
    const ds = new DecompressionStream("gzip");
    const blob = new Blob([buf]).stream().pipeThrough(ds);
    return await new Response(blob).text();
  }
  return new TextDecoder().decode(buf);
}

function loadFile(file) {
  if (/\.tar\.gz$/i.test(file.name)) {
    meta.textContent = "Legacy .tar.gz bundles are not supported; export or load a .json.gz file.";
    return;
  }
  const reader = new FileReader();
  reader.onload = async () => {
    try {
      const text = await arrayBufferToUtf8Text(reader.result);
      const data = JSON.parse(text);
      const payload = data.result || data;
      render(payload);
      if (payload && payload.servers && payload.matrix) {
        finalResult = payload;
        scoreBtn.disabled = false;
      }
      meta.textContent = "Data loaded from file";
      hideDropZone();
    } catch (err) {
      meta.textContent = "Error loading JSON: " + err.message;
    }
  };
  reader.readAsArrayBuffer(file);
}

dropZone.addEventListener("click", () => {
  fileInput.click();
});

dropZone.addEventListener("dragover", (e) => {
  e.preventDefault();
  dropZone.classList.add("dragover");
});

dropZone.addEventListener("dragleave", () => {
  dropZone.classList.remove("dragover");
});

dropZone.addEventListener("drop", (e) => {
  e.preventDefault();
  dropZone.classList.remove("dragover");
  const files = e.dataTransfer.files;
  if (files.length > 0) {
    loadFile(files[0]);
  }
});

fileInput.addEventListener("change", (e) => {
  const files = e.target.files;
  if (files.length > 0) {
    loadFile(files[0]);
  }
});

loadBtn.addEventListener("click", async () => {
  if (typeof window.webxdc !== "undefined") {
    try {
      const files = await window.webxdc.importFiles({ mimeType: "application/json" });
      if (files.length > 0) {
        loadFile(files[0]);
      }
    } catch (err) {
      meta.textContent = "Import failed: " + err.message;
    }
  } else {
    fileInput.click();
  }
});

/** Same bucketing as renderStats (off-diagonal matrix cells only). */
function cellBucketForStats(cell) {
  const st = String((cell && cell.status) || "").toUpperCase();
  if (st === "OK") {
    const ms = Number((cell && cell.latencyMS) || 0);
    if (ms < 1000) return "good";
    if (ms < 2500) return "mid";
    return "slow";
  }
  if (st === "TIMEOUT" || st === "SEND_ERR" || st === "IMAP_ERR") return "bad";
  if (st && st !== "PENDING" && st !== "TESTING") return "bad";
  return null;
}

function collectPairsForBucket(result, bucket) {
  const out = [];
  if (!result || !result.servers || !result.matrix) return out;
  const servers = result.servers;
  const n = servers.length;
  for (let i = 0; i < n; i++) {
    for (let j = 0; j < n; j++) {
      if (i === j) continue;
      const c = result.matrix[i][j] || {};
      if (cellBucketForStats(c) !== bucket) continue;
      const from = servers[i];
      const to = servers[j];
      let detail = "";
      if (bucket === "bad") {
        detail = String(c.status || "");
        const err = c.err != null ? String(c.err) : "";
        if (err) detail += (detail ? ": " : "") + err;
        if (!detail) detail = "(no detail)";
      } else {
        detail = Math.round(Number(c.latencyMS || 0)) + "ms";
      }
      out.push({ from, to, detail });
    }
  }
  out.sort((a, b) => {
    const ca = a.from.localeCompare(b.from);
    if (ca !== 0) return ca;
    return a.to.localeCompare(b.to);
  });
  return out;
}

function statsBucketSpan(bucket, count, clickable) {
  const labelMap = { good: "good", mid: "ok", slow: "slow", bad: "bad" };
  const clsMap = { good: "good", mid: "mid", slow: "slow", bad: "bad" };
  const label = labelMap[bucket];
  let cls = clsMap[bucket];
  if (clickable) cls += " stats-bucket";
  let attrs = ' class="' + cls + '"';
  if (clickable) {
    attrs +=
      ' role="button" tabindex="0" data-stats-bucket="' +
      bucket +
      '" title="Show matrix pairs in this category"';
  }
  return "<span" + attrs + "><b>" + count + "</b> " + label + "</span>";
}

function renderStatsBreakdownHtml(bucket) {
  const pairs = collectPairsForBucket(lastMatrixResult, bucket);
  const col3 = bucket === "bad" ? "Status / detail" : "Latency";
  let html =
    '<h3 class="score-title">' +
    pairs.length +
    " pair" +
    (pairs.length === 1 ? "" : "s") +
    "</h3>";
  html +=
    '<div class="score-table-scroll"><table class="score-table"><thead><tr><th>From</th><th>To</th><th>' +
    col3 +
    "</th></tr></thead><tbody>";
  for (const p of pairs) {
    html +=
      "<tr><td>" +
      escapeHtml(p.from) +
      "</td><td>" +
      escapeHtml(p.to) +
      "</td><td>" +
      escapeHtml(p.detail) +
      "</td></tr>";
  }
  html += "</tbody></table></div>";
  return html;
}

function openStatsBreakdownModal(bucket) {
  if (!lastMatrixResult || !lastMatrixResult.matrix) return;
  closeErrorModal();
  closeScoreModal();
  const titles = {
    good: "Good pairs (<1s latency)",
    mid: "Ok pairs (1s–2.5s latency)",
    slow: "Slow pairs (≥2.5s latency)",
    bad: "Failed pairs",
  };
  statsBreakdownTitle.textContent = titles[bucket] || bucket;
  statsBreakdownPanel.innerHTML = renderStatsBreakdownHtml(bucket);
  statsBreakdownBackdrop.classList.add("show");
}

function closeStatsBreakdownModal() {
  statsBreakdownBackdrop.classList.remove("show");
}

function cellLabel(cell, i, j) {
  const logPath = (cell && cell.logPath) ? String(cell.logPath) : "";
  const err = (cell && cell.err) ? String(cell.err) : "";
  switch ((cell.status || "").toUpperCase()) {
    case "PENDING": return {txt:"...", cls:"pending", logPath};
    case "TESTING": return {txt:"testing", cls:"testing", logPath};
    case "TIMEOUT": return {txt:"timeout", cls:"timeout fail-cell", err: err || "message not delivered within timeout", logPath: logPath};
    case "SEND_ERR":
    case "IMAP_ERR": return {txt:(cell.status||"err").toLowerCase(), cls:"err fail-cell", err: err || "no extra error text", logPath: logPath};
    case "OK": {
      const ms = Math.round(cell.latencyMS || 0);
      if (ms < 1000) return {txt: ms+"ms", cls:"good", logPath};
      if (ms < 2500) return {txt: ms+"ms", cls:"mid", logPath};
      return {txt: ms+"ms", cls:"slow", logPath};
    }
    default:
      return {txt:(cell.status||"").toLowerCase(), cls:"", logPath};
  }
}

function render(result) {
  if (!result || !result.servers || !result.matrix) return;
  lastMatrixResult = result;
  let html = "<tr><th>from\\to</th>";
  for (const s of result.servers) html += "<th>"+s+"</th>";
  html += "</tr>";
  for (let i=0;i<result.matrix.length;i++) {
    html += "<tr><td>"+result.servers[i]+"</td>";
    for (let j=0;j<result.matrix[i].length;j++) {
      const c = cellLabel(result.matrix[i][j], i, j);
      const isClickable = !!c.err;
      const diagCls = i === j ? " diag-cell" : "";
      const cls = c.cls + diagCls + (isClickable ? " clickable-cell" : "");
      const dataErr = isClickable ? ' data-error="' + escapeHtml(c.err) + '"' : "";
      const dataLogPath = c.logPath ? ' data-logpath="' + escapeHtml(c.logPath) + '"' : "";
      const title = isClickable ? ' title="Click to view error details"' : "";
      const ij = ' data-i="'+i+'" data-j="'+j+'"';
      html += '<td class="'+cls+'"' + ij + dataErr + dataLogPath + title + '>'+c.txt+"</td>";
    }
    html += "</tr>";
  }
  tbl.innerHTML = html;
  logs.textContent = "";
  renderStats(result);
}

function renderStats(result) {
  if (!result || !result.servers || !result.matrix) { stats.textContent = ""; return; }
  const n = result.servers.length;
  const bucketClick = !!(result.matrix && result.matrix.length);
  if (result.status) {
    const s = result.status;
    const fedLat = s.fedLatency != null ? s.fedLatency : 0;
    stats.innerHTML =
      "<b>Servers:</b> " + n + " | " +
      "<b>Federated:</b> " + s.federated + "<br>" +
      "<b>Federation latency:</b> " + fedLat + "ms avg<br>" +
      statsBucketSpan("good", s.good ?? 0, bucketClick) +
      ", " +
      statsBucketSpan("mid", s.mid ?? 0, bucketClick) +
      ", " +
      statsBucketSpan("slow", s.slow ?? 0, bucketClick) +
      ", " +
      statsBucketSpan("bad", s.bad ?? 0, bucketClick);
    return;
  }
  const connected = new Array(n).fill(false);
  let good = 0, mid = 0, slow = 0, bad = 0;
  let latSum = 0, latCount = 0;
  for (let i = 0; i < n; i++) {
    for (let j = 0; j < n; j++) {
      if (i === j) continue;
      const c = result.matrix[i][j] || {};
      const st = String(c.status||"").toUpperCase();
      if (st === "OK") {
        const ms = Number(c.latencyMS || 0);
        connected[i] = true;
        connected[j] = true;
        latSum += ms;
        latCount++;
        if (ms < 1000) good++;
        else if (ms < 2500) mid++;
        else slow++;
      } else if (st === "TIMEOUT" || st === "SEND_ERR" || st === "IMAP_ERR") {
        bad++;
      } else if (st && st !== "PENDING" && st !== "TESTING") {
        bad++;
      }
    }
  }
  const federated = connected.filter(Boolean).length;
  const fedLat = latCount > 0 ? Math.round(latSum / latCount) : 0;
  stats.innerHTML =
    "<b>Servers:</b> " + n + " | " +
    "<b>Federated:</b> " + federated + "<br>" +
    "<b>Federation latency:</b> " + fedLat + "ms avg<br>" +
    statsBucketSpan("good", good, bucketClick) +
    ", " +
    statsBucketSpan("mid", mid, bucketClick) +
    ", " +
    statsBucketSpan("slow", slow, bucketClick) +
    ", " +
    statsBucketSpan("bad", bad, bucketClick);
}

function computeScores(result) {
  if (!result || !result.servers || !result.matrix) return [];
  const servers = result.servers;
  const peers = Math.max(servers.length - 1, 1);
  const rows = [];
  for (let i = 0; i < servers.length; i++) {
    let ok = 0;
    let latencySum = 0;
    let latencyCount = 0;
    for (let j = 0; j < servers.length; j++) {
      if (i === j) continue;
      const c = result.matrix[i][j] || {};
      const st = String(c.status || "").toUpperCase();
      if (st === "OK") {
        ok++;
        latencySum += Number(c.latencyMS || 0);
        latencyCount++;
      }
    }
    const avg = latencyCount > 0 ? (latencySum / latencyCount) : Infinity;
    const connectedPct = (ok / peers) * 100;
    const score = latencyCount > 0 ? (connectedPct * 10 - avg / 100) : -999999;
    rows.push({ server: servers[i], ok, peers, connectedPct, avg, score });
  }
  rows.sort((a, b) => {
    if (b.ok !== a.ok) return b.ok - a.ok;
    if (a.avg !== b.avg) return a.avg - b.avg;
    return a.server.localeCompare(b.server);
  });
  return rows;
}

function serverWebHref(server) {
  const s = String(server).trim();
  if (!s) return "#";
  if (/^https?:\/\//i.test(s)) return s;
  return "https://" + s.replace(/^\/+/, "");
}

function renderScores(result) {
  const rows = computeScores(result);
  let html = '<h3 class="score-title">Server Scores (connectivity + latency)</h3>';
  html += '<div class="score-table-scroll"><table class="score-table"><thead><tr><th>#</th><th>Server</th><th>Connected</th><th>Success</th><th>Avg Latency</th><th>Score</th></tr></thead><tbody>';
  rows.forEach((r, idx) => {
    const avgTxt = Number.isFinite(r.avg) ? (Math.round(r.avg) + "ms") : "-";
    const scoreTxt = Number.isFinite(r.score) ? r.score.toFixed(2) : "-";
    const href = escapeHtml(serverWebHref(r.server));
    const label = escapeHtml(r.server);
    html += "<tr><td>" + (idx + 1) + '</td><td><a class="score-server-link" href="' + href +
      '" target="_blank" rel="noopener noreferrer">' + label + "</a></td><td>" + r.ok + "/" + r.peers +
      "</td><td>" + r.connectedPct.toFixed(1) + "%</td><td>" + avgTxt + "</td><td>" + scoreTxt + "</td></tr>";
  });
  html += "</tbody></table></div>";
  scorePanel.innerHTML = html;
}

function escapeHtml(s) {
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

/** Pretty-print matrix/pair logs with colored spans (safe HTML). */
function formatLogToHtml(raw) {
  if (raw == null || raw === "") return "";
  const text = String(raw).replace(/\r\n/g, "\n");
  const lines = text.split("\n");
  return lines.map(formatLogLineHtml).join("\n");
}

function formatLogRestHtml(rest) {
  if (!rest) return "";
  const pairRe = /^pair=(.+?)\s*->\s*(.+)$/;
  let m = rest.match(pairRe);
  if (m) {
    return (
      '<span class="log-key">pair</span><span class="log-eq">=</span>' +
      '<span class="log-from">' +
      escapeHtml(m[1].trim()) +
      "</span> <span class=\"log-arrow\">→</span> " +
      '<span class="log-to">' +
      escapeHtml(m[2].trim()) +
      "</span>"
    );
  }
  const eq = rest.indexOf("=");
  if (eq > 0) {
    const key = rest.slice(0, eq);
    if (/^[a-zA-Z_][a-zA-Z0-9_]*$/.test(key)) {
      const val = rest.slice(eq + 1);
      return (
        '<span class="log-key">' +
        escapeHtml(key) +
        '</span><span class="log-eq">=</span><span class="log-val">' +
        escapeHtml(val) +
        "</span>"
      );
    }
  }
  return escapeHtml(rest);
}

function formatImapStyleLine(line) {
  const tagT = line.match(/^(T\d+)(\s*)(.*)$/i);
  if (tagT && /^T\d+$/i.test(tagT[1])) {
    return '<span class="log-imap-tag">' + escapeHtml(tagT[1]) + "</span>" + escapeHtml(tagT[2] + tagT[3]);
  }
  if (/^\*\s/.test(line)) {
    return '<span class="log-imap-star">*</span>' + escapeHtml(line.slice(1));
  }
  const mailCmd = line.match(/^((?:MAIL FROM|RCPT TO|AUTH\s+(?:PLAIN|LOGIN)))(\s*)(.*)$/i);
  if (mailCmd) {
    return (
      '<span class="log-cmd-word">' +
      escapeHtml(mailCmd[1]) +
      "</span>" +
      escapeHtml(mailCmd[2] + mailCmd[3])
    );
  }
  const oneWord = line.match(/^([A-Za-z][A-Za-z0-9]*)(\s+)(.*)$/);
  if (oneWord && /^(EHLO|HELO|DATA|QUIT|RSET|STARTTLS|NOOP)$/i.test(oneWord[1])) {
    return (
      '<span class="log-cmd-word">' +
      escapeHtml(oneWord[1]) +
      "</span>" +
      escapeHtml(oneWord[2] + oneWord[3])
    );
  }
  return escapeHtml(line);
}

/** Overall outcome from full pair log text — drives header ✅ / ❌ only (not inline). */
function setErrorModalBadgeFromLog(rawText) {
  if (!errorModalBadge) return;
  const t = String(rawText || "");
  if (/final_status=(IMAP_ERR|SEND_ERR|TIMEOUT)\b/.test(t)) {
    errorModalBadge.textContent = "\u274c";
    errorModalBadge.title = "This pair run failed";
    errorModalBadge.hidden = false;
    return;
  }
  if (/final_status=OK\b/.test(t)) {
    errorModalBadge.textContent = "\u2705";
    errorModalBadge.title = "This pair run completed successfully";
    errorModalBadge.hidden = false;
    return;
  }
  errorModalBadge.textContent = "";
  errorModalBadge.title = "";
  errorModalBadge.hidden = true;
}

function formatLogLineHtml(line) {
  if (line === "") return "";
  let html;
  if (/^\[stream\]/.test(line)) {
    html = '<span class="log-stream-tag">' + escapeHtml(line) + "</span>";
  } else {
    const tsFull = line.match(/^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})\]\s*(.*)$/);
    if (tsFull) {
      const inner = tsFull[2];
      const head =
        '<span class="log-ts-bracket">[</span><span class="log-ts">' +
        escapeHtml(tsFull[1]) +
        '</span><span class="log-ts-bracket">]</span> ';
      html = head + formatLogRestHtml(inner);
    } else if (/^\d{3}([\s\-]|$)/.test(line)) {
      const m = line.match(/^(\d{3})([\s\-].*)$/);
      if (m) {
        html = '<span class="log-smtp-code">' + escapeHtml(m[1]) + "</span>" + escapeHtml(m[2]);
      } else {
        const lone = line.match(/^(\d{3})$/);
        if (lone) {
          html = '<span class="log-smtp-code">' + escapeHtml(lone[1]) + "</span>";
        } else {
          html = escapeHtml(line);
        }
      }
    } else {
      html = formatImapStyleLine(line);
    }
  }
  return html;
}

function openErrorModal(title, body, options) {
  closeScoreModal();
  closeStatsBreakdownModal();
  const opts = options || {};
  errorModalTitle.textContent = title || "Cell Error";
  if (opts.plainText) {
    errorModalBody.textContent = body || "No error details.";
    if (errorModalBadge) {
      errorModalBadge.textContent = "";
      errorModalBadge.title = "";
      errorModalBadge.hidden = true;
    }
  } else {
    const raw = body || "No error details.";
    errorModalBody.innerHTML = formatLogToHtml(raw);
    setErrorModalBadgeFromLog(raw);
  }
  errorModalBackdrop.classList.add("show");
}

function closeErrorModal() {
  if (activeLogStream) {
    activeLogStream.close();
    activeLogStream = null;
  }
  if (errorModalBadge) {
    errorModalBadge.textContent = "";
    errorModalBadge.title = "";
    errorModalBadge.hidden = true;
  }
  errorModalBackdrop.classList.remove("show");
}

function openScoreModal() {
  if (!finalResult) return;
  closeErrorModal();
  closeStatsBreakdownModal();
  renderScores(finalResult);
  scoreModalBackdrop.classList.add("show");
}

function closeScoreModal() {
  scoreModalBackdrop.classList.remove("show");
}

scoreBtn.onclick = () => openScoreModal();

stats.addEventListener("click", (ev) => {
  const el = ev.target.closest("[data-stats-bucket]");
  if (!el || !stats.contains(el)) return;
  const bucket = el.getAttribute("data-stats-bucket");
  if (bucket) openStatsBreakdownModal(bucket);
});

stats.addEventListener("keydown", (ev) => {
  if (ev.key !== "Enter" && ev.key !== " ") return;
  const el = ev.target.closest("[data-stats-bucket]");
  if (!el || !stats.contains(el)) return;
  ev.preventDefault();
  const bucket = el.getAttribute("data-stats-bucket");
  if (bucket) openStatsBreakdownModal(bucket);
});

closeStatsBreakdownBtn.onclick = () => closeStatsBreakdownModal();
statsBreakdownBackdrop.addEventListener("click", (ev) => {
  if (ev.target === statsBreakdownBackdrop) closeStatsBreakdownModal();
});

function cellFromTd(td) {
  const i = td.dataset.i;
  const j = td.dataset.j;
  if (i === undefined || j === undefined) return null;
  return lastMatrixResult && lastMatrixResult.matrix && lastMatrixResult.matrix[+i]
    ? lastMatrixResult.matrix[+i][+j]
    : null;
}

function embeddedLogsText(cell) {
  const logs = cell && Array.isArray(cell.logs) ? cell.logs : null;
  if (!logs || !logs.length) return "";
  let body = "";
  for (const L of logs) {
    const ts = L.timestamp ? "[" + L.timestamp + "] " : "";
    body += ts + (L.message || "") + "\n";
  }
  return body;
}

function openCellDetails(title, td) {
  const cell = cellFromTd(td);
  const embedded = embeddedLogsText(cell);
  if (embedded) {
    openErrorModal(title, embedded);
    return;
  }
  const fallback = td.getAttribute("data-error") || "No error details.";
  const logPath = td.getAttribute("data-logpath");
  if (!logPath) {
    openErrorModal(title, fallback);
    return;
  }
  if (activeLogStream) {
    activeLogStream.close();
    activeLogStream = null;
  }
  openErrorModal(title, "Loading full timestamped log stream...\n", { plainText: true });
  const es = new EventSource("/pair-log-stream?path=" + encodeURIComponent(logPath));
  activeLogStream = es;
  const streamLines = [];
  es.onmessage = (ev) => {
    let line = ev.data;
    try {
      const obj = JSON.parse(ev.data);
      line = obj.line || "";
    } catch (_) {}
    streamLines.push(line);
    const joined = streamLines.join("\n");
    errorModalBody.innerHTML = formatLogToHtml(joined);
    setErrorModalBadgeFromLog(joined);
    if (errorModalBody.parentElement) {
      errorModalBody.parentElement.scrollTop = errorModalBody.parentElement.scrollHeight;
    }
  };
  es.onerror = () => {
    streamLines.push("[stream] disconnected");
    const joined = streamLines.join("\n");
    errorModalBody.innerHTML = formatLogToHtml(joined);
    setErrorModalBadgeFromLog(joined);
    es.close();
    if (activeLogStream === es) {
      activeLogStream = null;
    }
  };
}

let selectedCell = null;
function setSelectedCell(td) {
  if (selectedCell && selectedCell !== td) {
    selectedCell.classList.remove("selected-cell");
  }
  selectedCell = td || null;
  if (selectedCell) {
    selectedCell.classList.add("selected-cell");
  }
}

tbl.addEventListener("click", (ev) => {
  const td = ev.target && ev.target.closest ? ev.target.closest("td") : null;
  if (!td) return;
  setSelectedCell(td);
  if (td.hasAttribute("data-error")) {
    const row = td.parentElement;
    const from = row && row.firstElementChild ? row.firstElementChild.textContent : "unknown";
    const colIdx = td.cellIndex;
    let to = "unknown";
    if (tbl.rows && tbl.rows[0] && tbl.rows[0].cells[colIdx]) {
      to = tbl.rows[0].cells[colIdx].textContent;
    }
    openCellDetails("Log: " + from + " -> " + to, td);
  }
});

tbl.addEventListener("dblclick", (ev) => {
  const td = ev.target && ev.target.closest ? ev.target.closest("td") : null;
  if (!td) return;
  setSelectedCell(td);
  if (td.hasAttribute("data-error")) return;
  const cell = cellFromTd(td);
  if (embeddedLogsText(cell)) {
    const row = td.parentElement;
    const from = row && row.firstElementChild ? row.firstElementChild.textContent : "unknown";
    const colIdx = td.cellIndex;
    let to = "unknown";
    if (tbl.rows && tbl.rows[0] && tbl.rows[0].cells[colIdx]) {
      to = tbl.rows[0].cells[colIdx].textContent;
    }
    openCellDetails("Log: " + from + " -> " + to, td);
    return;
  }
  const logPath = td.getAttribute("data-logpath");
  if (!logPath) {
    openErrorModal("Cell Log", "No log available for this cell yet.");
    return;
  }
  const row = td.parentElement;
  const from = row && row.firstElementChild ? row.firstElementChild.textContent : "unknown";
  const colIdx = td.cellIndex;
  let to = "unknown";
  if (tbl.rows && tbl.rows[0] && tbl.rows[0].cells[colIdx]) {
    to = tbl.rows[0].cells[colIdx].textContent;
  }
  openCellDetails("Log: " + from + " -> " + to, td);
});

closeScoreModalBtn.onclick = closeScoreModal;
scoreModalBackdrop.addEventListener("click", (ev) => {
  if (ev.target === scoreModalBackdrop) closeScoreModal();
});
closeErrorModalBtn.onclick = closeErrorModal;
copyErrorModalBtn.onclick = async () => {
  const txt = errorModalBody.textContent || "";
  if (!txt) return;
  const prev = copyErrorModalBtn.textContent;
  try {
    await navigator.clipboard.writeText(txt);
    copyErrorModalBtn.textContent = "Copied";
  } catch (_) {
    copyErrorModalBtn.textContent = "Copy failed";
  }
  setTimeout(() => { copyErrorModalBtn.textContent = prev; }, 1200);
};
errorModalBackdrop.addEventListener("click", (ev) => {
  if (ev.target === errorModalBackdrop) closeErrorModal();
});
document.addEventListener("keydown", (ev) => {
  if (ev.key !== "Escape") return;
  if (statsBreakdownBackdrop.classList.contains("show")) {
    closeStatsBreakdownModal();
    return;
  }
  if (scoreModalBackdrop.classList.contains("show")) {
    closeScoreModal();
    return;
  }
  closeErrorModal();
});

if (websocketEnabled) {
  const wsProto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(wsProto + "://" + location.host + "/ws");
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    const p = msg;
    const note = String(p.note || "").trim();
    const hasProgress = typeof p.completed === "number" && typeof p.total === "number";

    render(p.result);

    if (p.type === "done") {
      finalResult = p.result || null;
      scoreBtn.disabled = finalResult == null;
      const tot = typeof p.total === "number" ? p.total : 0;
      meta.textContent = tot > 0
        ? ("All " + tot + " pair tests finished.")
        : "All pair tests finished.";
      return;
    }

    scoreBtn.disabled = true;

    if (p.currentFrom && p.currentTo) {
      meta.textContent = "testing: " + p.currentFrom + " -> " + p.currentTo + " (" + p.completed + "/" + p.total + ")";
    } else if (typeof p.completed === "number" && p.total != null) {
      meta.textContent = "preparing accounts (" + p.completed + "/" + p.total + ")";
    } else if (note && !note.startsWith("Loaded ") && note !== "complete") {
      meta.textContent = hasProgress ? note + " (" + p.completed + "/" + p.total + ")" : note;
    } else {
      meta.textContent = "";
    }
  };
  ws.onclose = () => { meta.textContent += " | websocket disconnected"; };
} else {
  logs.textContent = "";
  (function tryLoadBundledRun() {
    fetch("static/bundled-run.json.gz", { cache: "no-store" })
      .then((r) => {
        if (!r.ok) {
          throw new Error("no bundled run");
        }
        return r.arrayBuffer();
      })
      .then(arrayBufferToUtf8Text)
      .then((text) => {
        const data = JSON.parse(text);
        const payload = data.result || data;
        render(payload);
        if (payload && payload.servers && payload.matrix) {
          finalResult = payload;
          scoreBtn.disabled = false;
        }
        meta.textContent = "Bundled CI run loaded";
        hideDropZone();
      })
      .catch(() => {});
  })();
}
