// Live view over the ingestbench JSON-Lines event stream.
//
// Re-reads the whole file each poll instead of tracking an offset: the
// stream is small, and a stateless reload cannot drift out of sync with
// a run that restarted underneath it.

const POLL_MS = 1000;
const ARM = {
  jev:        { label: 'Jev',  color: '#ff5a00', chip: 'jev' },
  generative: { label: 'GLM',  color: '#3f3f46', chip: 'gen' },
};

let events = [], lastLen = -1;

async function poll() {
  try {
    const r = await fetch('events.jsonl?t=' + Date.now(), { cache: 'no-store' });
    if (r.ok) {
      const txt = await r.text();
      if (txt.length !== lastLen) {
        lastLen = txt.length;
        events = txt.trim().split('\n').filter(Boolean)
          .map(l => { try { return JSON.parse(l); } catch { return null; } })
          .filter(Boolean);
        render();
      } else { tickClock(); }
    }
  } catch {}
  setTimeout(poll, POLL_MS);
}

const detects  = () => events.filter(e => e.t === 'phase' && e.phase === 'detect');
const parses   = () => events.filter(e => e.t === 'phase' && e.phase === 'parse');
const starts   = () => events.filter(e => e.t === 'phase_start');
const byArm    = a => detects().filter(e => e.arm === a);
const armEnd   = a => events.find(e => e.t === 'arm_end' && e.arm === a);
const runEnd   = () => events.find(e => e.t === 'run_end');
const sum = (xs, f) => xs.reduce((a, x) => a + (f(x) || 0), 0);

const fmtS  = s => s >= 60 ? `${Math.floor(s/60)}m ${(s%60).toFixed(0)}s` : `${s.toFixed(1)}s`;
const fmtUSD = v => v >= 0.01 ? `$${v.toFixed(3)}` : `$${v.toFixed(5)}`;
const fmtN  = n => n.toLocaleString('en-US');

function tickClock() {
  const done = runEnd();
  const last = events.length ? events[events.length - 1].time : 0;
  document.getElementById('clock').textContent = done ? fmtS(done.seconds) : fmtS(last);
}

function render() { tickClock(); status(); kpis(); docgrid(); charts(); totals(); timeline(); log(); }

function status() {
  const el = document.getElementById('status');
  if (runEnd()) { el.innerHTML = '<span class="badge badge-outline">complete</span>'; return; }
  if (!events.length) { el.innerHTML = '<span class="badge badge-outline">waiting</span>'; return; }
  el.innerHTML = '<span class="badge badge-ember pulsing">live</span>';
}

function kpis() {
  const j = byArm('jev'), g = byArm('generative');
  const je = armEnd('jev'), ge = armEnd('generative');
  const jS = je ? je.seconds : sum(j, e => e.seconds);
  const gS = ge ? ge.seconds : sum(g, e => e.seconds);
  const speed = (jS > 0 && gS > 0) ? (gS / jS) : null;
  const jc = sum(j, e => e.cost_usd), gc = sum(g, e => e.cost_usd);
  const cheaper = (jc > 0 && gc > 0) ? (gc / jc) : null;
  const pr = parses();

  const cell = (v, l, ember) => `<div class="kpi"><div class="v${ember ? ' ember' : ''}">${v}</div><div class="l">${l}</div></div>`;
  document.getElementById('kpis').innerHTML =
    cell(`${pr.length}`, 'documents') +
    cell(fmtN(sum(pr, e => e.pages)), 'pages parsed') +
    cell(jS ? fmtS(jS) : '—', 'jev wall-clock', true) +
    cell(gS ? fmtS(gS) : '—', 'glm wall-clock') +
    cell(speed ? `${speed.toFixed(0)}×` : '—', speed ? 'faster' : 'speed-up', true);

  document.getElementById('docsmeta').textContent =
    cheaper ? `${cheaper.toFixed(0)}× cheaper · ${sum(j, e => e.requests)} vs ${sum(g, e => e.requests)} requests` : '';
}

function docgrid() {
  const pr = parses();
  const running = new Set(starts()
    .filter(s => !detects().some(d => d.doc === s.doc && d.arm === s.arm))
    .map(s => s.doc + '|' + s.arm));

  document.getElementById('docgrid').innerHTML = pr.map(p => {
    const d = detects().filter(x => x.doc === p.doc);
    const live = [...running].some(k => k.startsWith(p.doc + '|'));
    const chips = ['jev', 'generative'].map(a => {
      const e = d.find(x => x.arm === a);
      if (e) return `<span class="chip ${ARM[a].chip}">${ARM[a].label} ${e.seconds.toFixed(1)}s · ${e.requests}r</span>`;
      if (running.has(p.doc + '|' + a)) return `<span class="chip run pulsing">${ARM[a].label} running</span>`;
      return '';
    }).join('');

    const pct = (d.length / 2) * 100;
    return `<div class="doc ${live ? 'active' : d.length === 2 ? 'done' : ''}">
      <div class="n">${p.doc.replace(/_/g, ' ')}</div>
      <div class="m">${p.pages} pages · parsed ${p.seconds.toFixed(1)}s</div>
      <div class="track"><div class="fill" style="width:${pct}%;background:${live ? '#ff5a00' : '#18181b'}"></div></div>
      <div class="arms">${chips}</div>
    </div>`;
  }).join('') || '<p class="muted tiny">waiting for the first document…</p>';
}

