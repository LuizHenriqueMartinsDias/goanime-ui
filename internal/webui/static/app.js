'use strict';

const $ = (sel, el = document) => el.querySelector(sel);
const view = $('#view');

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const ICON = {
  play: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>',
  check: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg>',
  download: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 4v11"/><path d="m7 10 5 5 5-5"/><path d="M5 20h14"/></svg>',
  heart: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 20s-7-4.4-9-9a4.8 4.8 0 0 1 9-3 4.8 4.8 0 0 1 9 3c-2 4.6-9 9-9 9z"/></svg>',
  x: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18"/></svg>',
  back: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m15 18-6-6 6-6"/></svg>',
  search: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></svg>',
};

// Flat tones for covers that have no art (or while it loads).
const TONES = [
  ['#24485A', '#CFE8F2'], ['#4A2E5C', '#E6D6F2'], ['#2E3566', '#D8DCF7'], ['#2F4A35', '#D5EBD9'],
  ['#5A4A26', '#F2E6C9'], ['#5C2A2A', '#F5D5D5'], ['#2A4F4F', '#D2EFEF'], ['#3A3550', '#E0DCF0'],
];

function tone(s) {
  let h = 0;
  for (const c of s) h = (h * 31 + c.codePointAt(0)) | 0;
  return TONES[Math.abs(h) % TONES.length];
}

function initials(title) {
  const words = title.replace(/[^\p{L}\p{N} ]/gu, ' ').trim().split(/\s+/);
  return ((words[0] || '').slice(0, 1) + (words[1] || '').slice(0, 1)).toUpperCase();
}

function cover(card, kind, extra = '', eager = false) {
  const [bg, ink] = tone(card.title);
  // TMDB art comes at full resolution; a poster-sized variant loads far faster.
  const src = (card.image || '').replace('/t/p/original/', '/t/p/w500/');
  const img = src
    ? `<img src="/api/img?u=${encodeURIComponent(src)}" alt=""${eager ? '' : ' loading="lazy"'} onerror="this.remove()">`
    : '';
  return `<div class="cover ${kind}" style="--tone:${bg};--ink:${ink}"><span class="initials" aria-hidden="true">${esc(initials(card.title))}</span>${img}${extra}</div>`;
}

const pct = (pos, dur) => (dur > 0 ? Math.min(100, Math.round((pos / dur) * 100)) : 0);
const minutesLeft = (pos, dur) => `${Math.max(1, Math.round((dur - pos) / 60))} min restantes`;

function fmtBytes(n) {
  if (!n) return '';
  const units = ['B', 'KB', 'MB', 'GB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i > 1 ? 1 : 0).replace('.', ',')} ${units[i]}`;
}

async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: {} };
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  } else if (init.method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
  }
  const r = await fetch(path, init);
  if (!r.ok) {
    let msg = `Erro ${r.status}`;
    try { msg = (await r.json()).error || msg; } catch { /* plain-text error */ }
    const err = new Error(msg);
    err.status = r.status;
    throw err;
  }
  if (r.status === 202 || r.status === 204) return null;
  return r.json();
}

// Per-viewer preferences; the page works the same if storage is unavailable.
const prefs = {
  get(key, fallback) {
    try { return localStorage.getItem(key) ?? fallback; } catch { return fallback; }
  },
  set(key, value) {
    try { localStorage.setItem(key, value); } catch { /* ignore */ }
  },
};
const quality = () => prefs.get('quality', 'best');
const subPref = () => prefs.get('subs', 'pt');

let toastTimer;
function toast(msg, bad = false) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = bad ? 'toast bad' : 'toast';
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, bad ? 9000 : 4000);
}

/* ---------- Router ---------- */

