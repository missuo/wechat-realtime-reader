"use strict";
// All message content is inserted with textContent / DOM nodes — never innerHTML.

const $ = (id) => document.getElementById(id);
const state = {
  kind: "",
  keyword: "",
  chat: null,          // {username, name, kind}
  filter: { sender: "", keyword: "", since: "", until: "" },
  messages: [],
  hasMore: false,
  nextBefore: 0,
  lastSeq: 0,
  loading: false,
  sessions: [],
  members: [],
};

// ---------- helpers ----------
async function api(path, params = {}) {
  const u = new URL(path, location.origin);
  for (const [k, v] of Object.entries(params)) if (v !== "" && v != null) u.searchParams.set(k, v);
  const r = await fetch(u, { credentials: "same-origin" });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}
const pad = (n) => String(n).padStart(2, "0");
function fmtListTime(iso) {
  const d = new Date(iso), now = new Date();
  if (d.toDateString() === now.toDateString()) return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return "昨天";
  if (now - d < 6 * 864e5) return "周" + "日一二三四五六"[d.getDay()];
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}/${d.getDate()}`;
  return `${String(d.getFullYear()).slice(2)}/${d.getMonth() + 1}/${d.getDate()}`;
}
const fmtTime = (iso) => { const d = new Date(iso); return `${pad(d.getHours())}:${pad(d.getMinutes())}`; };
function fmtDay(iso) {
  const d = new Date(iso), now = new Date();
  if (d.toDateString() === now.toDateString()) return "今天";
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return "昨天";
  const wd = "周" + "日一二三四五六"[d.getDay()];
  return d.getFullYear() === now.getFullYear()
    ? `${d.getMonth() + 1}月${d.getDate()}日 ${wd}`
    : `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`;
}

const noAvatar = new Set(); // usernames without a local avatar

function avatar(username, name, small) {
  const box = el("div", "avatar" + (small ? " sm" : ""));
  box.append(el("span", "av-letter", (name || username || "?").trim().slice(0, 1).toUpperCase()));
  if (!username || noAvatar.has(username)) return box;
  const img = new Image();
  img.alt = "";
  img.decoding = "async";
  // The long session list loads lazily; chat avatars load immediately (a
  // page holds few distinct senders and the browser caches them).
  if (!small) img.loading = "lazy";
  img.addEventListener("load", () => box.classList.add("has-img"));
  img.addEventListener("error", () => { noAvatar.add(username); img.remove(); });
  img.src = "/api/avatar/" + encodeURIComponent(username);
  box.append(img);
  return box;
}

function highlight(node, text, kw) {
  if (!kw) { node.textContent = text; return; }
  const lower = text.toLowerCase(), k = kw.toLowerCase();
  let i = 0, j;
  while ((j = lower.indexOf(k, i)) >= 0) {
    node.append(text.slice(i, j));
    node.append(el("mark", null, text.slice(j, j + k.length)));
    i = j + k.length;
  }
  node.append(text.slice(i));
}

// ---------- freshness ----------
function showFreshness(f) {
  const box = $("fresh");
  if (!f) return;
  box.className = "fresh " + (f.status === "fresh" ? "ok" : f.status === "unknown" ? "" : f.status === "error" ? "err" : "stale");
  const label = { fresh: "实时", stale: "滞后", error: "错误", unknown: "未同步" }[f.status] || f.status;
  box.querySelector("b").textContent = label;
  const snap = f.snapshot_time ? new Date(f.snapshot_time).toLocaleTimeString() : "-";
  box.title = `状态：${label}\n快照时间：${snap}\n延迟：${f.lag_ms ?? 0} ms` + (f.stale_dbs?.length ? `\n滞后：${f.stale_dbs.join(", ")}` : "");
}

// ---------- sidebar ----------
async function loadSidebar() {
  const list = $("list");
  try {
    const [s, c] = await Promise.all([
      api("/api/sessions", { keyword: state.keyword, kind: state.kind, limit: 300 }),
      state.keyword ? api("/api/contacts", { keyword: state.keyword, kind: state.kind, limit: 30 }) : Promise.resolve({ contacts: [] }),
    ]);
    state.sessions = s.sessions;
    showFreshness(s.freshness);
    list.replaceChildren();
    const seen = new Set();
    if (state.keyword && s.sessions.length) list.append(el("div", "section-title", "会话"));
    for (const ss of s.sessions) {
      seen.add(ss.username);
      list.append(sessionItem(ss));
    }
    const extra = c.contacts.filter((x) => !seen.has(x.username));
    if (extra.length) {
      list.append(el("div", "section-title", "联系人"));
      for (const ct of extra) list.append(contactItem(ct));
    }
    if (!list.children.length) list.append(el("div", "none", state.keyword ? "没有匹配的联系人" : "暂无会话"));
  } catch (e) {
    list.replaceChildren(el("div", "err", "加载失败：" + e.message));
  }
}

const kindLabel = { chatroom: "群", official: "公众号", stranger: "陌生人" };

function sessionItem(ss) {
  const it = el("div", "item");
  it.dataset.user = ss.username;
  if (state.chat?.username === ss.username) it.classList.add("active");
  const meta = el("div", "meta");
  const nm = el("div", "name", ss.name);
  meta.append(nm);
  let sum = ss.summary || "";
  if (ss.kind === "chatroom" && ss.last_sender_name && sum && !sum.startsWith(ss.last_sender_name)) sum = ss.last_sender_name + ": " + sum;
  meta.append(el("div", "sum", sum));
  const right = el("div", "right");
  right.append(el("span", null, fmtListTime(ss.last_time)));
  if (ss.unread > 0) right.append(el("span", "badge", ss.unread > 99 ? "99+" : String(ss.unread)));
  it.append(avatar(ss.username, ss.name), meta, right);
  it.addEventListener("click", () => openChat({ username: ss.username, name: ss.name, kind: ss.kind }));
  return it;
}

function contactItem(ct) {
  const it = el("div", "item");
  it.dataset.user = ct.username;
  const meta = el("div", "meta");
  meta.append(el("div", "name", ct.display_name));
  const sub = [ct.remark && ct.nick_name && ct.remark !== ct.nick_name ? "昵称 " + ct.nick_name : "", ct.alias ? "微信号 " + ct.alias : ""].filter(Boolean).join(" · ");
  meta.append(el("div", "sum", sub || ct.username));
  const right = el("div", "right");
  if (kindLabel[ct.kind]) right.append(el("span", "tag", kindLabel[ct.kind]));
  it.append(avatar(ct.username, ct.display_name), meta, right);
  it.addEventListener("click", () => openChat({ username: ct.username, name: ct.display_name, kind: ct.kind }));
  return it;
}

function markActive() {
  for (const it of $("list").querySelectorAll(".item")) it.classList.toggle("active", it.dataset.user === state.chat?.username);
}

// ---------- chat view ----------
function showView(v) {
  $("welcome").classList.toggle("hidden", v !== "welcome");
  $("chat").classList.toggle("hidden", v !== "chat");
  $("gsearch").classList.toggle("hidden", v !== "search");
  document.body.classList.toggle("in-chat", v !== "welcome");
}

async function openChat(chat, filter) {
  state.chat = chat;
  state.filter = { sender: "", keyword: "", since: "", until: "", ...(filter || {}) };
  showView("chat");
  markActive();
  $("chatName").textContent = chat.name;
  $("chatSub").textContent = chat.kind === "chatroom" ? "群聊" : chat.kind === "official" ? "公众号" : "";
  syncFilterForm();
  writeHash();
  loadMembers();
  await loadMessages(true);
}

async function loadMembers() {
  const chat = state.chat;
  const sel = $("fSender");
  sel.replaceChildren(new Option("全部成员", ""));
  try {
    const r = await api("/api/members", { chat: chat.username });
    if (state.chat !== chat) return;
    state.members = r.members;
    for (const m of r.members) {
      const label = m.is_self ? "我" : m.name;
      sel.append(new Option(`${label}（${m.count}）`, m.username));
    }
    sel.value = state.filter.sender;
    if (chat.kind === "chatroom") $("chatSub").textContent = `群聊 · ${r.members.length} 人发过言`;
    renderChips();
  } catch { /* members are optional */ }
}

function syncFilterForm() {
  $("fSender").value = state.filter.sender;
  $("fKeyword").value = state.filter.keyword;
  $("fSince").value = state.filter.since;
  $("fUntil").value = state.filter.until;
  const active = Object.values(state.filter).some(Boolean);
  $("filterToggle").classList.toggle("active", active);
  renderChips();
}

function renderChips() {
  const box = $("chips");
  box.replaceChildren();
  const f = state.filter;
  const add = (label, key) => {
    const c = el("span", "chip", label);
    const x = el("button", null, "×");
    x.title = "移除";
    x.addEventListener("click", () => { state.filter[key] = ""; syncFilterForm(); writeHash(); loadMessages(true); });
    c.append(x);
    box.append(c);
  };
  if (f.sender) {
    const m = state.members.find((x) => x.username === f.sender);
    const name = f.sender === state.self ? "我" : m ? m.name : f.sender;
    add("发送者：" + name, "sender");
  }
  if (f.keyword) add("关键词：" + f.keyword, "keyword");
  if (f.since) add("从 " + f.since, "since");
  if (f.until) add("到 " + f.until, "until");
}

async function loadMessages(reset) {
  if (state.loading) return;
  const chat = state.chat;
  const box = $("msgs");
  state.loading = true;
  if (reset) {
    state.messages = []; state.hasMore = false; state.nextBefore = 0; state.lastSeq = 0;
    box.replaceChildren(el("div", "loading", "加载中…"));
  }
  try {
    const r = await api("/api/messages", {
      chat: chat.username, ...state.filter, limit: 80,
      before: reset ? "" : state.nextBefore,
    });
    if (state.chat !== chat) return;
    showFreshness(r.freshness);
    const prevHeight = box.scrollHeight, prevTop = box.scrollTop;
    state.messages = reset ? r.messages : r.messages.concat(state.messages);
    state.hasMore = r.has_more;
    state.nextBefore = r.next_before;
    for (const m of r.messages) state.lastSeq = Math.max(state.lastSeq, m.seq);
    renderMessages();
    if (reset) scrollToBottom();
    else box.scrollTop = box.scrollHeight - prevHeight + prevTop;
  } catch (e) {
    box.replaceChildren(el("div", "err", "加载失败：" + e.message));
  } finally {
    state.loading = false;
  }
  // Not enough history to fill the pane: keep loading older pages so the
  // user can scroll up.
  const box2 = $("msgs");
  if (state.chat === chat && state.hasMore && box2.scrollHeight <= box2.clientHeight + 40) loadMessages(false);
}

function scrollToBottom() {
  const box = $("msgs");
  state.pinBottom = true;
  box.scrollTop = box.scrollHeight;
  // Cards and long text may reflow after fonts settle; pin once more.
  requestAnimationFrame(() => { box.scrollTop = box.scrollHeight; });
  $("jump").classList.add("hidden");
}

function renderMessages() {
  const box = $("msgs");
  box.replaceChildren();
  if (state.hasMore) {
    const b = el("button", "more", "加载更早的消息");
    b.addEventListener("click", () => loadMessages(false));
    box.append(b);
  }
  if (!state.messages.length) {
    box.append(el("div", "none", Object.values(state.filter).some(Boolean) ? "没有符合筛选条件的消息" : "暂无消息"));
    return;
  }
  let lastDay = "";
  for (const m of state.messages) {
    const day = new Date(m.time).toDateString();
    if (day !== lastDay) {
      const d = el("div", "day");
      d.append(el("span", null, fmtDay(m.time)));
      box.append(d);
      lastDay = day;
    }
    box.append(renderMessage(m, state.chat.kind === "chatroom", state.filter.keyword));
  }
}

function renderMessage(m, isGroup, kw, opts = {}) {
  if (m.kind === "system" || m.kind === "pat") {
    const s = el("div", "sys");
    highlight(s, m.text, kw);
    return s;
  }
  const row = el("div", "msg" + (m.is_self ? " mine" : ""));
  row.dataset.seq = m.seq;
  row.append(avatar(m.sender || "", m.sender_name, true));
  const body = el("div", "body");
  const who = el("div", "who");
  if (opts.chatName) {
    const c = el("span", "result-chat", opts.chatName);
    c.addEventListener("click", () => opts.onChat?.());
    who.append(c);
  }
  if ((isGroup || opts.chatName) && !m.is_self) {
    const a = el("a", null, m.sender_name);
    a.title = "只看 TA 的消息";
    a.addEventListener("click", () => {
      if (opts.onSender) return opts.onSender();
      state.filter.sender = m.sender; syncFilterForm(); writeHash(); loadMessages(true);
    });
    who.append(a);
  }
  // Only render the header row when it has something to show; an empty
  // row would push the bubble below the avatar.
  if (opts.fullTime) who.append(el("span", "time always", new Date(m.time).toLocaleString()));
  if (who.children.length) body.append(who);
  const line = el("div", "line");
  body.append(line);

  const ex = m.extra || {};
  if (["link", "file", "mini_program", "channels", "chat_record", "note", "music"].includes(m.kind) && ex.title) {
    const card = el(ex.url ? "a" : "div", "bubble card");
    if (ex.url && /^https?:\/\//.test(ex.url)) { card.href = ex.url; card.target = "_blank"; card.rel = "noopener noreferrer"; }
    const t = el("div", "ct"); highlight(t, ex.title, kw); card.append(t);
    if (ex.desc) { const d = el("div", "cd"); highlight(d, ex.desc, kw); card.append(d); }
    const label = { link: "链接", file: "文件", mini_program: "小程序", channels: "视频号", chat_record: "聊天记录", note: "笔记", music: "音乐" }[m.kind];
    const src = [label, ex.source, ex.file_size ? (ex.file_size / 1048576).toFixed(1) + " MB" : ""].filter(Boolean).join(" · ");
    card.append(el("div", "cs", src));
    line.append(card);
  } else if (m.kind === "image" && m.media_id) {
    line.append(imageBubble(m));
  } else if (m.kind === "video" && m.media_id) {
    line.append(videoBubble(m));
  } else if (m.kind === "voice") {
    line.append(voiceBubble(m, kw));
  } else {
    const isMedia = /^\[(图片|表情|语音|视频|语音\/视频通话)/.test(m.text) && m.kind !== "text";
    const b = el("div", "bubble" + (isMedia ? " media" : ""));
    highlight(b, m.text, kw);
    line.append(b);
  }
  if (!opts.fullTime) {
    const t = el("span", "time", fmtTime(m.time));
    t.title = new Date(m.time).toLocaleString();
    line.append(t);
  }
  if (ex.quote) {
    const q = el("div", "quote");
    q.textContent = (ex.quote.sender_name ? ex.quote.sender_name + "：" : "") + ex.quote.text;
    body.append(q);
  }
  row.append(body);
  return row;
}

// ---------- images ----------
function imageURL(m, size) {
  const t = Math.floor(Date.parse(m.time) / 1000);
  const p = new URLSearchParams({ chat: m.chat, id: m.media_id, local_id: m.local_id, t, size });
  return "/api/image?" + p.toString();
}

function imageBubble(m) {
  const b = el("div", "bubble pic");
  const img = new Image();
  img.alt = "图片";
  img.decoding = "async";
  img.addEventListener("load", () => { if (state.pinBottom) $("msgs").scrollTop = $("msgs").scrollHeight; });
  img.addEventListener("error", () => {
    b.className = "bubble media";
    b.textContent = "[图片] 未下载到本机";
  });
  img.addEventListener("click", () => openLightbox(m));
  img.src = imageURL(m, "thumb");
  b.append(img);
  return b;
}

function openLightbox(m) {
  const box = el("div", "lightbox");
  const img = new Image();
  img.alt = "图片";
  let triedThumb = false;
  img.addEventListener("error", () => {
    if (!triedThumb) { triedThumb = true; img.src = imageURL(m, "thumb"); }
  });
  img.src = imageURL(m, "full");
  const hint = el("div", "lb-hint", `${m.sender_name} · ${new Date(m.time).toLocaleString()} · 点击任意处关闭`);
  box.append(img, hint);
  const close = () => { box.remove(); document.removeEventListener("keydown", onKey); };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  box.addEventListener("click", close);
  document.addEventListener("keydown", onKey);
  document.body.append(box);
}

// ---------- videos ----------
function videoURL(m, cover) {
  const t = Math.floor(Date.parse(m.time) / 1000);
  const p = new URLSearchParams({ id: m.media_id, t });
  if (cover) p.set("cover", "1");
  return "/api/video?" + p.toString();
}

function videoBubble(m) {
  const b = el("div", "bubble pic video");
  const img = new Image();
  img.alt = "视频";
  img.decoding = "async";
  img.addEventListener("load", () => { if (state.pinBottom) $("msgs").scrollTop = $("msgs").scrollHeight; });
  img.addEventListener("error", () => {
    b.className = "bubble media";
    b.textContent = m.text + " 未下载到本机";
  });
  img.src = videoURL(m, true);
  const dur = (m.extra || {}).duration_sec;
  const badge = el("span", "play", dur ? `▶ ${Math.floor(dur / 60)}:${String(dur % 60).padStart(2, "0")}` : "▶");
  b.append(img, badge);
  b.addEventListener("click", () => openVideo(m));
  return b;
}

function openVideo(m) {
  const box = el("div", "lightbox");
  const v = document.createElement("video");
  v.controls = true;
  v.autoplay = true;
  v.playsInline = true;
  v.poster = videoURL(m, true);
  const hint = el("div", "lb-hint", `${m.sender_name} · ${new Date(m.time).toLocaleString()} · Esc 或点击空白处关闭`);
  v.addEventListener("error", () => { hint.textContent = "视频文件未下载到本机（在微信里点开一次即可）"; });
  v.src = videoURL(m, false);
  v.addEventListener("click", (e) => e.stopPropagation());
  box.append(v, hint);
  const close = () => { v.pause(); box.remove(); document.removeEventListener("keydown", onKey); };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  box.addEventListener("click", close);
  document.addEventListener("keydown", onKey);
  document.body.append(box);
}

// ---------- voice ----------
let playing = null; // the one <audio> currently playing

function voiceBubble(m, kw) {
  const b = el("div", "bubble voice");
  const dur = (m.extra || {}).duration_sec;
  const btn = el("button", "vplay", "▶");
  btn.type = "button";
  btn.title = "播放语音";
  const len = el("span", "vlen", dur ? `${dur}″` : "语音");
  const head = el("div", "vhead");
  head.append(btn, len);
  b.append(head);
  const tr = (m.extra || {}).transcript;
  if (tr) { const t = el("div", "vtext"); highlight(t, tr, kw); b.append(t); }

  let audio = null;
  btn.addEventListener("click", () => {
    if (!audio) {
      const t = Math.floor(Date.parse(m.time) / 1000);
      audio = new Audio("/api/voice?" + new URLSearchParams({ chat: m.chat, local_id: m.local_id, t }));
      audio.addEventListener("ended", () => { btn.textContent = "▶"; });
      audio.addEventListener("pause", () => { btn.textContent = "▶"; });
      audio.addEventListener("playing", () => { btn.textContent = "❚❚"; });
      audio.addEventListener("error", () => { btn.textContent = "▶"; len.textContent = "音频不可用"; });
    }
    if (!audio.paused) { audio.pause(); return; }
    if (playing && playing !== audio) playing.pause();
    playing = audio;
    audio.play().catch(() => {});
  });
  return b;
}

// ---------- live updates ----------
const nearBottom = () => { const b = $("msgs"); return b.scrollHeight - b.scrollTop - b.clientHeight < 120; };

const refreshLive = debounce(async () => {
  loadSidebar();
  const chat = state.chat;
  if (!chat || $("chat").classList.contains("hidden") || state.loading || !state.lastSeq) return;
  if (state.filter.until) return; // a closed range never grows
  try {
    const r = await api("/api/messages", { chat: chat.username, ...state.filter, after: state.lastSeq, limit: 200 });
    if (state.chat !== chat || !r.messages.length) return;
    const stick = nearBottom();
    const box = $("msgs");
    box.querySelector(".none")?.remove();
    let lastDay = state.messages.length ? new Date(state.messages[state.messages.length - 1].time).toDateString() : "";
    for (const m of r.messages) {
      if (state.messages.some((x) => x.seq === m.seq)) continue;
      const day = new Date(m.time).toDateString();
      if (day !== lastDay) {
        const d = el("div", "day");
        d.append(el("span", null, fmtDay(m.time)));
        box.append(d);
        lastDay = day;
      }
      const node = renderMessage(m, chat.kind === "chatroom", state.filter.keyword);
      node.classList.add("new-flash");
      box.append(node);
      state.messages.push(m);
      state.lastSeq = Math.max(state.lastSeq, m.seq);
    }
    if (stick) scrollToBottom();
    else $("jump").classList.remove("hidden");
  } catch { /* transient */ }
}, 250);

function connectEvents() {
  const es = new EventSource("/api/events");
  es.addEventListener("update", refreshLive);
  es.addEventListener("open", () => api("/api/status").then((s) => showFreshness(s.freshness)).catch(() => {}));
  es.addEventListener("error", () => { const f = $("fresh"); f.className = "fresh err"; f.querySelector("b").textContent = "断开"; });
}

// ---------- global search ----------
const suggest = debounce(async () => {
  const q = $("gSenderInput").value.trim();
  const box = $("gSuggest");
  $("gSender").value = "";
  if (!q) { box.classList.add("hidden"); return; }
  try {
    const r = await api("/api/contacts", { keyword: q, limit: 12 });
    box.replaceChildren();
    for (const c of r.contacts.filter((c) => c.kind !== "chatroom")) {
      const it = el("div", "item");
      const meta = el("div", "meta");
      meta.append(el("div", "name", c.display_name), el("div", "sum", c.alias || c.username));
      it.append(avatar(c.username, c.display_name, true), meta, el("div", "right"));
      it.addEventListener("mousedown", (e) => {
        e.preventDefault();
        $("gSender").value = c.username;
        $("gSenderInput").value = c.display_name;
        box.classList.add("hidden");
      });
      box.append(it);
    }
    box.classList.toggle("hidden", !box.children.length);
  } catch { box.classList.add("hidden"); }
}, 200);

async function runGlobalSearch() {
  const box = $("gResults");
  const params = {
    sender: $("gSender").value, keyword: $("gKeyword").value.trim(),
    since: $("gSince").value, until: $("gUntil").value, limit: 200,
  };
  if (!params.sender && $("gSenderInput").value.trim()) {
    box.replaceChildren(el("div", "err", "请从下拉列表中选择联系人"));
    return;
  }
  if (!params.sender && !params.keyword) {
    box.replaceChildren(el("div", "none", "请输入联系人或关键词"));
    return;
  }
  box.replaceChildren(el("div", "loading", "搜索中…"));
  try {
    const r = await api("/api/search", params);
    showFreshness(r.freshness);
    box.replaceChildren();
    if (!r.messages.length) { box.append(el("div", "none", "没有找到消息")); return; }
    box.append(el("div", "section-title", `找到 ${r.messages.length}${r.has_more ? "+" : ""} 条，按时间倒序`));
    for (const m of r.messages) {
      const chat = { username: m.chat, name: m.chat_name || m.chat, kind: m.chat.endsWith("@chatroom") ? "chatroom" : "friend" };
      box.append(renderMessage(m, true, params.keyword, {
        chatName: chat.name, fullTime: true,
        onChat: () => openChat(chat, { sender: params.sender, keyword: params.keyword }),
        onSender: () => openChat(chat, { sender: m.sender }),
      }));
    }
  } catch (e) {
    box.replaceChildren(el("div", "err", "搜索失败：" + e.message));
  }
}

// ---------- URL hash ----------
function writeHash() {
  if (!state.chat) { history.replaceState(null, "", "#"); return; }
  const p = new URLSearchParams({ chat: state.chat.username });
  for (const [k, v] of Object.entries(state.filter)) if (v) p.set(k, v);
  history.replaceState(null, "", "#" + p.toString());
}
async function readHash() {
  const p = new URLSearchParams(location.hash.slice(1));
  const u = p.get("chat");
  if (!u) return;
  const s = state.sessions.find((x) => x.username === u);
  let chat = s ? { username: u, name: s.name, kind: s.kind } : null;
  if (!chat) {
    const r = await api("/api/contacts", { keyword: u, limit: 1 }).catch(() => ({ contacts: [] }));
    const c = r.contacts.find((x) => x.username === u);
    chat = { username: u, name: c?.display_name || u, kind: c?.kind || (u.endsWith("@chatroom") ? "chatroom" : "friend") };
  }
  openChat(chat, { sender: p.get("sender") || "", keyword: p.get("keyword") || "", since: p.get("since") || "", until: p.get("until") || "" });
}

// ---------- wiring ----------
$("q").addEventListener("input", debounce((e) => { state.keyword = e.target.value.trim(); loadSidebar(); }, 200));
$("tabs").addEventListener("click", (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  if (b.dataset.mode === "search") {
    showView("search");
    $("gKeyword").focus();
    return;
  }
  for (const x of $("tabs").querySelectorAll("button")) x.classList.toggle("on", x === b);
  state.kind = b.dataset.kind;
  loadSidebar();
});
$("filterToggle").addEventListener("click", () => {
  const f = $("filters");
  f.classList.toggle("open");
  $("filterToggle").setAttribute("aria-expanded", f.classList.contains("open"));
});
$("filters").addEventListener("submit", (e) => {
  e.preventDefault();
  state.filter = { sender: $("fSender").value, keyword: $("fKeyword").value.trim(), since: $("fSince").value, until: $("fUntil").value };
  syncFilterForm(); writeHash(); loadMessages(true);
});
$("fSender").addEventListener("change", () => $("filters").requestSubmit());
$("fClear").addEventListener("click", () => {
  state.filter = { sender: "", keyword: "", since: "", until: "" };
  syncFilterForm(); writeHash(); loadMessages(true);
});
$("msgs").addEventListener("scroll", () => {
  const b = $("msgs");
  state.pinBottom = nearBottom();
  if (b.scrollTop < 60 && state.hasMore && !state.loading) loadMessages(false);
  if (nearBottom()) $("jump").classList.add("hidden");
});
$("jump").addEventListener("click", scrollToBottom);
$("back").addEventListener("click", () => { state.chat = null; writeHash(); markActive(); showView("welcome"); });
$("gBack").addEventListener("click", () => showView(state.chat ? "chat" : "welcome"));
$("gSenderInput").addEventListener("input", suggest);
$("gSenderInput").addEventListener("blur", () => setTimeout(() => $("gSuggest").classList.add("hidden"), 150));
$("gForm").addEventListener("submit", (e) => { e.preventDefault(); runGlobalSearch(); });

api("/api/status").then((s) => { state.self = s.self; renderChips(); }).catch(() => {});
loadSidebar().then(readHash);
connectEvents();
