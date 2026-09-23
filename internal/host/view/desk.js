'use strict';

// One desk, live: frames from the desk's stream drawn on a canvas, and the
// pointer and keyboard sent back as the desk's own. Coordinates go back in
// the desk's screen pixels, whatever size the canvas is drawn at.

const $ = (id) => document.getElementById(id);
const id = decodeURIComponent(location.pathname.split('/').pop());
const canvas = $('screen');
const g = canvas.getContext('2d');
const stage = $('stage');

const presets = {
  sharp: { quality: 85, fps: 15 },
  balanced: { quality: 65, fps: 12 },
  light: { quality: 45, fps: 8, max_w: 960 },
};

let ws = null;
let screenW = 1280;
let screenH = 800;
let paused = false;
let gone = false;
let retry = 0;
let frames = 0;
let bytes = 0;
let everFrame = false;

// ---- the host ------------------------------------------------------------

function deskURL(path) {
  return '/view/api/desks/' + encodeURIComponent(id) + path;
}

async function api(path, opts = {}) {
  const r = await fetch(deskURL(path), {
    ...opts,
    headers: { 'X-Hollow-View': '1', 'Content-Type': 'application/json', ...(opts.headers || {}) },
  });
  if (r.status === 401) location.reload(); // the session ended: show why
  return r;
}

function toast(msg, ms = 4000) {
  const t = $('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, ms);
}

function showDesk(d) {
  document.title = d.name + ' · hollow';
  $('name').textContent = d.name;
  const bits = [d.os, d.width + '×' + d.height, d.mem_mb + ' MB'];
  if (d.watchers > 1) bits.push(d.watchers + ' watching');
  $('meta').textContent = bits.join(' · ');
  paused = !!d.paused;
  const b = $('pauseBtn');
  b.textContent = paused ? 'Hand back' : 'Take over';
  b.className = paused ? 'handback' : 'primary';
  b.title = paused ? 'Give the desk back to the agent' : 'Pause the agent and use the desk yourself';
  $('hint').classList.toggle('held', paused);
  if (d.state !== 'ready') setOverlay('This desk is ' + d.state + (d.error ? ': ' + d.error : ''));
}

async function poll() {
  if (gone) return;
  try {
    const r = await api('');
    if (r.status === 404) {
      gone = true;
      setConn('gone', 'gone');
      setOverlay('This desk is gone. It was closed, or its host restarted.');
      if (ws) ws.close();
      return;
    }
    if (r.ok) showDesk(await r.json());
  } catch (e) { /* the host is unreachable; the stream says so */ }
}

async function setPaused(p) {
  const r = await api('/pause', { method: 'POST', body: JSON.stringify({ paused: p }) });
  if (r.ok) showDesk(await r.json());
  else toast('Could not ' + (p ? 'pause' : 'resume') + ' the agent.');
}

// The first time a person reaches for the desk, they take it: an agent
// clicking elsewhere mid-drag helps nobody.
function claim() {
  if (!paused) {
    paused = true;
    setPaused(true);
  }
}

// ---- the stream ----------------------------------------------------------

function setConn(kind, text) {
  const c = $('conn');
  c.className = 'pill ' + kind;
  c.lastElementChild.textContent = text;
}

function setOverlay(text) {
  const o = $('overlay');
  o.textContent = text;
  o.hidden = !text;
}

function connect() {
  if (gone) return;
  const p = presets[$('quality').value] || presets.balanced;
  const q = new URLSearchParams(Object.entries(p).map(([k, v]) => [k, String(v)]));
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const sock = new WebSocket(scheme + '//' + location.host + deskURL('/stream') + '?' + q);
  sock.binaryType = 'blob';
  ws = sock;
  setConn('wait', retry ? 'reconnecting' : 'connecting');
  sock.onopen = () => { retry = 0; setConn('live', 'live'); };
  sock.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      const m = JSON.parse(ev.data);
      if (m.type === 'hello') {
        screenW = m.screen_w;
        screenH = m.screen_h;
        canvas.width = m.frame_w;
        canvas.height = m.frame_h;
        fit();
      } else if (m.type === 'error') {
        toast(m.error);
      }
      return;
    }
    frames++;
    bytes += ev.data.size;
    queue(ev.data);
  };
  sock.onclose = () => {
    if (ws !== sock || gone) return;
    setConn('wait', 'reconnecting');
    setTimeout(connect, Math.min(8000, 400 * 2 ** retry++));
    poll();
  };
}

