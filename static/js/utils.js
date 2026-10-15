// ── XSS helper ────────────────────────────────────────────────────────────────
export function esc(s) {
  return String(s ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// ── API helpers ───────────────────────────────────────────────────────────────

const OP_ID_HEADER = 'X-Operation-ID';

// Lanes that do not create tracked operations (jobs data-plane and the
// replication store/scheduler). They keep their own progress channels
// (jobs.update) and must not open an operation event stream that can never
// resolve.
const OP_TRACKING_SKIP = /^\/api\/(jobs(\/|$)|replication(\/|$)|snapshots\/send\b|rewrite\/)/;

// Operation progress streams keyed by operation ID. One dialog binds one ID,
// so a second browser tab can never paint another operation's steps here.
const opStreams = new Map();
let dialogOpId = null; // operation currently allowed to touch the op-log dialog

// generateOpId returns a client-generated operation ID. randomUUID is only
// available in secure contexts; plain-HTTP LAN deployments fall back to an
// RFC-4122-shaped v4 id built from getRandomValues/Math.random (uniqueness,
// not cryptographic strength, is what idempotency needs).
function generateOpId() {
  const hex = n => Array.from({ length: n }, () =>
    Math.floor(Math.random() * 16).toString(16)).join('');
  let uuid;
  if (window.crypto?.randomUUID) {
    uuid = window.crypto.randomUUID();
  } else if (window.crypto?.getRandomValues) {
    const b = new Uint8Array(16);
    window.crypto.getRandomValues(b);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    const h = Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
    uuid = `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
  } else {
    uuid = `${hex(8)}-${hex(4)}-4${hex(3)}-a${hex(3)}-${hex(12)}`;
  }
  return 'op_' + uuid;
}

// openOperationStream subscribes to one operation's SSE stream. It tolerates
// the "connected before the server accepted the POST" race (404), network
// blips and slow-consumer server closes by recreating the EventSource on a
// short capped backoff; every (re)connection receives an op.snapshot full
// replace, and op.step events are seq-deduped, so neither path can gap or
// duplicate the dialog.
function openOperationStream(opId) {
  if (opStreams.has(opId)) return opStreams.get(opId);
  const handle = { es: null, closed: false, fails: 0, lastSeq: 0 };
  opStreams.set(opId, handle);

  const connect = () => {
    if (handle.closed) return;
    const es = new EventSource(`/api/operations/${encodeURIComponent(opId)}/events`);
    handle.es = es;

    es.addEventListener('op.snapshot', e => {
      handle.fails = 0;
      let rec;
      try { rec = JSON.parse(e.data); } catch { return; }
      handle.lastSeq = rec.seq_last || 0;
      if (dialogAccepts(opId)) renderRunningSteps(rec.steps || [], false);
      if (rec.status && rec.status !== 'running') closeOperationStream(opId);
    });
    es.addEventListener('op.step', e => {
      handle.fails = 0;
      let step;
      try { step = JSON.parse(e.data); } catch { return; }
      if (!step || step.seq <= handle.lastSeq) return; // dedupe replays
      handle.lastSeq = step.seq;
      if (dialogAccepts(opId)) appendOpLogStep(step);
    });
    es.addEventListener('op.finished', () => closeOperationStream(opId));
    es.onerror = () => {
      if (handle.closed) return;
      es.close();
      handle.fails++;
      // 404 pre-acceptance, dropped slow subscriber, proxy restart — the
      // snapshot-on-reconnect model converges on all of them.
      const delay = Math.min(200 * handle.fails, 3000);
      setTimeout(connect, delay);
    };
  };
  connect();
  return handle;
}

function closeOperationStream(opId) {
  const handle = opStreams.get(opId);
  if (!handle) return;
  handle.closed = true;
  try { handle.es?.close(); } catch { /* ignore */ }
  opStreams.delete(opId);
}

// dialogAccepts reports whether live events for opId may paint into the
// currently open running dialog.
function dialogAccepts(opId) {
  return opLogDialog.open
    && dialogOpId === opId
    && document.getElementById('opLogClose').disabled;
}

// recoverOperation resolves an ambiguous request (POST response never
// arrived) by querying the operation the server knows under the same ID,
// until a terminal record appears, the operation expires, or the 1h retention
// horizon is crossed. The dialog is driven via the same SSE stream meanwhile.
async function recoverOperation(opId) {
  if (dialogAccepts(opId)) renderRunningSteps(null, true);
  const deadline = Date.now() + 65 * 60 * 1000;
  let consecutiveNotFound = 0;
  for (;;) {
    await new Promise(r => setTimeout(r, 2000));
    let rec = null;
    let status = 0;
    try {
      const res = await fetch(`/api/operations/${encodeURIComponent(opId)}`);
      status = res.status;
      if (status === 401) { window.location.href = '/login'; throw new Error('unauthorized'); }
      if (status === 410) {
        // Fixed Chinese copy; the server body is only a machine-readable
        // contract and is not user-facing text.
        const err = new Error('操作结果已过期，不可恢复');
        err.unrecoverable = true;
        throw err;
      }
      if (res.ok) rec = await res.json();
    } catch (e) {
      if (e.unrecoverable) throw e;
      // network still down — reset the not-found streak and keep waiting
      consecutiveNotFound = 0;
      status = 0;
    }
    if (rec && rec.status && rec.status !== 'running') {
      if (rec.status === 'succeeded') return { tasks: rec.steps || [] };
      const err = new Error(rec.error || `operation ${rec.status}`);
      err.tasks = rec.steps || [];
      throw err;
    }
    // A reachable server that has never heard of this ID means the POST did
    // not reach acceptance (lost before the server saw it). Give it a short
    // grace window for an in-flight request, then stop ambiguously waiting.
    if (status === 404) {
      consecutiveNotFound++;
      if (consecutiveNotFound >= 30) { // ~60 s of reachable 404s
        const err = new Error('请求未能确认：服务端没有该操作记录，请在网络恢复后检查实际状态');
        err.unrecoverable = true;
        throw err;
      }
    } else {
      consecutiveNotFound = 0;
    }
    if (Date.now() > deadline) {
      const err = new Error('操作结果已超过可恢复时限，不可恢复');
      err.unrecoverable = true;
      throw err;
    }
  }
}

export async function api(method, path, body) {
  const tracked = method !== 'GET' && !OP_TRACKING_SKIP.test(path);
  const opId = tracked ? generateOpId() : null;
  if (tracked) {
    dialogOpId = opId;
    openOperationStream(opId);
  }

  const opts = { method, headers: { 'Content-Type': 'application/json' } };
  if (opId) opts.headers[OP_ID_HEADER] = opId;
  if (body !== undefined) opts.body = JSON.stringify(body);

  let res;
  try {
    res = await fetch(path, opts);
  } catch (networkErr) {
    // Response never arrived: the change may have been accepted. Never turn
    // this into an "unknown" error — resolve against the known operation ID.
    if (!tracked) throw networkErr;
    try {
      return await recoverOperation(opId);
    } finally {
      closeOperationStream(opId);
    }
  }
  // HTTP response received (any status): the outcome is deterministic; live
  // streaming has served its purpose for this request.
  closeOperationStream(opId);

  if (res.status === 401) { window.location.href = '/login'; throw new Error('unauthorized'); }
  if (res.status === 204) return null;
  const data = await res.json();
  if (!res.ok) {
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.tasks = data.tasks || null;
    throw err;
  }
  return data;
}

// ── Formatting ────────────────────────────────────────────────────────────────
export function fmtBytes(n) {
  if (n === 0) return '—';
  const units = ['B', 'K', 'M', 'G', 'T', 'P'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
}

export function fmtDate(epoch) {
  if (!epoch) return '—';
  return new Date(epoch * 1000).toLocaleString();
}

// Relative age from a unix-seconds timestamp: "3d ago", "2h ago", "5m ago".
export function fmtAge(epoch) {
  if (!epoch) return '—';
  const secs = Math.max(0, Math.floor(Date.now() / 1000 - epoch));
  if (secs < 60) return 'just now';
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 365) return `${d}d ago`;
  return `${Math.floor(d / 365)}y ago`;
}

export function fmtPct(p) {
  return p.toFixed(1) + '%';
}

export function fmtSpeed(mbps) {
  if (mbps < 0) return '—';
  if (mbps >= 1000) return (mbps / 1000) + ' Gbps';
  return mbps + ' Mbps';
}

export function fmtNum(n) {
  if (!n) return '—';
  return n.toLocaleString(undefined, { maximumFractionDigits: 0 });
}

export function fmtHours(h) {
  if (!h) return '—';
  if (h >= 8760) return (h / 8760).toFixed(1) + ' y';
  return h.toLocaleString() + ' h';
}

export function fmtUptime(secs) {
  const s = Math.round(secs);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// ── Operation log dialog ──────────────────────────────────────────────────────
export const opLogDialog = document.getElementById('opLogDialog');
document.getElementById('opLogClose').addEventListener('click', () => opLogDialog.close());

const stepIcons = { ok: '✓', changed: '●', failed: '✗', skipped: '–' };

// stepHTML renders one task step identically in running snapshots and in the
// final response-driven render.
function stepHTML(t) {
  return `
    <div class="op-step ${esc(t.status)}">
      <span class="op-step-icon">${esc(stepIcons[t.status] || '?')}</span>
      <span class="op-step-name">${esc(t.name)}</span>
      ${t.msg ? `<span class="op-step-msg">${esc(t.msg)}</span>` : ''}
    </div>`;
}

export function showOpLogRunning(title) {
  document.getElementById('opLogTitle').textContent = title;
  renderRunningSteps(null, false);
  document.getElementById('opLogError').style.display = 'none';
  document.getElementById('opLogClose').disabled = true;
  if (!opLogDialog.open) opLogDialog.showModal();
}

// renderRunningSteps replaces the dialog body while execution is in flight.
// An empty list shows a placeholder; recovering switches its wording after a
// lost response. The close button stays disabled until a terminal render.
function renderRunningSteps(steps, recovering) {
  const stepsEl = document.getElementById('opLogSteps');
  document.getElementById('opLogError').style.display = 'none';
  document.getElementById('opLogClose').disabled = true;
  if (!steps || steps.length === 0) {
    stepsEl.innerHTML = `
      <div class="op-step running">
        <span class="op-step-icon">⟳</span>
        <span class="op-step-name">${recovering ? '连接中断，正在恢复结果…' : 'Running…'}</span>
      </div>`;
    return;
  }
  stepsEl.innerHTML = steps.map(stepHTML).join('');
}

export function showOpLog(title, tasks, errorMsg) {
  document.getElementById('opLogTitle').textContent = title;
  const stepsEl = document.getElementById('opLogSteps');
  const errorEl = document.getElementById('opLogError');

  stepsEl.innerHTML = (tasks || []).map(stepHTML).join('');

  if (errorMsg) {
    errorEl.textContent = errorMsg;
    errorEl.style.display = '';
  } else {
    errorEl.style.display = 'none';
  }
  document.getElementById('opLogClose').disabled = false;
  if (!opLogDialog.open) opLogDialog.showModal();
}

// appendOpLogStep appends a single tracked operation step to the running
// dialog. Removes any placeholder on the first call. Sequence de-duplication
// happens in the stream layer before this is called.
export function appendOpLogStep(step) {
  const stepsEl = document.getElementById('opLogSteps');
  // Remove the running/recovering placeholder if present.
  const running = stepsEl.querySelector('.op-step.running');
  if (running) running.remove();
  const div = document.createElement('div');
  div.className = `op-step ${esc(step.status)}`;
  div.innerHTML = `
    <span class="op-step-icon">${esc(stepIcons[step.status] || '?')}</span>
    <span class="op-step-name">${esc(step.name)}</span>
    ${step.msg ? `<span class="op-step-msg">${esc(step.msg)}</span>` : ''}`;
  stepsEl.appendChild(div);
}

// ── Toast ─────────────────────────────────────────────────────────────────────
let toastTimer;
export function toast(msg, type = 'ok') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = `toast show ${type}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 3500);
}

// ── Event delegation ──────────────────────────────────────────────────────────
// One listener per stable container, bound once at module init. Render
// functions replace the container's innerHTML freely; the listener survives.
// Dispatches on the closest [data-action] ancestor of the event target, so
// an inner button wins over an outer row that also carries data-action.
// Handlers receive (dataset, element, event).
export function delegate(container, handlers, type = 'click') {
  container.addEventListener(type, e => {
    const el = e.target.closest('[data-action]');
    if (!el || !container.contains(el)) return;
    const fn = handlers[el.dataset.action];
    if (fn) fn(el.dataset, el, e);
  });
}

// ── ZFS name validation regexes ───────────────────────────────────────────────
export const reZFSName   = /^[a-zA-Z][a-zA-Z0-9_.:-]*(\/[a-zA-Z][a-zA-Z0-9_.:-]*)*$/;
export const reSnapLabel = /^[a-zA-Z0-9][a-zA-Z0-9_.:-]*$/;

// ── Refresh ───────────────────────────────────────────────────────────────────
export function setRefreshing(v) {
  document.getElementById('refreshBtn').classList.toggle('spinning', v);
}