function parseRoute() {
  const h = location.hash.replace(/^#\/?/, '');
  const [path, qs] = h.split('?');
  const parts = path.split('/');
  return { name: parts[0] || '', arg: parts[1] ? decodeURIComponent(parts[1]) : '', params: new URLSearchParams(qs || '') };
}

let renderToken = 0;
const routes = { '': homeView, search: searchView, favorites: favoritesView, anime: animeView, downloads: downloadsView };

async function router(soft = false) {
  const r = parseRoute();
  const token = ++renderToken;
  const navName = r.name === 'anime' ? '' : r.name;
  document.querySelectorAll('.nav-link').forEach((a) => {
    const route = a.dataset.route === 'home' ? '' : a.dataset.route;
    if (route === navName && r.name !== 'anime') a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  if (!soft) window.scrollTo(0, 0);
  await (routes[r.name] || homeView)(r, () => token === renderToken, soft);
}
window.addEventListener('hashchange', () => router());

/* ---------- Shared pieces ---------- */

function searchForm(q) {
  return `
<form class="searchbar" role="search" data-search>
  <label for="busca" class="sr-only">Buscar anime</label>
  <div class="field">${ICON.search}<input id="busca" name="q" type="search" value="${esc(q)}" placeholder="Buscar anime… ex: frieren, one piece" autocomplete="off"></div>
  <button class="btn btn-primary btn-tall" type="submit">Buscar</button>
</form>`;
}

function posterCard(card, extraMeta = '', fav = false) {
  const tag = fav
    ? `<span class="fav-mark">${ICON.heart}</span>`
    : `<span class="tag left">${esc(card.source)}</span>`;
  const meta = extraMeta || [card.tags?.join(' · '), card.episodes ? `${card.episodes} episódios` : ''].filter(Boolean).join(' · ');
  return `
<a class="card" href="#/anime/${encodeURIComponent(card.id)}">
  ${cover(card, 'poster', tag)}
  <div class="card-text"><span class="card-title">${esc(card.title)}</span><span class="card-meta">${esc(meta)}</span></div>
</a>`;
}

function emptyState(title, body, action = '') {
  return `<div class="empty"><strong>${esc(title)}</strong><span>${esc(body)}</span>${action}</div>`;
}

function skeletonGrid(n, ratio = '2 / 3') {
  return `<div class="grid posters">${Array.from({ length: n }, () => `<div class="skeleton" style="aspect-ratio:${ratio}"></div>`).join('')}</div>`;
}

/* ---------- Home ---------- */

async function homeView(r, current, soft) {
  if (!soft) view.innerHTML = searchForm('') + '<div class="skeleton" style="height:260px;border-radius:24px"></div>';
  let data;
  try {
    data = await api('/api/home');
  } catch (e) {
    if (current()) view.innerHTML = searchForm('') + `<div class="error-box">${esc(e.message)}</div>`;
    return;
  }
  if (!current()) return;

  const cont = data.continue;
  const favs = data.favorites;
  let html = searchForm('');

  if (cont.length) {
    const h = cont[0];
    const resuming = h.position > 0 && h.duration > 0;
    const meter = resuming
      ? `<div class="meter"><div class="track"><div style="width:${pct(h.position, h.duration)}%"></div></div><span>${minutesLeft(h.position, h.duration)}</span></div>`
      : '';
    html += `
<section class="hero" aria-labelledby="hero-title">
  ${cover(h, 'poster', '', true)}
  <div class="hero-body">
    <span class="eyebrow">${resuming ? 'Continue de onde parou' : 'Próximo episódio'}</span>
    <h1 id="hero-title">${esc(h.title)}</h1>
    <p class="muted" style="margin:0;font-size:16px">Episódio ${esc(h.next)} · ${esc(h.source)}</p>
    ${meter}
    <div class="actions">
      <button class="btn btn-primary" type="button" data-action="play" data-id="${esc(h.id)}" data-ep="${esc(h.next)}">${ICON.play}${resuming ? 'Continuar' : 'Assistir'} EP ${esc(h.next)}</button>
      <a class="btn" href="#/anime/${encodeURIComponent(h.id)}">Ver episódios</a>
    </div>
  </div>
</section>`;
  }

  if (cont.length > 1) {
    html += `
<section class="section" aria-labelledby="cont-title">
  <div class="section-head"><h2 id="cont-title">Continuar assistindo</h2></div>
  <div class="grid wides">${cont.slice(1).map(continueCard).join('')}</div>
</section>`;
  }

  html += `
<section class="section" aria-labelledby="fav-title">
  <div class="section-head"><h2 id="fav-title">Favoritos</h2>${favs.length > 12 ? '<a class="link-muted" href="#/favorites">Ver todos</a>' : ''}</div>
  ${favs.length
    ? `<div class="grid posters">${favs.slice(0, 12).map((f) => posterCard(f, '', true)).join('')}</div>`
    : emptyState('Nenhum favorito ainda', 'Abra um anime e toque em Favoritar para guardá-lo aqui.')}
</section>`;

  if (!cont.length && !favs.length) {
    html = searchForm('') + emptyState('Bem-vindo ao GoAnime', 'Busque um anime para começar. O que você assistir aparece aqui para continuar depois.', '<a class="btn btn-primary" href="#/search">Buscar anime</a>');
  }
  view.innerHTML = html;
}

function continueCard(c) {
  const resuming = c.position > 0 && c.duration > 0;
  const bar = resuming ? `<div class="progress"><div style="width:${pct(c.position, c.duration)}%"></div></div>` : '';
  return `
<a class="card" href="#/anime/${encodeURIComponent(c.id)}">
  ${cover(c, 'wide', `<span class="tag right">EP ${esc(c.next)}</span>${bar}`)}
  <div class="card-text"><span class="card-title">${esc(c.title)}</span><span class="card-meta">${esc(c.source)} · ${resuming ? minutesLeft(c.position, c.duration) : 'próximo episódio'}</span></div>
</a>`;
}

/* ---------- Search ---------- */

let sourcesCache;

async function searchView(r, current) {
  const q = r.params.get('q') || '';
  const src = r.params.get('source') || 'todas';
  if (!sourcesCache) {
    try { sourcesCache = await api('/api/sources'); } catch { sourcesCache = []; }
    if (!current()) return;
  }
  const chips = [{ key: 'todas', label: 'Todas' }, ...sourcesCache]
    .map((s) => `<button type="button" class="chip" aria-pressed="${s.key === src}" data-action="source" data-source="${esc(s.key)}">${esc(s.label)}</button>`)
    .join('');
  const q8 = quality();
  const opts = [['best', 'Melhor qualidade'], ['1080p', '1080p'], ['720p', '720p'], ['480p', '480p']]
    .map(([v, l]) => `<option value="${v}"${v === q8 ? ' selected' : ''}>${l}</option>`).join('');

  view.innerHTML = `
<form class="searchbar" role="search" data-search>
  <label for="busca" class="sr-only">Buscar anime</label>
  <div class="field">${ICON.search}<input id="busca" name="q" type="search" value="${esc(q)}" placeholder="Buscar anime… ex: frieren, one piece" autocomplete="off"></div>
  <label for="qualidade" class="sr-only">Qualidade</label>
  <select id="qualidade" class="select" data-quality>${opts}</select>
  <button class="btn btn-primary btn-tall" type="submit">Buscar</button>
</form>
<div class="chips" role="group" aria-label="Fonte"><span class="chips-label">Fonte</span>${chips}</div>
<section class="section" id="results" aria-live="polite"></section>`;

  const results = $('#results');
  if (!q) {
    $('#busca').focus();
    results.innerHTML = emptyState('O que vamos assistir?', 'Digite o nome de um anime. Títulos em português vêm do AnimeFire; em inglês, do HiAnime.');
    return;
  }
  results.innerHTML = `<div class="section-head"><h1 style="font-weight:700;font-size:28px">Buscando “${esc(q)}”…</h1></div>${skeletonGrid(8)}`;
  let list;
  try {
    list = await api(`/api/search?q=${encodeURIComponent(q)}&source=${encodeURIComponent(src)}`);
  } catch (e) {
    if (current()) results.innerHTML = `<div class="error-box">A busca falhou: ${esc(e.message)}</div>`;
    return;
  }
  if (!current()) return;
  if (!list.length) {
    results.innerHTML = emptyState(`Nada encontrado para “${q}”`, 'Tente o nome original em japonês ou em inglês, ou outra fonte.');
    return;
  }
  results.innerHTML = `
<div class="section-head"><h1 style="font-weight:700;font-size:28px">Resultados para “${esc(q)}”</h1><span class="muted" style="font-size:14px">${list.length} encontrados</span></div>
<div class="grid posters">${list.map((c) => posterCard(c)).join('')}</div>`;
}

/* ---------- Favorites ---------- */

async function favoritesView(r, current) {
  view.innerHTML = `<h1 class="page-title">Favoritos</h1>${skeletonGrid(6)}`;
  let data;
  try { data = await api('/api/home'); } catch (e) {
    if (current()) view.innerHTML = `<h1 class="page-title">Favoritos</h1><div class="error-box">${esc(e.message)}</div>`;
    return;
  }
  if (!current()) return;
  view.innerHTML = `<div class="section-head"><h1 class="page-title">Favoritos</h1><span class="muted">${data.favorites.length} animes</span></div>` +
    (data.favorites.length
      ? `<div class="grid posters">${data.favorites.map((f) => posterCard(f, '', true)).join('')}</div>`
      : emptyState('Nenhum favorito ainda', 'Abra um anime e toque em Favoritar para guardá-lo aqui.', '<a class="btn btn-primary" href="#/search">Buscar anime</a>'));
}

/* ---------- Anime detail ---------- */

const backButton = `<button class="back" type="button" data-action="back">${ICON.back}Voltar</button>`;
let animeState = null; // last /api/anime response, for the download range

async function animeView(r, current, soft) {
  const id = r.arg;
  if (!soft) {
    view.innerHTML = `${backButton}
<div class="detail-head"><div class="skeleton" style="flex:0 1 230px;min-width:170px;aspect-ratio:2/3;border-radius:18px"></div>
<div class="detail-body"><div class="skeleton" style="height:56px;max-width:520px"></div><p class="muted">Carregando episódios…</p></div></div>`;
  }
  let d;
  try {
    d = await api(`/api/anime/${encodeURIComponent(id)}`);
  } catch (e) {
    if (!current()) return;
    view.innerHTML = `${backButton}<div class="error-box">${esc(e.message)}</div>`;
    return;
  }
  if (!current()) return;
  animeState = d;

  const a = d.anime;
  const eps = d.episodes;
  const first = eps[0]?.label;
  const next = d.next || first;
  const resuming = d.next && d.next === d.lastEp && d.position > 0;
  const lastIdx = eps.findIndex((e) => e.label === d.lastEp);
  let status = 'Nenhum episódio assistido ainda.';
  if (d.lastEp && resuming) status = `Você parou no episódio ${d.lastEp}, faltando ${minutesLeft(d.position, d.duration).replace(' restantes', '')}.`;
  else if (d.lastEp && lastIdx === eps.length - 1) status = 'Você chegou ao último episódio disponível.';
  else if (d.lastEp) status = `Último assistido: episódio ${d.lastEp}. Próximo: ${next}.`;
  const desc = (d.description || '').replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();

  const pills = [a.source, ...(a.tags || []), `${eps.length} episódios`, ...(d.genres || []).slice(0, 3)]
    .map((p) => `<span class="pill">${esc(p)}</span>`).join('');

  const epCards = eps.map((e) => {
    const isCurrent = e.label === next && !e.watched;
    const cls = e.watched ? 'watched' : isCurrent ? 'current' : '';
    const icon = e.watched ? ICON.check : ICON.play;
    const name = `EP ${e.label}`;
    const mini = isCurrent && resuming ? `<div class="mini"><div style="width:${pct(d.position, d.duration)}%"></div></div>` : '';
    const label = `${e.watched ? 'Assistir novamente' : 'Assistir'} episódio ${e.label}${e.title ? `: ${e.title}` : ''}`;
    return `
<div class="ep ${cls}"${e.title ? ` title="${esc(e.title)}"` : ''}>
  <div class="ep-row">
    <button type="button" class="ep-play" data-action="play" data-id="${esc(a.id)}" data-ep="${esc(e.label)}" aria-label="${esc(label)}">${icon}<span class="ep-name">${esc(name)}</span></button>
    <button type="button" class="ep-dl" data-action="download" data-id="${esc(a.id)}" data-ep="${esc(e.label)}" aria-label="Baixar episódio ${esc(e.label)}">${ICON.download}</button>
  </div>${mini}
</div>`;
  }).join('');

  const fromDefault = Number.parseFloat(next) || 1;
  const toDefault = Number.parseFloat(eps[eps.length - 1]?.label) || fromDefault;

  view.innerHTML = `${backButton}
<section class="detail-head">
  ${cover(a, 'poster', '', true)}
  <div class="detail-body">
    <div class="pills">${pills}</div>
    <h1>${esc(a.title)}</h1>
    ${desc ? `<p class="desc">${esc(desc)}</p>` : ''}
    <p class="muted" style="margin:0">${esc(status)}</p>
    <div class="actions">
      ${next ? `<button class="btn btn-primary" type="button" data-action="play" data-id="${esc(a.id)}" data-ep="${esc(next)}">${ICON.play}${resuming ? 'Continuar' : 'Assistir'} EP ${esc(next)}</button>` : ''}
      <button class="btn fav-btn" type="button" data-action="fav" data-id="${esc(a.id)}" aria-pressed="${d.favorite}">${ICON.heart}${d.favorite ? 'Nos favoritos' : 'Favoritar'}</button>
      ${d.lastEp ? `<button class="link-muted" type="button" data-action="forget" data-id="${esc(a.id)}">Limpar progresso</button>` : ''}
    </div>
  </div>
</section>
<section class="section" aria-labelledby="eps-title">
  <div class="section-head" style="align-items:center">
    <h2 id="eps-title">Episódios</h2>
    <form class="range" data-range>
      <span class="muted" style="font-size:14px;font-weight:600">Baixar</span>
      <label for="de" class="sr-only">Do episódio</label>
      <input id="de" name="from" type="number" min="0" step="any" value="${fromDefault}">
      <span class="muted" style="font-size:14px">até</span>
      <label for="ate" class="sr-only">Até o episódio</label>
      <input id="ate" name="to" type="number" min="0" step="any" value="${toDefault}">
      <button class="btn-light" type="submit" style="display:inline-flex;align-items:center;gap:6px">${ICON.download}Baixar intervalo</button>
    </form>
  </div>
  ${eps.length ? `<div class="eps">${epCards}</div>` : emptyState('Nenhum episódio encontrado', 'A fonte não listou episódios para este título.')}
</section>`;
}

/* ---------- Downloads ---------- */

async function downloadsView(r, current, soft) {
  if (!soft) view.innerHTML = '<h1 class="page-title">Downloads</h1><div class="skeleton" style="height:120px"></div>';
  let d;
  try { d = await api('/api/downloads'); } catch (e) {
    if (current()) view.innerHTML = `<h1 class="page-title">Downloads</h1><div class="error-box">${esc(e.message)}</div>`;
    return;
  }
  if (!current()) return;
  renderDownloads(d);
}

function jobStatus(j) {
  switch (j.state) {
    case 'queued': return 'Na fila';
    case 'resolving': return 'Obtendo o link do vídeo…';
    case 'downloading': return `${Math.round(j.progress * 100)}%${j.speed ? ` · ${fmtBytes(j.speed)}/s` : ''}`;
    case 'error': return j.error;
    default: return j.state;
  }
}

function renderDownloads(d) {
  const jobs = d.jobs.map((j) => `
<div class="job${j.state === 'error' ? ' error' : ''}">
  <div class="job-num">${esc(j.episode)}</div>
  <div class="job-body">
    <div class="job-top"><span class="job-title">${esc(j.title)} · EP ${esc(j.episode)}</span><span class="muted job-status" style="font-size:14px">${esc(jobStatus(j))}</span></div>
    ${j.state === 'error' ? '' : `<div class="track"><div style="width:${Math.round(j.progress * 100)}%"></div></div>`}
  </div>
  <button type="button" class="icon-btn" data-action="cancel" data-job="${j.id}" aria-label="${j.state === 'error' ? 'Remover' : 'Cancelar'} download de ${esc(j.title)} episódio ${esc(j.episode)}">${ICON.x}</button>
</div>`).join('');

  const files = d.files.map((f) => `
<div class="file">
  <svg class="check" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg>
  <span class="file-name">${esc(f.name)}</span>
  <span class="muted" style="font-size:14px">${esc(fmtBytes(f.size))}</span>
  <button type="button" class="btn-soft" data-action="playfile" data-path="${esc(f.path)}">${ICON.play}Assistir</button>
</div>`).join('');

  view.innerHTML = `
<div class="section-head" style="align-items:flex-end">
  <div style="display:flex;flex-direction:column;gap:6px"><h1 class="page-title">Downloads</h1><span class="muted" style="font-size:15px;overflow-wrap:anywhere">Salvos em ${esc(d.dir)}</span></div>
  <button type="button" class="btn" data-action="openfolder">Abrir pasta</button>
</div>
<section class="section" aria-labelledby="fila"><h2 id="fila" class="label-caps">Em andamento</h2>
  ${jobs || emptyState('Nenhum download em andamento', 'Na página de um anime, use o ícone de download de um episódio ou “Baixar intervalo”.')}
</section>
<section class="section" aria-labelledby="prontos"><h2 id="prontos" class="label-caps">Concluídos</h2>
  ${files ? `<div class="files">${files}</div>` : emptyState('Nada baixado ainda', 'Os episódios baixados aparecem aqui para assistir offline.')}
</section>`;
}

/* ---------- Events ---------- */

view.addEventListener('submit', async (e) => {
  const form = e.target;
  if (form.matches('[data-search]')) {
    e.preventDefault();
    const q = new FormData(form).get('q').trim();
    if (!q) return;
    const src = parseRoute().params.get('source') || 'todas';
    location.hash = `#/search?q=${encodeURIComponent(q)}&source=${encodeURIComponent(src)}`;
  } else if (form.matches('[data-range]')) {
    e.preventDefault();
    if (!animeState) return;
    const fd = new FormData(form);
    const from = Number.parseFloat(fd.get('from'));
    const to = Number.parseFloat(fd.get('to'));
    const labels = animeState.episodes.map((x) => x.label).filter((l) => {
      const n = Number.parseFloat(l);
      return n >= Math.min(from, to) && n <= Math.max(from, to);
    });
    if (!labels.length) { toast('Nenhum episódio nesse intervalo.', true); return; }
    await enqueue(animeState.anime.id, labels);
  }
});

view.addEventListener('change', (e) => {
  if (e.target.matches('[data-quality]')) {
    prefs.set('quality', e.target.value);
    toast(`Qualidade: ${e.target.selectedOptions[0].textContent}`);
  }
});

async function enqueue(id, labels) {
  try {
    await api('/api/downloads', { method: 'POST', body: { id, episodes: labels, quality: quality() } });
    toast(labels.length === 1 ? `EP ${labels[0]} adicionado aos downloads.` : `${labels.length} episódios adicionados aos downloads.`);
    poll();
  } catch (err) { toast(err.message, true); }
}

view.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-action]');
  if (!b) return;
  const { action, id, ep } = b.dataset;
  try {
    switch (action) {
      case 'back':
        if (history.length > 1) history.back(); else location.hash = '#/search';
        break;
      case 'source': {
        const p = parseRoute().params;
        p.set('source', b.dataset.source);
        location.hash = `#/search?${p}`;
        break;
      }
      case 'play':
        b.disabled = true;
        await api('/api/play', { method: 'POST', body: { id, episode: ep, quality: quality(), subs: subPref() } });
        toast(`Abrindo EP ${ep} no mpv…`);
        setTimeout(() => { b.disabled = false; }, 1500);
        poll();
        break;
      case 'download':
        await enqueue(id, [ep]);
        break;
      case 'fav': {
        const on = b.getAttribute('aria-pressed') !== 'true';
        await api(`/api/anime/${encodeURIComponent(id)}/favorite`, { method: 'POST', body: { favorite: on } });
        b.setAttribute('aria-pressed', String(on));
        b.innerHTML = `${ICON.heart}${on ? 'Nos favoritos' : 'Favoritar'}`;
        toast(on ? 'Adicionado aos favoritos.' : 'Removido dos favoritos.');
        break;
      }
      case 'forget':
        await api(`/api/anime/${encodeURIComponent(id)}/history`, { method: 'DELETE' });
        toast('Progresso apagado.');
        router(true);
        break;
      case 'cancel':
        await api(`/api/downloads/${b.dataset.job}`, { method: 'DELETE' });
        poll();
        break;
      case 'playfile':
        await api('/api/downloads/play', { method: 'POST', body: { path: b.dataset.path } });
        toast('Abrindo no mpv…');
        break;
      case 'openfolder':
        await api('/api/downloads/open', { method: 'POST' });
        break;
    }
  } catch (err) {
    b.disabled = false;
    toast(err.message, true);
  }
});

