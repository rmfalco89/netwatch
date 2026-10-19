// Netwatch dashboard. Every number comes from one of two places:
//   measured  — router-bound traffic (default routes) and interface counters
//   attributed — per-process connections from nettop, split by destination
// The Overview shows both, so it is visible how much of the measured total
// the per-process view accounts for.

const PERIODS = ['today', '1h', '24h', '7d', '30d'];
const CLASS_INFO = {
    internet:     { label: 'Internet',      color: '#ef4444' },
    lan:          { label: 'LAN',           color: '#3b82f6' },
    vpn:          { label: 'VPN',           color: '#a855f7' },
    tailnet:      { label: 'Tailscale',     color: '#14b8a6' },
    unclassified: { label: 'Unclassified',  color: '#f59e0b' },
    host:         { label: 'Host-internal', color: '#52525b' },
};

const state = {
    page: 'overview',
    period: 'today',
    label: '',          // Who → drill-down
    whereClass: 'internet',
    whereSort: 'out',
    sort: {},           // tableId → {col, dir}
};
let charts = [];
let refreshTimer = null;

// ---- helpers -------------------------------------------------------------------

const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path) {
    const res = await fetch(path);
    if (!res.ok) throw new Error(`${path}: ${res.status}`);
    return res.json();
}

function humanBytes(b) {
    b = b || 0;
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (b >= 1000 && i < u.length - 1) { b /= 1000; i++; }
    return `${b.toFixed(i === 0 ? 0 : b < 10 ? 2 : 1)} ${u[i]}`;
}

function rate(bytes, seconds) {
    if (!seconds) return '';
    const bits = bytes * 8 / seconds;
    if (bits >= 1e6) return `${(bits / 1e6).toFixed(1)} Mbit/s`;
    if (bits >= 1e3) return `${(bits / 1e3).toFixed(0)} kbit/s`;
    return `${bits.toFixed(0)} bit/s`;
}

const pct = (a, b) => b > 0 ? `${Math.round(100 * a / b)}%` : '—';
const periodLabel = p => p === 'today' ? 'Today' : `Last ${p}`;
const cls = (row, k) => row.classes?.[k] || { in: 0, out: 0 };

function periodSelector() {
    return `<div class="period-selector">${PERIODS.map(p =>
        `<button class="period-btn ${p === state.period ? 'active' : ''}" data-period="${p}">${p}</button>`).join('')}</div>`;
}

function bindPeriodSelector() {
    document.querySelectorAll('.period-btn[data-period]').forEach(b =>
        b.addEventListener('click', () => { state.period = b.dataset.period; render(); }));
}

function sortRows(rows, id, defCol, getters) {
    const s = state.sort[id] || { col: defCol, dir: 'desc' };
    const g = getters[s.col];
    return [...rows].sort((a, b) => {
        const x = g(a), y = g(b);
        const c = typeof x === 'string' ? x.localeCompare(y) : x - y;
        return s.dir === 'asc' ? c : -c;
    });
}

function th(id, col, text, num, defCol) {
    const s = state.sort[id] || { col: defCol, dir: 'desc' };
    const cl = [num ? 'num' : '', s.col === col ? `sort-${s.dir}` : ''].join(' ');
    return `<th class="${cl}" data-table="${id}" data-col="${col}">${text}</th>`;
}

function bindSort() {
    document.querySelectorAll('th[data-table]').forEach(h => h.addEventListener('click', () => {
        const id = h.dataset.table, col = h.dataset.col;
        const cur = state.sort[id];
        state.sort[id] = { col, dir: cur?.col === col && cur.dir === 'desc' ? 'asc' : 'desc' };
        render();
    }));
}

function destroyCharts() { charts.forEach(c => c.destroy()); charts = []; }

// ---- pages ----------------------------------------------------------------------