// --- charts (hand-rolled SVG; no library, and none needed) ---

function hbars(elId, rows, unit, maxOverride) {
  const el = document.getElementById(elId);
  if (!rows.length) { el.innerHTML = '<p class="muted tiny">no data yet</p>'; return; }
  const max = maxOverride || Math.max(...rows.map(r => r.v), 1);
  const H = 22, GAP = 7, LW = 150, W = 640;
  const h = rows.length * (H + GAP);

  const bars = rows.map((r, i) => {
    const y = i * (H + GAP);
    const w = Math.max(2, (r.v / max) * (W - LW - 90));
    return `<text class="slbl" x="0" y="${y + 15}">${r.label}</text>
      <rect x="${LW}" y="${y}" width="${w}" height="${H}" rx="${H / 2}" fill="${r.color}"/>
      <text class="axlbl" x="${LW + w + 8}" y="${y + 15}">${r.t}</text>`;
  }).join('');

  el.innerHTML = `<svg class="chart" viewBox="0 0 ${W} ${h}" preserveAspectRatio="xMinYMin meet">${bars}</svg>
    <p class="tiny" style="margin-top:10px">${unit}</p>`;
}

function line(elId, series, yfmt, caption) {
  const el = document.getElementById(elId);
  const pts = series.flatMap(s => s.points);
  if (pts.length < 2) { el.innerHTML = '<p class="muted tiny">no data yet</p>'; return; }
  const W = 640, H = 220, P = 34;
  const maxX = Math.max(...pts.map(p => p[0]), 1);
  const maxY = Math.max(...pts.map(p => p[1]), 1e-9);
  const X = x => P + (x / maxX) * (W - P - 12);
  const Y = y => H - P - (y / maxY) * (H - P - 14);

  const paths = series.filter(s => s.points.length > 1).map(s =>
    `<path d="${s.points.map((p, i) => `${i ? 'L' : 'M'}${X(p[0]).toFixed(1)},${Y(p[1]).toFixed(1)}`).join(' ')}"
       fill="none" stroke="${s.color}" stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round"/>`).join('');

  const dots = series.map(s => s.points.length
    ? `<circle cx="${X(s.points.at(-1)[0])}" cy="${Y(s.points.at(-1)[1])}" r="4" fill="${s.color}"/>` : '').join('');

  let ticks = '';
  for (let i = 0; i <= 3; i++) {
    const v = (maxY / 3) * i, y = Y(v);
    ticks += `<line class="ax" x1="${P}" y1="${y}" x2="${W - 12}" y2="${y}"/>
      <text class="axlbl" x="0" y="${y + 3}">${yfmt(v)}</text>`;
  }
  const legend = series.map(s =>
    `<span class="legend-item"><span class="swatch" style="background:${s.color}"></span>${s.label}</span>`).join('');

  el.innerHTML = `<div class="legend">${legend}</div>
    <svg class="chart" viewBox="0 0 ${W} ${H}">${ticks}${paths}${dots}
      <text class="axlbl" x="${P}" y="${H - 12}">0s</text>
      <text class="axlbl" x="${W - 40}" y="${H - 12}">${fmtS(maxX)}</text>
    </svg><p class="tiny">${caption}</p>`;
}

function charts() {
  const docs = [...new Set(detects().map(e => e.doc))].sort();

  const lat = [], req = [];
  for (const d of docs) for (const a of ['jev', 'generative']) {
    const e = detects().find(x => x.doc === d && x.arm === a);
    if (!e) continue;
    const short = d.replace(/_/g, ' ').replace(/ 10K$/, '');
    lat.push({ label: `${short} · ${ARM[a].label}`, v: e.seconds, t: fmtS(e.seconds), color: ARM[a].color });
    req.push({ label: `${short} · ${ARM[a].label}`, v: e.requests, t: `${e.requests}`, color: ARM[a].color });
  }
  hbars('chart-latency', lat, 'detection phase only; parsing is shared and excluded');
  hbars('chart-requests', req, 'one batched request vs one call per scanned page');

  // Cumulative cost and completion, both over the shared run clock.
  const cum = arm => {
    const es = byArm(arm).slice().sort((a, b) => a.time - b.time);
    let c = 0;
    return es.map(e => { c += (e.cost_usd || 0); return [e.time, c]; });
  };
  const done = arm => {
    const es = byArm(arm).slice().sort((a, b) => a.time - b.time);
    return es.map((e, i) => [e.time, i + 1]);
  };
  line('chart-cost', [
    { label: 'Jev', color: ARM.jev.color, points: cum('jev') },
    { label: 'GLM', color: ARM.generative.color, points: cum('generative') },
  ], v => fmtUSD(v), 'cost accrued over the run');
  line('chart-throughput', [
    { label: 'Jev', color: ARM.jev.color, points: done('jev') },
    { label: 'GLM', color: ARM.generative.color, points: done('generative') },
  ], v => v.toFixed(0), 'documents finished, against the run clock');
}

