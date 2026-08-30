/*
 * Chat frontend. Plain JavaScript, no build step, no framework — so it can be
 * edited live with nothing but a text editor.
 *
 * Layout of this file:
 *   1. state      – the single source of truth the UI renders from
 *   2. api        – thin fetch() wrappers over the JSON endpoints
 *   3. socket     – WebSocket connect / reconnect / inbound event handling
 *   4. render     – DOM updates, always derived from state
 *   5. events     – form submits and clicks
 */

// ---------------------------------------------------------------- 1. state
const state = {
  me: null,            // {id, username}
  contacts: [],        // [{id, username, online, unread}]
  activeId: null,      // contact currently open
  // conversations[contactId] = { messages: [], lastId: 0, loaded: false }
  // lastId is the highest server message ID seen; it is the cursor used to
  // fetch history and to fill any gap after a reconnect.
  conversations: {},
  pending: {},         // clientMsgId -> optimistic message, until the server echoes it
  socket: null,
  reconnectDelay: 500,
};

function conv(contactId) {
  if (!state.conversations[contactId]) {
    state.conversations[contactId] = { messages: [], lastId: 0, loaded: false };
  }
  return state.conversations[contactId];
}

// ------------------------------------------------------------------ 2. api
async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) { showLogin(); throw new Error('unauthenticated'); }
  if (!res.ok) {
    const err = await res.json().catch(() => ({}));
    throw new Error(err.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}

async function loadContacts() {
  const users = await api('GET', '/api/users');
  const unread = Object.fromEntries(state.contacts.map(c => [c.id, c.unread || 0]));
  state.contacts = users.map(u => ({ ...u, unread: unread[u.id] || 0 }));
  renderContacts();
}

// Fetch everything newer than what we already have for this contact. Called
// on first open and after every reconnect; the cursor makes it idempotent.
async function syncHistory(contactId) {
  const c = conv(contactId);
  const msgs = await api('GET', `/api/conversations/${contactId}/messages?after=${c.lastId}&limit=200`);
  msgs.forEach(m => addMessage(m, false));
  c.loaded = true;
  if (contactId === state.activeId) renderMessages();
}

// --------------------------------------------------------------- 3. socket
function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  state.socket = ws;

  ws.onopen = () => {
    state.reconnectDelay = 500;
    setConnStatus(true);
    // Anything we missed while offline is fetched by cursor, per open
    // conversation. Unopened ones will sync when first opened.
    Object.keys(state.conversations).forEach(id => syncHistory(Number(id)).catch(console.error));
    loadContacts().catch(console.error);
  };

  ws.onmessage = (ev) => handleEvent(JSON.parse(ev.data));

  ws.onclose = () => {
    setConnStatus(false);
    if (!state.me) return; // logged out on purpose
    // Exponential backoff with jitter so a server restart is not met by
    // every client reconnecting in the same millisecond.
    const delay = state.reconnectDelay + Math.random() * 250;
    state.reconnectDelay = Math.min(state.reconnectDelay * 2, 10000);
    setTimeout(connect, delay);
  };
  ws.onerror = () => ws.close();
}

function handleEvent(ev) {
  switch (ev.type) {
    case 'message': {
      const m = ev.message;
      const contactId = m.senderId === state.me.id ? m.recipientId : m.senderId;
      addMessage(m, true);
      if (contactId !== state.activeId && m.senderId !== state.me.id) {
        const c = state.contacts.find(x => x.id === contactId);
        if (c) c.unread++;
        renderContacts();
      }
      if (contactId === state.activeId) renderMessages();
      break;
    }
    case 'snapshot':
      state.contacts.forEach(c => { c.online = ev.onlineIds.includes(c.id); });
      renderContacts();
      break;
    case 'presence': {
      const c = state.contacts.find(x => x.id === ev.userId);
      if (c) { c.online = ev.online; renderContacts(); }
      else loadContacts().catch(console.error); // a brand-new user signed up
      break;
    }
    case 'error':
      console.warn('server rejected message:', ev.error);
      break;
  }
}