async function render() {
    destroyCharts();
    clearInterval(refreshTimer);
    document.querySelectorAll('.nav-links a').forEach(a => a.classList.toggle('active', a.dataset.page === state.page));
    const el = document.getElementById('content');
    try {
        if (state.page === 'overview') await renderOverview(el);
        else if (state.page === 'who') await (state.label ? renderLabel(el) : renderWho(el));
        else if (state.page === 'where') await renderWhere(el);
        else renderAbout(el);
    } catch (e) {
        el.innerHTML = `<h1>Error</h1><p class="empty">${esc(e.message)}</p>`;
    }
    if (state.page !== 'about') refreshTimer = setInterval(render, 60000);
}

async function renderOverview(el) {
    const q = `period=${state.period}`;
    const [sum, who, where, tl] = await Promise.all([
        api(`/api/summary?${q}`), api(`/api/who?${q}`),
        api(`/api/destinations?${q}&sort=out&limit=10`), api(`/api/timeline?${q}`),
    ]);
    const secs = sum.to - sum.from;
    const c = sum.classes || {};
    const inet = sum.internet, attr = c.internet || { in: 0, out: 0 }, uncl = c.unclassified || { in: 0, out: 0 };
    const host = c.host || { in: 0, out: 0 };

    const topUp = [...who].filter(r => cls(r, 'internet').out > 0)
        .sort((a, b) => cls(b, 'internet').out - cls(a, 'internet').out).slice(0, 10);

    el.innerHTML = `
        <div class="table-header"><h1>Overview · ${periodLabel(state.period)}</h1>${periodSelector()}</div>
        <div class="cards">
            <div class="card">
                <div class="card-label">Internet — measured at the router route</div>
                <div class="card-value"><span class="up">${humanBytes(inet.out)} ↑</span></div>
                <div class="card-sub"><span class="down">${humanBytes(inet.in)} ↓</span> · avg ${rate(inet.out, secs)} up</div>
            </div>
            <div class="card">
                <div class="card-label">LAN — other devices at home</div>
                <div class="card-value">${humanBytes(sum.lan.out)} ↑</div>
                <div class="card-sub">${humanBytes(sum.lan.in)} ↓ · includes bridged VMs</div>
            </div>
            ${(sum.utm.in + sum.utm.out) > 0 ? `
            <div class="card">
                <div class="card-label">UTM VMs — own network, not per-process</div>
                <div class="card-value">${humanBytes(sum.utm.out)} ↑</div>
                <div class="card-sub">${humanBytes(sum.utm.in)} ↓ · internet + LAN, from vmenet</div>
            </div>` : ''}
            <div class="card dim">
                <div class="card-label">Host-internal — never leaves this Mac</div>
                <div class="card-value">${humanBytes(host.out)}</div>
                <div class="card-sub">loopback between processes, containers, VMs</div>
            </div>
        </div>
        <p class="note">Of the measured internet upload, <b>${pct(attr.out, inet.out)}</b> is pinned on a process and destination
            (${humanBytes(attr.out)}). Connections that opened and closed between two samples add ${humanBytes(uncl.out)} up / ${humanBytes(uncl.in)} down
            whose process connection wasn't seen (<i>unclassified</i>); their destinations are still measured, see <a href="#where" class="link">Where</a>.</p>

        <div class="chart-wrap">
            <div class="table-header"><h2>Internet traffic over time (measured)</h2></div>
            <div class="chart-box"><canvas id="tl"></canvas></div>
        </div>

        <div class="grid2">
            <div class="table-wrap">
                <div class="table-header"><h2>Top internet uploaders</h2><a href="#who" class="link">all →</a></div>
                <div class="table-scroll"><table>
                    <thead><tr><th>Who</th><th class="num">Up</th><th class="num">Down</th><th class="num">of which served</th></tr></thead>
                    <tbody>${topUp.map(r => `<tr>
                        <td class="process-name" data-label="${esc(r.name)}">${esc(r.name)}</td>
                        <td class="num up">${humanBytes(cls(r, 'internet').out)}</td>
                        <td class="num">${humanBytes(cls(r, 'internet').in)}</td>
                        <td class="num dimtext">${humanBytes(cls(r, 'internet/served').out)}</td>
                    </tr>`).join('') || '<tr><td colspan="4" class="empty">No internet traffic attributed yet</td></tr>'}</tbody>
                </table></div>
            </div>
            <div class="table-wrap">
                <div class="table-header"><h2>Top upload destinations</h2><a href="#where" class="link">all →</a></div>
                <div class="table-scroll"><table>
                    <thead><tr><th>Where</th><th>Who</th><th class="num">Up</th></tr></thead>
                    <tbody>${(where || []).map(d => `<tr>
                        <td>${destCell(d)}</td><td class="dimtext who-cell" title="${esc(d.labels)}">${esc(d.labels) || '<i>short-lived, process not seen</i>'}</td>
                        <td class="num up">${humanBytes(d.out)}</td>
                    </tr>`).join('') || '<tr><td colspan="3" class="empty">Nothing yet</td></tr>'}</tbody>
                </table></div>
            </div>
        </div>`;
    bindPeriodSelector();
    bindLabelLinks();
    timelineChart('tl', tl, [
        ['truth:internet', 'out', 'Internet up', '#ef4444'],
        ['truth:internet', 'in', 'Internet down', '#3b82f6'],
    ]);
}