function totals() {
  const rows = ['jev', 'generative'].map(a => {
    const e = byArm(a); if (!e.length) return '';
    const c = sum(e, x => x.cost_usd);
    return `<tr>
      <td><span class="badge ${a === 'jev' ? 'badge-ember' : 'badge-filled'}">${ARM[a].label}</span></td>
      <td class="num">${e.length}</td>
      <td class="num">${sum(e, x => x.requests)}</td>
      <td class="num">${fmtN(sum(e, x => x.in_tokens))}</td>
      <td class="num">${fmtS(sum(e, x => x.seconds))}</td>
      <td class="num">${c > 0 ? fmtUSD(c) : '—'}</td>
      <td class="num">${c > 0 ? fmtUSD(c / e.length) : '—'}</td>
      <td class="num">${c > 0 ? fmtUSD(c / e.length * 10000) : '—'}</td>
    </tr>`;
  }).join('');
  document.getElementById('cost').innerHTML = `<table>
    <thead><tr><th>arm</th><th>docs</th><th>req</th><th>tokens</th><th>model time</th><th>cost</th><th>per doc</th><th>per 10k docs</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="8" class="muted">no data yet</td></tr>'}</tbody></table>`;
}

function timeline() {
  const d = detects();
  const el = document.getElementById('timeline');
  if (!d.length) { el.innerHTML = '<p class="muted tiny">nothing scheduled yet</p>'; return; }
  const maxT = Math.max(...d.map(e => e.time), 1);
  const items = d.map(e => {
    const s = starts().find(x => x.doc === e.doc && x.arm === e.arm);
    return { ...e, t0: s ? s.time : e.time - e.seconds };
  }).sort((a, b) => a.t0 - b.t0);

  const lanes = [];
  let bars = '';
  for (const it of items) {
    let l = lanes.findIndex(end => end <= it.t0 + 0.01);
    if (l === -1) { lanes.push(0); l = lanes.length - 1; }
    lanes[l] = it.time;
    bars += `<div class="tl-lane" title="${it.doc} · ${it.arm} · ${it.seconds.toFixed(1)}s"
      style="left:${(it.t0 / maxT) * 100}%;width:${Math.max(.5, ((it.time - it.t0) / maxT) * 100)}%;
      top:${10 + l * 22}px;background:${ARM[it.arm].color}"></div>`;
  }
  let ticks = '';
  for (let i = 0; i <= 4; i++) ticks += `<div class="tl-tick" style="left:${(i / 4) * 100}%">${fmtS((i / 4) * maxT)}</div>`;
  el.innerHTML = `<div class="tl" style="height:${Math.max(120, lanes.length * 22 + 50)}px">${bars}<div class="tl-axis">${ticks}</div></div>`;
}

function log() {
  document.getElementById('log').innerHTML = events.slice(-30).reverse().map(e => {
    const t = `<span class="d">${e.time.toFixed(1)}s</span>`;
    if (e.t === 'phase' && e.phase === 'detect')
      return `${t} <span class="k">${e.arm}</span> ${e.doc} · ${e.seconds.toFixed(1)}s · ${e.requests}r · ${fmtUSD(e.cost_usd || 0)}`;
    if (e.t === 'phase' && e.phase === 'parse')
      return `${t} <span class="d">parse</span> ${e.doc} · ${e.pages}p · ${e.seconds.toFixed(1)}s`;
    if (e.t === 'phase_start') return `${t} <span class="d">start</span> ${e.arm} ${e.doc}`;
    if (e.t === 'arm_start')  return `${t} <span class="k">── ${e.arm} ──</span>`;
    if (e.t === 'arm_end')    return `${t} <span class="k">${e.arm} done</span> ${fmtS(e.seconds)}`;
    if (e.t === 'run_end')    return `${t} <span class="k">run complete</span> ${fmtS(e.seconds)}`;
    if (e.t === 'run_start')  return `${t} ${e.note}`;
    return `${t} ${e.t}`;
  }).join('<br>');
}

poll();