// addMessage inserts a server message into its conversation, replacing the
// optimistic copy if this is our own echo, and ignoring anything already seen
// (a history fetch and a live event can both deliver the same message).
function addMessage(m, live) {
  const contactId = m.senderId === state.me.id ? m.recipientId : m.senderId;
  const c = conv(contactId);
  if (state.pending[m.clientMsgId]) {
    const i = c.messages.findIndex(x => x.clientMsgId === m.clientMsgId);
    if (i >= 0) c.messages.splice(i, 1);
    delete state.pending[m.clientMsgId];
  }
  if (c.messages.some(x => x.id === m.id)) return;
  c.messages.push(m);
  // Server ID is the only order we trust; live events and history fetches
  // can interleave, so sort rather than assume arrival order.
  c.messages.sort((a, b) => (a.id || Infinity) - (b.id || Infinity));
  if (m.id > c.lastId) c.lastId = m.id;
  if (live && contactId !== state.activeId) { /* unread handled by caller */ }
}

function sendMessage(body) {
  const clientMsgId = crypto.randomUUID();
  const optimistic = {
    id: null, clientMsgId, senderId: state.me.id, recipientId: state.activeId,
    body, createdAt: new Date().toISOString(), pending: true,
  };
  state.pending[clientMsgId] = optimistic;
  conv(state.activeId).messages.push(optimistic);
  renderMessages();
  state.socket.send(JSON.stringify({ type: 'send', to: state.activeId, body, clientMsgId }));
}

// --------------------------------------------------------------- 4. render
const $ = (sel) => document.querySelector(sel);

function showLogin() {
  state.me = null;
  if (state.socket) state.socket.close();
  $('#app').hidden = true;
  $('#login').hidden = false;
  $('#login-username').focus();
}

function showApp() {
  $('#login').hidden = true;
  $('#app').hidden = false;
  $('#me-name').textContent = state.me.username;
}

function setConnStatus(online) {
  $('#conn-status').classList.toggle('online', online);
  $('#conn-status').title = online ? 'connected' : 'reconnecting…';
}

function renderContacts() {
  const ul = $('#contacts');
  ul.innerHTML = '';
  for (const c of state.contacts) {
    const li = document.createElement('li');
    li.className = c.id === state.activeId ? 'active' : '';
    li.innerHTML = `<span class="dot ${c.online ? 'online' : ''}"></span><span class="name"></span>`;
    li.querySelector('.name').textContent = c.username; // textContent: never inject user input as HTML
    if (c.unread) {
      const b = document.createElement('span');
      b.className = 'badge'; b.textContent = c.unread;
      li.appendChild(b);
    }
    li.onclick = () => openConversation(c.id);
    ul.appendChild(li);
  }
}

function renderMessages() {
  const ol = $('#messages');
  ol.innerHTML = '';
  if (!state.activeId) return;
  for (const m of conv(state.activeId).messages) {
    const li = document.createElement('li');
    li.className = (m.senderId === state.me.id ? 'mine' : '') + (m.pending ? ' pending' : '');
    li.textContent = m.body;
    const t = document.createElement('time');
    t.textContent = new Date(m.createdAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    li.appendChild(t);
    ol.appendChild(li);
  }
  ol.scrollTop = ol.scrollHeight;
}

async function openConversation(contactId) {
  state.activeId = contactId;
  const c = state.contacts.find(x => x.id === contactId);
  if (c) c.unread = 0;
  $('#chat-header').innerHTML = `<span class="dot ${c && c.online ? 'online' : ''}"></span><strong></strong>`;
  $('#chat-header strong').textContent = c ? c.username : '';
  $('#send-form').hidden = false;
  renderContacts();
  renderMessages();
  if (!conv(contactId).loaded) await syncHistory(contactId);
  $('#send-input').focus();
}

// --------------------------------------------------------------- 5. events
$('#login-form').onsubmit = async (e) => {
  e.preventDefault();
  const errEl = $('#login-error');
  errEl.hidden = true;
  try {
    state.me = await api('POST', '/api/login', { username: $('#login-username').value.trim() });
    await start();
  } catch (err) {
    errEl.textContent = err.message; errEl.hidden = false;
  }
};

$('#logout').onclick = async () => {
  await api('POST', '/api/logout').catch(() => {});
  Object.assign(state, { contacts: [], activeId: null, conversations: {}, pending: {} });
  showLogin();
};

$('#send-form').onsubmit = (e) => {
  e.preventDefault();
  const input = $('#send-input');
  const body = input.value.trim();
  if (!body || !state.activeId || !state.socket || state.socket.readyState !== WebSocket.OPEN) return;
  sendMessage(body);
  input.value = '';
};

async function start() {
  showApp();
  await loadContacts();
  connect();
}

// On page load: resume the session if the cookie is still valid.
api('GET', '/api/me').then(me => { state.me = me; return start(); }).catch(showLogin);