// Frames are decoded one at a time, and one that arrives while another is
// being decoded replaces any still waiting: the newest picture wins.
let next = null;
let drawing = false;
function queue(blob) {
  next = blob;
  if (!drawing) draw();
}
async function draw() {
  drawing = true;
  while (next) {
    const blob = next;
    next = null;
    try {
      const bm = await createImageBitmap(blob);
      g.drawImage(bm, 0, 0, canvas.width, canvas.height);
      bm.close();
      if (!everFrame) { everFrame = true; setOverlay(''); }
    } catch (e) { /* a torn frame; the next one fixes it */ }
  }
  drawing = false;
}

setInterval(() => {
  if (ws && ws.readyState === WebSocket.OPEN) {
    const kb = bytes / 1024;
    const rate = kb > 1024 ? (kb / 1024).toFixed(1) + ' MB/s' : Math.round(kb) + ' KB/s';
    setConn('live', frames ? 'live · ' + frames + ' fps · ' + rate : 'live · still');
  }
  frames = 0;
  bytes = 0;
}, 1000);

// Drawn as large as fits, never larger than the desk's own size unless
// the page is full screen.
function fit() {
  const r = stage.getBoundingClientRect();
  const pad = document.fullscreenElement ? 0 : 28;
  let s = Math.min((r.width - pad) / screenW, (r.height - pad) / screenH);
  if (!document.fullscreenElement) s = Math.min(s, 1);
  canvas.style.width = Math.max(1, Math.floor(screenW * s)) + 'px';
  canvas.style.height = Math.max(1, Math.floor(screenH * s)) + 'px';
}
new ResizeObserver(fit).observe(stage);

// ---- input ---------------------------------------------------------------

function send(m) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m));
}

function at(e) {
  const r = canvas.getBoundingClientRect();
  const x = Math.round(((e.clientX - r.left) / r.width) * screenW);
  const y = Math.round(((e.clientY - r.top) / r.height) * screenH);
  return { x: Math.max(0, Math.min(screenW - 1, x)), y: Math.max(0, Math.min(screenH - 1, y)) };
}

const buttonOf = (e) => ({ 0: 1, 1: 2, 2: 3 })[e.button] || 1;

// Moves are sent at most once a frame; the last position is what matters.
let pendingMove = null;
canvas.addEventListener('pointermove', (e) => {
  const first = !pendingMove;
  pendingMove = at(e);
  if (first) {
    requestAnimationFrame(() => {
      if (pendingMove) send({ t: 'move', ...pendingMove });
      pendingMove = null;
    });
  }
});
canvas.addEventListener('pointerdown', (e) => {
  e.preventDefault();
  canvas.focus();
  canvas.setPointerCapture(e.pointerId);
  claim();
  pendingMove = null;
  send({ t: 'down', b: buttonOf(e), ...at(e) });
});
canvas.addEventListener('pointerup', (e) => {
  pendingMove = null;
  send({ t: 'up', b: buttonOf(e), ...at(e) });
});
canvas.addEventListener('contextmenu', (e) => e.preventDefault());

// A notch is a notch on the desk; trackpads send many small deltas, so
// they are added up until they make one.
let wheelX = 0;
let wheelY = 0;
canvas.addEventListener('wheel', (e) => {
  e.preventDefault();
  claim();
  const k = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 800 : 1;
  wheelY += e.deltaY * k;
  wheelX += e.deltaX * k;
  const dy = Math.trunc(wheelY / 60);
  const dx = Math.trunc(wheelX / 60);
  if (dy || dx) {
    wheelY -= dy * 60;
    wheelX -= dx * 60;
    send({ t: 'wheel', dx, dy, ...at(e) });
  }
}, { passive: false });