function ownerHTML(r) {
    const owner = r.org ? `${r.org}${r.country ? ' · ' + r.country : ''}` : '';
    const sub = `<div class="dimtext small">${esc(r.remote)}${r.asn ? ' · ' + esc(r.asn) : ''}</div>`
        + (r.ptr ? `<div class="dimtext small">${esc(r.ptr)}</div>` : '');
    return { owner, sub };
}

function destCell(d) {
    const { owner, sub } = ownerHTML(d);
    const ports = d.ports ? `<span class="dimtext">:${esc(d.ports.split(',').slice(0, 3).join(', :'))}</span>` : '';
    const served = d.served ? ' <span class="tag">served</span>' : '';
    return `<div><b>${esc(owner || d.remote)}</b> ${ports}${served}</div>${sub}`;
}

function endpointCell(w) {
    const { owner, sub } = ownerHTML(w);
    const dir = w.dir === 'served' ? `<span class="tag">served :${w.port}</span>` : `<span class="dimtext">:${w.port}</span>`;
    return `<div><b>${esc(owner || w.remote)}</b> ${dir}</div>${sub}`;
}

function whoTable(id, rows, clickable) {
    const total = r => Object.entries(r.classes).filter(([k]) => !k.includes('/') && k !== 'host').reduce((s, [, v]) => s + v.in + v.out, 0);
    const getters = {
        name: r => r.name,
        iup: r => cls(r, 'internet').out, idown: r => cls(r, 'internet').in,
        lup: r => cls(r, 'lan').out, ldown: r => cls(r, 'lan').in,
        vpn: r => cls(r, 'vpn').in + cls(r, 'vpn').out + cls(r, 'tailnet').in + cls(r, 'tailnet').out,
        uncl: r => cls(r, 'unclassified').in + cls(r, 'unclassified').out,
        host: r => cls(r, 'host').in + cls(r, 'host').out,
        total,
    };
    const sorted = sortRows(rows.filter(r => total(r) + getters.host(r) > 0), id, 'iup', getters);
    return `<div class="table-scroll"><table>
        <thead><tr>
            ${th(id, 'name', 'Who', false, 'iup')}
            ${th(id, 'iup', 'Internet ↑', true, 'iup')}${th(id, 'idown', 'Internet ↓', true, 'iup')}
            ${th(id, 'lup', 'LAN ↑', true, 'iup')}${th(id, 'ldown', 'LAN ↓', true, 'iup')}
            ${th(id, 'vpn', 'VPN / Tailscale', true, 'iup')}
            ${th(id, 'uncl', 'Unclassified', true, 'iup')}
            ${th(id, 'host', 'Host-internal', true, 'iup')}
        </tr></thead>
        <tbody>${sorted.map(r => `<tr>
            <td class="${clickable ? 'process-name' : ''}" ${clickable ? `data-label="${esc(r.name)}"` : ''}>${esc(r.name)}</td>
            <td class="num up">${humanBytes(cls(r, 'internet').out)}${cls(r, 'internet/served').out ? `<div class="dimtext small">served ${humanBytes(cls(r, 'internet/served').out)}</div>` : ''}</td>
            <td class="num">${humanBytes(cls(r, 'internet').in)}</td>
            <td class="num">${humanBytes(cls(r, 'lan').out)}</td>
            <td class="num">${humanBytes(cls(r, 'lan').in)}</td>
            <td class="num">${humanBytes(getters.vpn(r))}</td>
            <td class="num dimtext">${humanBytes(cls(r, 'unclassified').out)} ↑ ${humanBytes(cls(r, 'unclassified').in)} ↓</td>
            <td class="num dimtext">${humanBytes(getters.host(r))}</td>
        </tr>`).join('')}</tbody>
    </table></div>`;
}