/* ---------- Live status ---------- */

let lastPlayer = { state: 'idle' };

function renderPlayer(s) {
  const dot = $('#player-dot');
  const text = $('#player-text');
  const bar = $('#player-bar');
  dot.className = `dot ${s.state}`;
  const what = s.title ? `${s.title} · EP ${s.episode}` : '';
  text.textContent = {
    idle: 'mpv pronto',
    resolving: `Carregando ${what}…`,
    playing: `Tocando ${what}`,
    error: 'Não foi possível tocar',
  }[s.state] || s.state;
  bar.hidden = !(s.state === 'playing' && s.duration > 0);
  if (!bar.hidden) $('#player-fill').style.width = `${pct(s.position, s.duration)}%`;
}

/* ---------- Subtitles ---------- */

// The picker has two modes. While an episode with subtitle tracks plays it
// lists those tracks and switches mpv live; otherwise it holds the language
// the next episode starts with.
const SUB_PREFS = [['pt', 'Português'], ['en', 'Inglês'], ['off', 'Desligada']];
const subSelect = $('#sub-select');
let subOptionsKey = '';

function subName(label) {
  const l = label.toLowerCase();
  if (l.includes('portug')) return l.includes('brazil') ? 'Português (BR)' : 'Português';
  if (l.includes('english')) return 'Inglês';
  if (l.includes('spanish')) return 'Espanhol';
  return label;
}

