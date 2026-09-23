'use strict';

// Every desk on this host, with a picture of each screen kept fresh.

const $ = (id) => document.getElementById(id);
const cards = new Map(); // desk id -> {el, img, parts}

async function get(path) {
  const r = await fetch(path, { headers: { 'X-Hollow-View': '1' } });
  if (r.status === 401) location.reload();
  if (!r.ok) throw new Error(path + ': ' + r.status);
  return r.json();
}

function idle(t) {
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
  if (s < 60) return 'active now';
  if (s < 3600) return 'idle ' + Math.floor(s / 60) + 'm';
  return 'idle ' + Math.floor(s / 3600) + 'h' + String(Math.floor((s % 3600) / 60)).padStart(2, '0');
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function card(d) {
  let c = cards.get(d.id);
  if (!c) {
    const a = el('a', 'card');
    a.href = '/view/d/' + encodeURIComponent(d.id);
    const thumb = el('div', 'thumb');
    const img = el('img');
    img.alt = '';
    img.hidden = true;
    const wait = el('span', '', 'no picture yet');
    thumb.append(img, wait);
    const info = el('div', 'info');
    const row = el('div', 'row');
    const name = el('b');
    const paused = el('span', 'tag paused', 'you have it');
    const state = el('span', 'tag');
    row.append(name, paused, state);
    const sub = el('div', 'sub');
    info.append(row, sub);
    a.append(thumb, info);
    c = { el: a, img, wait, name, paused, state, sub };
    cards.set(d.id, c);
  }
  c.name.textContent = d.name;
  c.name.title = d.name === d.id ? d.id : d.name + ' (' + d.id + ')';
  c.state.textContent = d.state;
  c.state.className = 'tag ' + d.state;
  c.paused.hidden = !d.paused;
  const bits = [d.os, d.width + '×' + d.height, d.mem_mb + ' MB', idle(d.last_used)];
  if (d.watchers) bits.push(d.watchers + ' watching');
  c.sub.textContent = bits.join(' · ');
  c.ready = d.state === 'ready';
  return c;
}

// A new picture is loaded off to the side and swapped in whole, so a card
// never flickers to empty.
function refreshThumb(id, c) {
  if (!c.ready || c.loading) return;
  c.loading = true;
  const next = new Image();
  next.onload = () => {
    c.img.src = next.src;
    c.img.hidden = false;
    c.wait.hidden = true;
    c.loading = false;
  };
  next.onerror = () => { c.loading = false; };
  next.src = '/view/api/desks/' + encodeURIComponent(id) + '/screenshot?format=jpeg&quality=60&fit=540x338&n=' + Date.now();
}

async function refresh() {
  let st, desks;
  try {
    [st, desks] = await Promise.all([get('/view/api/status'), get('/view/api/desks')]);
  } catch (e) {
    $('stats').textContent = 'the host is not answering';
    return;
  }
  document.title = st.name + ' · hollow';
  $('host').textContent = st.name;
  const gb = (mb) => (mb / 1024).toFixed(1) + ' GB';
  $('stats').textContent = [
    desks.length + (desks.length === 1 ? ' desk' : ' desks'),
    gb(st.free_mb) + ' free of ' + gb(st.mem_mb),
    st.cpus + ' CPUs',
    'hollow ' + st.version,
  ].join(' · ');

  const grid = $('grid');
  const seen = new Set();
  for (const d of desks) {
    seen.add(d.id);
    const c = card(d);
    if (c.el.parentNode !== grid) grid.append(c.el);
    refreshThumb(d.id, c);
  }
  for (const [id, c] of cards) {
    if (!seen.has(id)) {
      c.el.remove();
      cards.delete(id);
    }
  }
  $('empty').hidden = desks.length > 0;
}

refresh();
setInterval(refresh, 3000);