async function renderWho(el) {
    const rows = await api(`/api/who?period=${state.period}`);
    el.innerHTML = `
        <div class="table-header"><h1>Who · ${periodLabel(state.period)}</h1>${periodSelector()}</div>
        <p class="note">One row per program, VM or container host. A VM row includes its LAN tunnels; <b>Docker (Colima VM)</b> is every
            container together (containers share one network process, so they can't be told apart from the Mac). Click a row for its processes and destinations.
            <b>Host-internal</b> is loopback traffic between processes on this Mac and is not network traffic.</p>
        <div class="table-wrap">${whoTable('who', rows, true)}</div>`;
    bindPeriodSelector();
    bindSort();
    bindLabelLinks();
}

async function renderLabel(el) {
    const L = encodeURIComponent(state.label);
    const q = `period=${state.period}&label=${L}`;
    const [procs, where, tl] = await Promise.all([
        api(`/api/who?${q}`), api(`/api/where?${q}&sort=total&limit=50`), api(`/api/timeline?${q}`),
    ]);
    el.innerHTML = `
        <div class="table-header"><h1><a href="#who" class="link" id="back">Who</a> / ${esc(state.label)} · ${periodLabel(state.period)}</h1>${periodSelector()}</div>
        <div class="chart-wrap"><div class="table-header"><h2>Over time</h2></div><div class="chart-box"><canvas id="tl"></canvas></div></div>
        <div class="table-wrap"><div class="table-header"><h2>Processes</h2></div>${whoTable('label-procs', procs, false)}</div>
        <div class="table-wrap"><div class="table-header"><h2>Destinations</h2></div>${whereTable(where, false)}</div>`;
    document.getElementById('back').addEventListener('click', e => { e.preventDefault(); state.label = ''; location.hash = 'who'; render(); });
    bindPeriodSelector();
    bindSort();
    timelineChart('tl', tl, [
        ['internet', 'out', 'Internet up', '#ef4444'], ['internet', 'in', 'Internet down', '#3b82f6'],
        ['lan', 'out', 'LAN up', '#a855f7'], ['unclassified', 'out', 'Unclassified up', '#f59e0b'],
    ]);
}

function whereTable(rows, showWho) {
    return `<div class="table-scroll"><table>
        <thead><tr><th>Where</th><th>Class</th>${showWho ? '<th>Who</th>' : ''}<th class="num">Up</th><th class="num">Down</th></tr></thead>
        <tbody>${(rows || []).map(w => `<tr>
            <td>${endpointCell(w)}</td>
            <td><span class="pill" style="background:${CLASS_INFO[w.class]?.color || '#555'}">${esc(CLASS_INFO[w.class]?.label || w.class)}</span></td>
            ${showWho ? `<td class="dimtext who-cell" title="${esc(w.labels)}">${esc(w.labels)}</td>` : ''}
            <td class="num up">${humanBytes(w.out)}</td><td class="num">${humanBytes(w.in)}</td>
        </tr>`).join('') || `<tr><td colspan="5" class="empty">No connections recorded</td></tr>`}</tbody>
    </table></div>`;
}