// The preference a live pick implies, so the next episode starts the same way.
function prefFor(label) {
  const l = label.toLowerCase();
  if (l.includes('portug')) return 'pt';
  if (l.includes('english')) return 'en';
  return null;
}

function renderSubs(s) {
  const live = s.state === 'playing' && s.subs?.length > 0;
  const opts = live ? [...s.subs.map((t) => [String(t.id), subName(t.label)]), ['0', 'Desligada']] : SUB_PREFS;
  const key = `${live}|${opts.map((o) => o.join('=')).join('|')}`;
  if (key !== subOptionsKey) {
    subOptionsKey = key;
    subSelect.innerHTML = opts.map(([v, l]) => `<option value="${esc(v)}">${esc(l)}</option>`).join('');
    subSelect.dataset.mode = live ? 'live' : 'pref';
  }
  if (document.activeElement !== subSelect) subSelect.value = live ? String(s.sid) : subPref();

  // Tracks show up on the first watch tick, which also reports the duration.
  const hint = $('#sub-hint');
  hint.hidden = live || !(s.state === 'playing' && s.duration > 0);
  hint.textContent = 'Este episódio não tem faixas de legenda; se for legendado, ela já vem no vídeo.';
}

subSelect.addEventListener('change', async () => {
  const opt = subSelect.selectedOptions[0];
  if (subSelect.dataset.mode === 'pref') {
    prefs.set('subs', subSelect.value);
    toast(`Legenda: ${opt.textContent}. Vale a partir do próximo episódio.`);
    return;
  }
  const sid = Number(subSelect.value);
  try {
    await api('/api/player/subtitle', { method: 'POST', body: { sid } });
    const track = lastPlayer.subs?.find((t) => t.id === sid);
    const pref = sid === 0 ? 'off' : track && prefFor(track.label);
    if (pref) prefs.set('subs', pref);
    toast(`Legenda: ${opt.textContent}`);
  } catch (err) {
    toast(err.message, true);
  }
  subSelect.blur();
});

async function poll() {
  try {
    const [p, d] = await Promise.all([api('/api/player'), api('/api/downloads')]);
    renderPlayer(p);
    renderSubs(p);
    if (p.state === 'error' && lastPlayer.state !== 'error') toast(p.error, true);
    const route = parseRoute().name;
    // Playback ended: refresh pages that show progress.
    if (lastPlayer.state === 'playing' && p.state === 'idle' && (route === '' || route === 'anime')) router(true);
    lastPlayer = p;

    const active = d.jobs.filter((j) => j.state !== 'error').length;
    const badge = $('#dl-badge');
    badge.hidden = active === 0;
    badge.textContent = String(active);
    if (route === 'downloads') renderDownloads(d);
  } catch { /* server restarting; try again next tick */ }
}

router();
renderSubs(lastPlayer);
poll();
setInterval(poll, 2000);