// Keys: printable characters are typed as characters, so the desk's
// keyboard layout never matters; everything else goes as an xdotool chord.
const named = {
  Enter: 'Return', Backspace: 'BackSpace', Tab: 'Tab', Escape: 'Escape', Delete: 'Delete',
  Insert: 'Insert', Home: 'Home', End: 'End', PageUp: 'Page_Up', PageDown: 'Page_Down',
  ArrowUp: 'Up', ArrowDown: 'Down', ArrowLeft: 'Left', ArrowRight: 'Right', ' ': 'space',
  ContextMenu: 'Menu', PrintScreen: 'Print',
};
for (let i = 1; i <= 12; i++) named['F' + i] = 'F' + i;
const byCode = {
  Minus: 'minus', Equal: 'equal', BracketLeft: 'bracketleft', BracketRight: 'bracketright',
  Backslash: 'backslash', Semicolon: 'semicolon', Quote: 'apostrophe', Comma: 'comma',
  Period: 'period', Slash: 'slash', Backquote: 'grave', Space: 'space',
};
const modifierKeys = new Set(['Shift', 'Control', 'Alt', 'Meta', 'CapsLock', 'OS', 'AltGraph', 'Fn']);

canvas.addEventListener('keydown', (e) => {
  if (e.isComposing || modifierKeys.has(e.key)) return;
  const ctrl = e.ctrlKey || e.metaKey;
  // ctrl+v and ⌘V paste this machine's clipboard: let the paste event
  // through to carry it.
  if (ctrl && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'v') return;
  e.preventDefault();
  claim();
  if (!ctrl && !e.altKey && e.key.length === 1) {
    send({ t: 'type', text: e.key });
    return;
  }
  let key = named[e.key];
  if (!key) {
    if (/^Key[A-Z]$/.test(e.code)) key = e.code.slice(3).toLowerCase();
    else if (/^Digit[0-9]$/.test(e.code)) key = e.code.slice(5);
    else if (byCode[e.code]) key = byCode[e.code];
    else return;
  }
  const mods = [];
  if (ctrl) mods.push('ctrl');
  if (e.altKey) mods.push('alt');
  if (e.shiftKey) mods.push('shift');
  send({ t: 'key', keys: [...mods, key].join('+') });
});

document.addEventListener('paste', (e) => {
  if (document.activeElement !== canvas) return;
  const text = e.clipboardData && e.clipboardData.getData('text');
  if (!text) return;
  e.preventDefault();
  claim();
  send({ t: 'paste', text });
});

// ---- controls ------------------------------------------------------------

$('pauseBtn').addEventListener('click', () => setPaused(!paused));
$('quality').addEventListener('change', () => {
  const old = ws;
  ws = null;
  if (old) old.close();
  retry = 0;
  connect();
});
$('fullBtn').addEventListener('click', () => {
  if (document.fullscreenElement) document.exitFullscreen();
  else stage.requestFullscreen().then(() => canvas.focus()).catch(() => {});
});

async function readClipboard() {
  const r = await api('/clipboard');
  const box = $('clipText');
  if (r.ok) {
    box.value = (await r.json()).text || '';
  } else {
    let msg = 'The clipboard could not be read.';
    try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
    box.value = '';
    box.placeholder = msg;
  }
}
$('clipBtn').addEventListener('click', () => {
  const pop = $('clipPop');
  pop.hidden = !pop.hidden;
  if (!pop.hidden) readClipboard();
});
$('clipClose').addEventListener('click', () => { $('clipPop').hidden = true; canvas.focus(); });
$('clipRefresh').addEventListener('click', readClipboard);
$('clipCopy').addEventListener('click', async () => {
  const box = $('clipText');
  try {
    await navigator.clipboard.writeText(box.value);
  } catch (e) {
    // Not a secure context (plain http on a mesh): the old way still works
    // from a click.
    box.select();
    document.execCommand('copy');
  }
  toast('Copied.', 1500);
});
$('sendBtn').addEventListener('click', () => {
  const text = $('sendText').value;
  if (!text) return;
  claim();
  send({ t: 'paste', text });
  $('sendText').value = '';
  $('clipPop').hidden = true;
  canvas.focus();
});

fit();
poll();
connect();
setInterval(poll, 3000);
canvas.focus();