function destTable(rows) {
    return `<div class="table-scroll"><table>
        <thead><tr><th>Where</th><th>Who</th><th class="num">Up</th><th class="num">Down</th><th class="num">Not pinned on a process</th></tr></thead>
        <tbody>${(rows || []).map(d => `<tr>
            <td>${destCell(d)}</td>
            <td class="dimtext who-cell" title="${esc(d.labels)}">${esc(d.labels) || '<i>short-lived, process not seen</i>'}</td>
            <td class="num up">${humanBytes(d.out)}</td><td class="num">${humanBytes(d.in)}</td>
            <td class="num dimtext">${humanBytes(Math.max(0, d.out - d.attr_out))} ↑ ${humanBytes(Math.max(0, d.in - d.attr_in))} ↓</td>
        </tr>`).join('') || '<tr><td colspan="5" class="empty">Nothing yet</td></tr>'}</tbody>
    </table></div>`;
}

async function renderWhere(el) {
    const internet = state.whereClass === 'internet';
    const rows = internet
        ? await api(`/api/destinations?period=${state.period}&sort=${state.whereSort}&limit=200`)
        : await api(`/api/where?period=${state.period}&class=${state.whereClass}&sort=${state.whereSort}&limit=200`);
    const btn = (attr, v, cur, text) => `<button class="period-btn ${v === cur ? 'active' : ''}" data-${attr}="${v}">${text}</button>`;
    el.innerHTML = `
        <div class="table-header"><h1>Where · ${periodLabel(state.period)}</h1>${periodSelector()}</div>
        <div class="toolbar">
            <div class="period-selector">${['internet', 'lan', 'vpn', 'tailnet', ''].map(k => btn('class', k, state.whereClass, k ? CLASS_INFO[k].label : 'All')).join('')}</div>
            <div class="period-selector">${[['out', 'by upload'], ['in', 'by download'], ['total', 'by total']].map(([k, t]) => btn('sort', k, state.whereSort, t)).join('')}</div>
        </div>
        <p class="note">${internet
            ? `Internet bytes are <b>measured per address</b> from the route macOS keeps for each destination, so even connections too short to catch
               are here. <b>Who</b> lists the programs seen talking to it; <b>not pinned</b> is what no listed connection accounts for (short-lived connections, and TCP/IP overhead).`
            : `Connections seen per process. `}
            <b>served</b> means a remote device connected to a port on this Mac (its upload is someone downloading from here, e.g. through Caddy on :443).
            Owners come from reverse DNS and Team Cymru's IP-to-ASN service and fill in within a few minutes.</p>
        <div class="table-wrap">${internet ? destTable(rows) : whereTable(rows, true)}</div>`;
    bindPeriodSelector();
    document.querySelectorAll('[data-class]').forEach(b => b.addEventListener('click', () => { state.whereClass = b.dataset.class; render(); }));
    document.querySelectorAll('[data-sort]').forEach(b => b.addEventListener('click', () => { state.whereSort = b.dataset.sort; render(); }));
}

function renderAbout(el) {
    el.innerHTML = `
        <h1>How it's measured</h1>
        <div class="settings-section prose">
            <h3>Measured totals</h3>
            <p><b>Internet</b> is the byte count of the default route through the router (<code>nettop -m route</code>): everything this Mac
            sent to or received from beyond the LAN, whoever sent it. <b>LAN</b> is the physical interfaces' total minus that.
            <b>UTM VMs</b> come from their <code>vmenet</code> interfaces; a bridged VM has its own address and never passes through this
            Mac's network stack, so it appears nowhere else.</p>
            <h3>Attributed traffic</h3>
            <p>Two long-running <code>nettop</code> streams report bytes per process every few seconds: one for every interface except loopback,
            one for loopback only. Each open connection's remote address puts its bytes in a class: <b>Internet</b>, <b>LAN</b>,
            <b>VPN</b> (WireGuard clients), <b>Tailscale</b>. Bytes from connections that opened and closed between two updates are
            counted for the process but have no destination: <b>unclassified</b>.</p>
            <p><b>Host-internal</b> is loopback: processes on this Mac talking to each other. Containers and VMs generate a lot of it
            (Colima's network process relays every port forward over it), and it is not network traffic.</p>
            <h3>Who</h3>
            <p>Processes are labelled from their command line: a Lima VM is its <code>limactl hostagent</code> plus its ssh tunnels;
            <b>Docker (Colima VM)</b> is Colima's <code>usernet</code> process (containers' outbound traffic) plus the ssh multiplexer that
            carries published ports. Individual containers can't be told apart from the Mac.</p>
            <h3>Before 2026-10-19</h3>
            <p>Older data came from one-shot samples that mixed loopback with network traffic and booked phantom traffic whenever a
            connection closed. It is kept in the database (table <code>usage</code>) but not shown.</p>
        </div>
        <div class="settings-section"><h3>Collector</h3><div id="status-detail">Loading…</div></div>`;
    api('/api/status').then(s => {
        const c = s.collector;
        const t = x => x && !x.startsWith('0001') ? new Date(x).toLocaleString() : '—';
        document.getElementById('status-detail').innerHTML = `<table>
            <tr><td>Running since</td><td>${t(c.started)}</td></tr>
            <tr><td>Last write</td><td>${t(c.last_flush)} ${c.last_flush_error ? `<span class="up">${esc(c.last_flush_error)}</span>` : ''}</td></tr>
            <tr><td>Stream updates</td><td>external ${c.external_blocks} (last ${t(c.last_external)}) · loopback ${c.loopback_blocks} (last ${t(c.last_loopback)})</td></tr>
            <tr><td>Intervals</td><td>nettop every ${s.stream_interval}s · written every ${s.flush_interval}s</td></tr>
            <tr><td>Data since</td><td>${s.data_from ? new Date(s.data_from * 1000).toLocaleString() : '—'}</td></tr>
        </table>`;
    }).catch(() => {});
}

function bindLabelLinks() {
    document.querySelectorAll('[data-label]').forEach(td => td.addEventListener('click', () => {
        state.label = td.dataset.label;
        state.page = 'who';
        location.hash = 'who';
        render();
    }));
}

// series: [seriesKey, 'in'|'out', name, color]
function timelineChart(id, tl, series) {
    const ctx = document.getElementById(id);
    if (!ctx || typeof Chart === 'undefined') return;
    const pts = tl.points || [];
    const bucket = tl.bucket;
    const fmt = ts => {
        const d = new Date(ts * 1000);
        return bucket >= 86400 ? d.toLocaleDateString([], { month: 'short', day: 'numeric' })
            : bucket >= 3600 ? d.toLocaleString([], { weekday: 'short', hour: '2-digit' })
            : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    };
    charts.push(new Chart(ctx, {
        type: 'bar',
        data: {
            labels: pts.map(p => fmt(p.ts)),
            datasets: series.map(([k, dir, name, color]) => ({
                label: name, backgroundColor: color, borderWidth: 0,
                data: pts.map(p => (p.series[k] || {})[dir] || 0),
            })),
        },
        options: {
            responsive: true, maintainAspectRatio: false, animation: false,
            plugins: {
                legend: { labels: { color: '#8b8d97' } },
                tooltip: { callbacks: { label: c => `${c.dataset.label}: ${humanBytes(c.raw)} (${rate(c.raw, bucket)})` } },
            },
            scales: {
                x: { ticks: { color: '#8b8d97', maxTicksLimit: 12 }, grid: { display: false } },
                y: { ticks: { color: '#8b8d97', callback: v => humanBytes(v) }, grid: { color: '#2a2d37' } },
            },
        },
    }));
}

async function checkStatus() {
    const dot = document.querySelector('.status-dot'), text = document.querySelector('.status-text');
    try {
        const s = await api('/api/status');
        dot.className = `status-dot ${s.healthy ? 'online' : 'offline'}`;
        text.textContent = s.healthy ? 'Collecting' : 'Collector stalled';
    } catch {
        dot.className = 'status-dot offline';
        text.textContent = 'Offline';
    }
}

function fromHash() {
    const h = location.hash.replace('#', '');
    if (['overview', 'who', 'where', 'about'].includes(h)) {
        if (h !== state.page) state.label = '';
        state.page = h;
    }
}

window.addEventListener('hashchange', () => { fromHash(); render(); });
document.querySelectorAll('.nav-links a').forEach(a => a.addEventListener('click', () => {
    if (a.dataset.page === state.page && state.label) { state.label = ''; render(); }
}));
fromHash();
render();
checkStatus();
setInterval(checkStatus, 30000);
