// Fliporium frontend. Talks to the Go App via the Wails-injected window.go.main.App
// surface and listens for "message", "flip", "flip-progress", "peer-state-changed",
// "app-state", "info" events.

const $ = (id) => document.getElementById(id);

const state = {
    self: null,
    peers: [],
    booths: [],                                  // [BoothRecord]
    selection: null,                             // {kind: 'peer'|'booth', key: name|id}
    unread: new Set(),                           // peer names with unread
    unreadBooths: new Set(),                     // booth ids with unread
    msgsByPeer: new Map(),                       // peer -> [MessageRecord]
    msgsByBooth: new Map(),                      // boothId -> [MessageRecord]
    flipsByPeer: new Map(),                      // peer -> Map<id, FlipRecord>
    showtimeByBooth: new Map(),                  // boothId -> { sessionId, flipId, leader, filename, mime, position, playing }
    notepadByBooth: new Map(),                   // boothId -> NotepadRecord
    peerStatus: new Map(),                       // peerName -> "active"|"idle"|"away"
    replyingTo: null,                            // { uuid, text, peer } when composing a reply
    appState: "initializing",
};

const QUICK_REACTIONS = ["👍","❤️","😂","🎉","🔥","🙏"];

let notepadDebounce = null;
const NOTEPAD_DEBOUNCE_MS = 500;

let leaderStateInterval = null;
let suppressNextEvent = false;
const SHOWTIME_STATE_INTERVAL_MS = 2000;
const SHOWTIME_SYNC_THRESHOLD_SEC = 1.0;

function isPeerSelected(name) {
    return state.selection && state.selection.kind === "peer" && state.selection.key === name;
}
function isBoothSelected(id) {
    return state.selection && state.selection.kind === "booth" && state.selection.key === id;
}

// ---------- small Markdown renderer ----------

function renderMarkdown(text) {
    let html = text
        .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
    html = html.replace(/```([\s\S]*?)```/g, (_, code) =>
        `<pre><code>${code.replace(/^\n|\n$/g, "")}</code></pre>`);
    html = html.replace(/`([^`\n]+)`/g, "<code>$1</code>");
    html = html.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
    html = html.replace(/\*([^*\n]+)\*/g, "<em>$1</em>");
    html = html.replace(/(^|[^_])_([^_\n]+)_(?!\w)/g, "$1<em>$2</em>");
    html = html.replace(/\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g,
        '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    const parts = html.split(/(<pre[\s\S]*?<\/pre>)/g);
    for (let i = 0; i < parts.length; i++) {
        if (!parts[i].startsWith("<pre")) parts[i] = parts[i].replace(/\n/g, "<br>");
    }
    return parts.join("");
}

// ---------- formatting ----------

function shortTime(at) {
    const d = new Date(at);
    if (isNaN(d)) return "";
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function formatBytes(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
    return (n / (1024 * 1024 * 1024)).toFixed(2) + " GB";
}

function escapeAttr(s) {
    return String(s).replace(/&/g, "&amp;").replace(/"/g, "&quot;");
}

function toast(text, ms = 2500) {
    const el = $("toast");
    el.textContent = text;
    el.classList.remove("hidden");
    clearTimeout(toast._t);
    toast._t = setTimeout(() => el.classList.add("hidden"), ms);
}

// ---------- The Floor (peer list) ----------

function renderFloor() {
    const peersUl = $("peers");
    peersUl.innerHTML = "";
    if (state.peers.length === 0) {
        $("floor-empty").classList.remove("hidden");
    } else {
        $("floor-empty").classList.add("hidden");
        for (const p of state.peers) {
            const li = document.createElement("li");
            li.dataset.name = p.name;
            if (isPeerSelected(p.name)) li.classList.add("selected");
            if (state.unread.has(p.name)) li.classList.add("unread");

            const dot = document.createElement("span");
            // Status from peer-status events overrides "online" if available.
            const liveStatus = state.peerStatus.get(p.name);
            let dotClass = "offline";
            if (p.connected) {
                if (liveStatus === "idle") dotClass = "idle";
                else if (liveStatus === "away") dotClass = "away";
                else dotClass = "connected";
            } else if (p.tailnetOnline) {
                dotClass = "online";
            }
            dot.className = "dot " + dotClass;
            li.appendChild(dot);

            const name = document.createElement("span");
            name.className = "name";
            name.textContent = p.displayName || p.name;
            li.appendChild(name);

            const meta = document.createElement("span");
            meta.className = "meta";
            meta.textContent = p.connected ? "live" : (p.tailnetOnline ? "online" : "");
            li.appendChild(meta);

            li.addEventListener("click", () => selectPeer(p.name));
            peersUl.appendChild(li);
        }
    }

    // Booths section
    const boothsUl = $("booths");
    boothsUl.innerHTML = "";
    if (state.booths.length === 0) {
        $("booths-empty").classList.remove("hidden");
    } else {
        $("booths-empty").classList.add("hidden");
        for (const b of state.booths) {
            const li = document.createElement("li");
            li.dataset.boothId = b.id;
            if (isBoothSelected(b.id)) li.classList.add("selected");
            if (state.unreadBooths.has(b.id)) li.classList.add("unread");

            const icon = document.createElement("span");
            icon.className = "dot online"; // booth dot
            li.appendChild(icon);

            const name = document.createElement("span");
            name.className = "name";
            name.textContent = b.name;
            li.appendChild(name);

            const meta = document.createElement("span");
            meta.className = "meta";
            meta.textContent = (b.members || []).length + "m";
            li.appendChild(meta);

            li.addEventListener("click", () => selectBooth(b.id));
            boothsUl.appendChild(li);
        }
    }
}

async function refreshPeers() {
    try {
        const peers = await window.go.main.App.ListPeers();
        state.peers = peers || [];
        renderFloor();
    } catch (e) {
        console.warn("listPeers:", e);
    }
}

async function refreshBooths() {
    try {
        const booths = await window.go.main.App.ListBooths();
        state.booths = booths || [];
        renderFloor();
    } catch (e) {
        console.warn("listBooths:", e);
    }
}

// ---------- chat pane ----------

function timelineForPeer(peerName) {
    const items = [];
    for (const m of (state.msgsByPeer.get(peerName) || [])) {
        items.push({ kind: "msg", at: m.at, data: m });
    }
    const flipMap = state.flipsByPeer.get(peerName);
    if (flipMap) {
        for (const f of flipMap.values()) {
            items.push({ kind: "flip", at: f.startedAt || f.at, data: f });
        }
    }
    items.sort((a, b) => new Date(a.at) - new Date(b.at));
    return items;
}

function timelineForBooth(boothID) {
    const items = [];
    for (const m of (state.msgsByBooth.get(boothID) || [])) {
        items.push({ kind: "msg", at: m.at, data: m });
    }
    items.sort((a, b) => new Date(a.at) - new Date(b.at));
    return items;
}

function findMessageByUUID(uuid) {
    if (!state.selection) return null;
    if (state.selection.kind === "booth") {
        const list = state.msgsByBooth.get(state.selection.key) || [];
        return list.find(m => m.uuid === uuid) || null;
    }
    const list = state.msgsByPeer.get(state.selection.key) || [];
    return list.find(m => m.uuid === uuid) || null;
}

function renderMessage(m, container) {
    // Reuse an existing <li> if we're updating the same UUID in place.
    let li = m.uuid ? container.querySelector(`[data-msg="${escapeAttr(m.uuid)}"]`) : null;
    if (!li) {
        li = document.createElement("li");
        if (m.uuid) li.dataset.msg = m.uuid;
        container.appendChild(li);
    }
    li.className = "msg " + (m.direction === "out" ? "out" : m.direction === "info" ? "info" : "in");
    if (m.deletedAt) li.classList.add("deleted");
    li.innerHTML = "";

    if (m.direction !== "info") {
        const meta = document.createElement("div");
        meta.className = "meta";
        const editedTag = m.editedAt && !m.deletedAt ? ` <span class="edited-tag">(edited)</span>` : "";
        meta.innerHTML = (m.direction === "out" ? "me" : escapeAttr(m.displayName || m.peer)) + " - " + escapeAttr(shortTime(m.at)) + editedTag;
        li.appendChild(meta);
    }

    if (m.parentUuid) {
        const parent = findMessageByUUID(m.parentUuid);
        const chip = document.createElement("div");
        chip.className = "reply-chip";
        const preview = parent ? (parent.text || "").replace(/\s+/g, " ").slice(0, 80) : "(message)";
        chip.textContent = "↳ " + preview;
        if (parent) chip.addEventListener("click", () => jumpToMessage(parent.uuid));
        li.appendChild(chip);
    }

    const body = document.createElement("div");
    body.className = "body";
    body.innerHTML = m.deletedAt ? "[deleted]" : renderMarkdown(m.text || "");
    li.appendChild(body);

    // Reactions
    if (m.reactions && Object.keys(m.reactions).length) {
        const wrap = document.createElement("div");
        wrap.className = "msg-reactions";
        for (const [emoji, peers] of Object.entries(m.reactions)) {
            const btn = document.createElement("button");
            btn.className = "msg-reaction" + (peers.includes(state.self?.hostname) ? " mine" : "");
            btn.textContent = emoji + " " + peers.length;
            btn.title = peers.join(", ");
            btn.addEventListener("click", (e) => { e.stopPropagation(); toggleReaction(m.uuid, emoji); });
            wrap.appendChild(btn);
        }
        li.appendChild(wrap);
    }

    // Toolbar (only for non-info, non-deleted messages with a UUID)
    if (m.direction !== "info" && !m.deletedAt && m.uuid) {
        const tb = document.createElement("div");
        tb.className = "msg-toolbar";
        const reactBtn = button("☺", "react", () => openReactionPicker(li, m));
        const replyBtn = button("↩", "reply", () => startReply(m));
        tb.appendChild(reactBtn);
        tb.appendChild(replyBtn);
        tb.appendChild(button(m.pinned ? "📌" : "📍", m.pinned ? "unpin" : "pin", () => togglePin(m)));
        if (m.direction === "out") {
            tb.appendChild(button("✎", "edit", () => editMessageFlow(m)));
            tb.appendChild(button("🗑", "delete", () => deleteMessageFlow(m)));
        }
        li.appendChild(tb);
    }
}

async function togglePin(m) {
    if (!m.uuid) return;
    try { await window.go.main.App.PinMessage(m.uuid, !m.pinned); }
    catch (e) { toast("pin: " + e); }
}

function renderPinnedBanner() {
    const banner = $("pinned-banner");
    if (!banner) return;
    if (!state.selection) { banner.classList.add("hidden"); return; }
    const list = state.selection.kind === "booth"
        ? (state.msgsByBooth.get(state.selection.key) || [])
        : (state.msgsByPeer.get(state.selection.key) || []);
    const pinned = list.filter(m => m.pinned && !m.deletedAt);
    if (pinned.length === 0) { banner.classList.add("hidden"); banner.innerHTML = ""; return; }
    banner.innerHTML = "";
    for (const m of pinned) {
        const row = document.createElement("div");
        row.className = "pin-row";
        const preview = (m.text || "").replace(/\s+/g, " ").slice(0, 100);
        row.innerHTML = `
            <span class="icon">📌</span>
            <span class="preview">${escapeAttr(preview)}</span>
            <span class="from">${escapeAttr(m.direction === "out" ? "me" : (m.displayName || m.peer))}</span>
        `;
        row.addEventListener("click", () => jumpToMessage(m.uuid));
        banner.appendChild(row);
    }
    banner.classList.remove("hidden");
}

function applyMessagePin(p) {
    const found = findMessageEverywhere(p.uuid, p.boothId);
    if (!found) return;
    found.msg.pinned = !!p.pinned;
    if ((p.boothId && isBoothSelected(p.boothId)) || (!p.boothId && isPeerSelected(found.peer || found.msg.peer))) {
        renderMessage(found.msg, $("messages"));
        renderPinnedBanner();
    }
}

function button(label, title, onclick) {
    const b = document.createElement("button");
    b.textContent = label;
    b.title = title;
    b.addEventListener("click", (e) => { e.stopPropagation(); onclick(); });
    return b;
}

function openReactionPicker(anchor, m) {
    // Remove any existing picker
    document.querySelectorAll(".reaction-picker").forEach(el => el.remove());
    const picker = document.createElement("div");
    picker.className = "reaction-picker";
    for (const emoji of QUICK_REACTIONS) {
        const b = document.createElement("button");
        b.textContent = emoji;
        b.addEventListener("click", (e) => { e.stopPropagation(); toggleReaction(m.uuid, emoji); picker.remove(); });
        picker.appendChild(b);
    }
    anchor.appendChild(picker);
    // dismiss on outside click
    setTimeout(() => {
        const onAway = (e) => { if (!picker.contains(e.target)) { picker.remove(); document.removeEventListener("click", onAway, true); } };
        document.addEventListener("click", onAway, true);
    }, 0);
}

async function toggleReaction(uuid, emoji) {
    if (!uuid) return;
    try { await window.go.main.App.ToggleReaction(uuid, emoji); }
    catch (e) { toast("reaction: " + e); }
}

async function editMessageFlow(m) {
    const next = prompt("Edit message:", m.text || "");
    if (next === null || next.trim() === "" || next === m.text) return;
    try { await window.go.main.App.EditMessage(m.uuid, next); }
    catch (e) { toast("edit: " + e); }
}

async function deleteMessageFlow(m) {
    if (!confirm("Delete this message? Other people will see [deleted].")) return;
    try { await window.go.main.App.DeleteMessage(m.uuid); }
    catch (e) { toast("delete: " + e); }
}

function startReply(m) {
    state.replyingTo = { uuid: m.uuid, text: m.text, peer: m.peer };
    const banner = $("reply-banner");
    banner.classList.add("active");
    $("reply-banner-text").textContent = (m.direction === "out" ? "your message: " : (m.peer + ": ")) + (m.text || "").slice(0, 80);
    $("composerInput").focus();
}

function cancelReply() {
    state.replyingTo = null;
    $("reply-banner").classList.remove("active");
}

function jumpToMessage(uuid) {
    const el = document.querySelector(`[data-msg="${CSS.escape(uuid)}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "center" });
    el.classList.add("highlight");
    setTimeout(() => el.classList.remove("highlight"), 1500);
}

// peerLabel resolves a routing id to its friendly display name, falling back
// to the id when unknown.
function peerLabel(name) {
    const p = state.peers && state.peers.find(x => x.name === name);
    return (p && p.displayName) || name;
}

// flipVisibleNow reports whether a flip belongs in the currently open view —
// either a 1:1 with its peer, or a room the peer is a member of.
function flipVisibleNow(f) {
    if (isPeerSelected(f.peer)) return true;
    if (state.selection && state.selection.kind === "booth") {
        const booth = state.booths.find(b => b.id === state.selection.key);
        if (booth && (booth.members || []).includes(f.peer)) return true;
    }
    return false;
}

function renderFlipCard(f, container) {
    let card = container.querySelector(`[data-flip="${escapeAttr(f.id)}"]`);
    if (!card) {
        card = document.createElement("li");
        card.dataset.flip = f.id;
        container.appendChild(card);
    }
    card.className = "flip " + (f.direction === "out" ? "out" : "in") + " " + (f.status || "");
    const senderLabel = f.direction === "out" ? "me" : peerLabel(f.peer);
    const sizeStr = formatBytes(f.size || 0);
    const ts = shortTime(f.startedAt || f.at);

    let statusLine = "";
    if (f.status === "started") {
        statusLine = f.direction === "out" ? "sending..." : "receiving...";
    } else if (f.status === "complete") {
        statusLine = f.direction === "out" ? "sent" : "caught";
    } else if (f.status === "failed") {
        statusLine = "failed";
    } else if (f.status === "cancelled") {
        statusLine = "cancelled";
    }

    const progressPct = (f.size > 0 && f.bytes != null) ? Math.min(100, Math.round((f.bytes / f.size) * 100)) : 0;
    const showProgress = f.status === "started" || (f.status !== "complete" && f.bytes != null);

    const preview = renderFlipPreview(f);

    const actions = [];
    if (f.status === "complete" && f.direction === "in") {
        actions.push(`<button onclick="openFlipExt('${escapeAttr(f.id)}')">open</button>`);
    }
    // Broadcast (Showtime) button: only when viewing a booth and the flip is video/audio.
    const isMedia = f.mime && (f.mime.startsWith("video/") || f.mime.startsWith("audio/"));
    if (f.status === "complete" && isMedia && state.selection && state.selection.kind === "booth") {
        actions.push(`<button class="broadcast-btn" onclick="startShowtime('${escapeAttr(state.selection.key)}','${escapeAttr(f.id)}')">&#9654; broadcast</button>`);
    }

    card.innerHTML = `
        <div class="meta">${senderLabel} - ${ts}</div>
        <div class="file-row">
            <span class="file-icon">📎</span>
            <span class="file-name">${escapeAttr(f.filename || "")}</span>
            <span class="file-size">${sizeStr}</span>
        </div>
        ${showProgress ? `<div class="progress"><div style="width:${progressPct}%"></div></div>` : ""}
        <div class="file-status">${escapeAttr(statusLine)}</div>
        ${preview}
        ${actions.length ? `<div class="actions">${actions.join("")}</div>` : ""}
    `;
}

async function startShowtime(boothId, flipId) {
    try {
        await window.go.main.App.StartShowtime(boothId, flipId);
    } catch (e) {
        toast("showtime: " + e);
    }
}
window.startShowtime = startShowtime;

function renderFlipPreview(f) {
    if (f.status !== "complete" || !f.catchUrl) return "";
    const mime = (f.mime || "").toLowerCase();
    const url = f.catchUrl;
    if (mime.startsWith("image/")) {
        return `<div class="preview"><img src="${escapeAttr(url)}" alt="${escapeAttr(f.filename)}"></div>`;
    }
    if (mime.startsWith("video/")) {
        return `<div class="preview"><video controls preload="metadata" src="${escapeAttr(url)}"></video></div>`;
    }
    if (mime.startsWith("audio/")) {
        return `<div class="preview"><audio controls preload="metadata" src="${escapeAttr(url)}"></audio></div>`;
    }
    if (mime === "application/pdf") {
        return `<div class="preview"><embed src="${escapeAttr(url)}" type="application/pdf" width="100%" height="420"></div>`;
    }
    if (mime.startsWith("text/") || isTextMime(mime, f.filename)) {
        // Render text inline via fetch; the placeholder gets replaced after.
        const phid = "txt-" + f.id;
        queueMicrotask(() => fillTextPreview(phid, url));
        return `<div class="preview"><pre id="${phid}">(loading)</pre></div>`;
    }
    return "";
}

function isTextMime(mime, filename) {
    if (!filename) return false;
    const ext = filename.split(".").pop().toLowerCase();
    return ["md","txt","log","csv","json","js","ts","html","htm","css","go","py","rb","rs","java","c","cpp","h","hpp","yaml","yml","toml","ini","conf","sh","bat","ps1"].includes(ext);
}

async function fillTextPreview(elemId, url) {
    try {
        const res = await fetch(url);
        const text = await res.text();
        const el = document.getElementById(elemId);
        if (!el) return;
        const MAX = 8192;
        if (text.length <= MAX) {
            el.textContent = text;
        } else {
            el.textContent = text.slice(0, MAX);
            const note = document.createElement("div");
            note.className = "text-truncated";
            note.textContent = `(truncated; ${formatBytes(text.length)} total - click "open" for full file)`;
            el.parentNode.appendChild(note);
        }
    } catch (e) {
        const el = document.getElementById(elemId);
        if (el) el.textContent = "(could not read)";
    }
}

async function openFlipExt(id) {
    try { await window.go.main.App.OpenFlipExternally(id); }
    catch (e) { toast("open: " + e); }
}
window.openFlipExt = openFlipExt;

function renderChat() {
    const list = $("messages");
    list.innerHTML = "";
    renderPinnedBanner();
    if (!state.selection) {
        $("chat-title").textContent = "create a room or pick one on The Floor";
        $("chat-state").textContent = "";
        $("composerInput").disabled = true;
        $("sendBtn").disabled = true;
        $("attachBtn").disabled = true;
        $("copyInviteBtn").classList.add("hidden");
        return;
    }
    if (state.selection.kind === "peer") {
        renderChatPeer(state.selection.key, list);
    } else if (state.selection.kind === "booth") {
        renderChatBooth(state.selection.key, list);
    }
    // Copy-invite is only meaningful for a room (booth) selection.
    $("copyInviteBtn").classList.toggle("hidden", !state.selection || state.selection.kind !== "booth");
    list.scrollTop = list.scrollHeight;
}

function renderChatPeer(name, list) {
    $("notepad-panel").classList.add("hidden");
    const peer = state.peers.find(p => p.name === name);
    $("chat-title").textContent = peer ? (peer.displayName || peer.name) : name;
    $("chat-state").textContent = peer && peer.connected
        ? "live"
        : peer && peer.tailnetOnline
            ? "online - not yet connected"
            : "offline";
    const canSend = !!(peer && peer.connected);
    $("composerInput").disabled = !canSend;
    $("sendBtn").disabled = !canSend;
    $("attachBtn").disabled = !canSend;
    $("composerInput").placeholder = canSend
        ? "type a message - **markdown** works..."
        : "connect to this peer first (or wait for them to come online)";
    for (const item of timelineForPeer(name)) {
        if (item.kind === "msg") renderMessage(item.data, list);
        else if (item.kind === "flip") renderFlipCard(item.data, list);
    }
}

function renderChatBooth(boothId, list) {
    const booth = state.booths.find(b => b.id === boothId);
    if (!booth) {
        $("chat-title").textContent = "(missing booth)";
        $("notepad-panel").classList.add("hidden");
        return;
    }
    $("chat-title").textContent = booth.name;
    $("chat-state").textContent = "members: " + (booth.members || []).join(", ");
    $("composerInput").disabled = false;
    $("sendBtn").disabled = false;
    $("attachBtn").disabled = false; // booth-flip
    $("composerInput").placeholder = "post to " + booth.name + " ...";
    renderNotepadPanel(boothId);
    // also: any flips visible in the booth (cards) — for now flips for booths
    // arrive as 1:1 flips per member, so we surface flip cards by including
    // them from each peer's flip list whose peer is also a booth member.
    const memberSet = new Set(booth.members || []);
    const flipCards = [];
    for (const [peerName, m] of state.flipsByPeer) {
        if (!memberSet.has(peerName)) continue;
        for (const f of m.values()) {
            flipCards.push({ kind: "flip", at: f.startedAt || f.at, data: f });
        }
    }
    const items = timelineForBooth(boothId).concat(flipCards);
    items.sort((a, b) => new Date(a.at) - new Date(b.at));
    for (const item of items) {
        if (item.kind === "msg") renderMessage(item.data, list);
        else if (item.kind === "flip") renderFlipCard(item.data, list);
    }
    renderShowtimePanel(boothId);
}

async function renderNotepadPanel(boothId) {
    const panel = $("notepad-panel");
    const ta = $("notepad-text");
    panel.classList.remove("hidden");
    let n = state.notepadByBooth.get(boothId);
    if (!n) {
        try {
            n = await window.go.main.App.GetNotepad(boothId);
            state.notepadByBooth.set(boothId, n || { boothId, text: "", version: 0 });
        } catch (e) {
            n = { boothId, text: "", version: 0 };
        }
    }
    ta.dataset.boothId = boothId;
    if (document.activeElement !== ta) {
        ta.value = n.text || "";
    }
    const metaTxt = n.lastEditor
        ? "v" + n.version + " by " + n.lastEditor + (n.lastModified ? " - " + shortTime(n.lastModified) : "")
        : "(empty)";
    $("notepad-meta").textContent = metaTxt;
}

function applyIncomingNotepad(rec) {
    state.notepadByBooth.set(rec.boothId, rec);
    const ta = $("notepad-text");
    if (ta.dataset.boothId !== rec.boothId) return;
    if (document.activeElement === ta) return; // user is editing; don't clobber
    ta.value = rec.text || "";
    const metaTxt = rec.lastEditor
        ? "v" + rec.version + " by " + rec.lastEditor + (rec.lastModified ? " - " + shortTime(rec.lastModified) : "")
        : "(empty)";
    $("notepad-meta").textContent = metaTxt;
}

function renderShowtimePanel(boothId) {
    const panel = $("showtime-panel");
    const session = state.showtimeByBooth.get(boothId);
    if (!session) {
        panel.classList.add("hidden");
        panel.innerHTML = "";
        if (leaderStateInterval) { clearInterval(leaderStateInterval); leaderStateInterval = null; }
        return;
    }
    panel.classList.remove("hidden");
    const isLeader = session.leader === (state.self && state.self.hostname);
    const mediaTag = (session.mime || "").startsWith("audio/") ? "audio" : "video";
    const src = "/catch/" + encodeURIComponent(session.flipId);
    panel.innerHTML = `
        <div class="header">
            <span>SHOWTIME ${isLeader ? "- you're leading" : "- following " + escapeAttr(session.leader)}</span>
            <span>
                <span class="title">${escapeAttr(session.filename || session.flipId)}</span>
                <button onclick="endShowtime('${escapeAttr(session.sessionId)}','${escapeAttr(boothId)}')">end</button>
            </span>
        </div>
        <${mediaTag} id="showtime-media" controls preload="metadata" src="${escapeAttr(src)}"></${mediaTag}>
    `;
    const media = $("showtime-media");
    if (!media) return;

    // Wire leader -> broadcasts state; follower -> obeys incoming state.
    if (isLeader) {
        const broadcast = () => {
            if (suppressNextEvent) { suppressNextEvent = false; return; }
            window.go.main.App.SendShowtimeState(session.sessionId, boothId, !media.paused, media.currentTime || 0)
                .catch(e => console.warn("showtime state:", e));
        };
        media.addEventListener("play", broadcast);
        media.addEventListener("pause", broadcast);
        media.addEventListener("seeked", broadcast);
        if (leaderStateInterval) clearInterval(leaderStateInterval);
        leaderStateInterval = setInterval(() => {
            if (media.readyState >= 1) broadcast();
        }, SHOWTIME_STATE_INTERVAL_MS);
    } else {
        if (leaderStateInterval) { clearInterval(leaderStateInterval); leaderStateInterval = null; }
        // Apply the current known state immediately if we have one.
        applyShowtimeState(session);
    }
}

function applyShowtimeState(session) {
    const media = $("showtime-media");
    if (!media || media.dataset.followingSession !== session.sessionId) {
        if (media) media.dataset.followingSession = session.sessionId;
    }
    if (typeof session.position === "number" &&
        Math.abs((media.currentTime || 0) - session.position) > SHOWTIME_SYNC_THRESHOLD_SEC) {
        suppressNextEvent = true;
        try { media.currentTime = session.position; } catch (e) {}
    }
    if (session.playing && media.paused) {
        suppressNextEvent = true;
        media.play().catch(() => {});
    } else if (!session.playing && !media.paused) {
        suppressNextEvent = true;
        media.pause();
    }
}

async function endShowtime(sessionId, boothId) {
    try { await window.go.main.App.EndShowtime(sessionId, boothId); }
    catch (e) { toast("end: " + e); }
}
window.endShowtime = endShowtime;

async function selectPeer(name) {
    state.selection = { kind: "peer", key: name };
    state.unread.delete(name);
    renderFloor();

    if (!state.msgsByPeer.has(name)) {
        try {
            const msgs = await window.go.main.App.ListMessages(name, 200);
            state.msgsByPeer.set(name, msgs || []);
        } catch (e) {
            state.msgsByPeer.set(name, []);
        }
    }
    if (!state.flipsByPeer.has(name)) {
        try {
            const flips = await window.go.main.App.ListFlips(name);
            const m = new Map();
            for (const f of (flips || [])) m.set(f.id, f);
            state.flipsByPeer.set(name, m);
        } catch (e) {
            state.flipsByPeer.set(name, new Map());
        }
    }

    renderChat();

    const peer = state.peers.find(p => p.name === name);
    if (peer && peer.tailnetOnline && !peer.connected) {
        try { await window.go.main.App.Connect(name); }
        catch (e) { toast("connect: " + e); }
    }
}

async function selectBooth(boothId) {
    state.selection = { kind: "booth", key: boothId };
    state.unreadBooths.delete(boothId);
    renderFloor();

    // Entering a room joins its peer-to-peer mesh (best-effort).
    try { await window.go.main.App.SwitchRoom(boothId); } catch (e) { /* not on webrtc transport */ }

    if (!state.msgsByBooth.has(boothId)) {
        try {
            const msgs = await window.go.main.App.ListBoothMessages(boothId, 200);
            state.msgsByBooth.set(boothId, msgs || []);
        } catch (e) {
            state.msgsByBooth.set(boothId, []);
        }
    }
    renderChat();
}

function appendMessage(m) {
    if (m.boothId) {
        if (!state.msgsByBooth.has(m.boothId)) state.msgsByBooth.set(m.boothId, []);
        state.msgsByBooth.get(m.boothId).push(m);
        if (isBoothSelected(m.boothId)) {
            renderMessage(m, $("messages"));
            $("messages").scrollTop = $("messages").scrollHeight;
        } else if (m.direction === "in") {
            state.unreadBooths.add(m.boothId);
            renderFloor();
        }
        return;
    }
    if (!state.msgsByPeer.has(m.peer)) state.msgsByPeer.set(m.peer, []);
    state.msgsByPeer.get(m.peer).push(m);
    if (isPeerSelected(m.peer)) {
        renderMessage(m, $("messages"));
        $("messages").scrollTop = $("messages").scrollHeight;
    } else if (m.direction === "in") {
        state.unread.add(m.peer);
        renderFloor();
    }
}

function upsertFlip(f) {
    if (!state.flipsByPeer.has(f.peer)) state.flipsByPeer.set(f.peer, new Map());
    state.flipsByPeer.get(f.peer).set(f.id, f);
    if (flipVisibleNow(f)) {
        renderFlipCard(f, $("messages"));
        $("messages").scrollTop = $("messages").scrollHeight;
    } else if (f.direction === "in" && f.status === "complete") {
        state.unread.add(f.peer);
        renderFloor();
    }
}

function updateFlipProgress(id, peer, bytes, size) {
    const m = state.flipsByPeer.get(peer);
    if (!m) return;
    const f = m.get(id);
    if (!f) return;
    f.bytes = bytes;
    f.size = size || f.size;
    if (flipVisibleNow(f)) renderFlipCard(f, $("messages"));
}

function findMessageEverywhere(uuid, boothId) {
    if (boothId) {
        const list = state.msgsByBooth.get(boothId);
        if (list) {
            const m = list.find(x => x.uuid === uuid);
            if (m) return { list, msg: m };
        }
    } else {
        for (const [pname, list] of state.msgsByPeer) {
            const m = list.find(x => x.uuid === uuid);
            if (m) return { list, msg: m, peer: pname };
        }
    }
    return null;
}

function applyReaction(r) {
    const found = findMessageEverywhere(r.uuid, r.boothId);
    if (!found) return;
    const m = found.msg;
    if (!m.reactions) m.reactions = {};
    if (!m.reactions[r.emoji]) m.reactions[r.emoji] = [];
    const idx = m.reactions[r.emoji].indexOf(r.peer);
    if (r.action === "remove") {
        if (idx >= 0) m.reactions[r.emoji].splice(idx, 1);
        if (m.reactions[r.emoji].length === 0) delete m.reactions[r.emoji];
    } else {
        if (idx < 0) m.reactions[r.emoji].push(r.peer);
    }
    if ((r.boothId && isBoothSelected(r.boothId)) || (!r.boothId && (isPeerSelected(found.peer || m.peer)))) {
        renderMessage(m, $("messages"));
    }
}

function applyMessageEdit(e) {
    const found = findMessageEverywhere(e.uuid, e.boothId);
    if (!found) return;
    found.msg.text = e.text;
    found.msg.editedAt = e.editedAt;
    if ((e.boothId && isBoothSelected(e.boothId)) || (!e.boothId && isPeerSelected(found.peer || found.msg.peer))) {
        renderMessage(found.msg, $("messages"));
    }
}

function applyMessageDelete(d) {
    const found = findMessageEverywhere(d.uuid, d.boothId);
    if (!found) return;
    found.msg.deletedAt = d.deletedAt;
    if ((d.boothId && isBoothSelected(d.boothId)) || (!d.boothId && isPeerSelected(found.peer || found.msg.peer))) {
        renderMessage(found.msg, $("messages"));
    }
}

function upsertBooth(b) {
    const existing = state.booths.findIndex(x => x.id === b.id);
    if (existing >= 0) state.booths[existing] = b;
    else state.booths.unshift(b);
    renderFloor();
}

// ---------- top bar / self ----------

function renderSelf() {
    const el = $("selfName");
    if (!state.self) {
        el.textContent = "starting...";
        $("selfDot").className = "dot offline";
        $("selfIp").textContent = "";
        return;
    }
    el.textContent = state.self.displayName || state.self.hostname || "-";
    el.title = "click to change your name";
    el.style.cursor = "pointer";
    el.onclick = editDisplayName;
    $("selfDot").className = "dot " + (state.self.online ? "connected" : "offline");
    $("selfIp").textContent = "";
}

async function editDisplayName() {
    const current = (state.self && state.self.displayName) || "";
    const next = prompt("Your name (what friends see):", current);
    if (next === null) return;
    const name = next.trim();
    if (!name || name === current) return;
    try {
        await window.go.main.App.SetDisplayName(name);
        if (state.self) state.self.displayName = name;
        renderSelf();
        toast("name updated to " + name);
    } catch (e) {
        toast("couldn't set name: " + e);
    }
}

let currentTwin = null;
async function refreshTwin() {
    try { currentTwin = await window.go.main.App.GetTwin(); }
    catch (e) { currentTwin = null; }
}

// ---------- Backstage drawer ----------

function openBackstage() {
    $("backstage").classList.remove("hidden");
    paintBackstage();
}
function closeBackstage() { $("backstage").classList.add("hidden"); }

async function paintBackstage() {
    // Self
    if (state.self) {
        const ip = (state.self.ips && state.self.ips[0]) ? state.self.ips[0] : "";
        $("bs-self").innerHTML = `<strong>${escapeAttr(state.self.hostname || "")}</strong> &mdash; ${escapeAttr(ip)}`;
    }
    // Theme
    let theme = "dark";
    try { theme = (await window.go.main.App.GetPref("theme")) || "dark"; } catch (e) {}
    document.querySelectorAll('input[name="theme"]').forEach(r => r.checked = r.value === theme);
    // Sounds
    let snd = "0";
    try { snd = (await window.go.main.App.GetPref("sounds_on")) || "0"; } catch (e) {}
    $("bs-sounds").checked = snd === "1";
    // Twin
    await refreshTwin();
    $("bs-twin-input").value = currentTwin || "";
    $("bs-twin-current").textContent = currentTwin ? ("currently paired with " + currentTwin) : "not paired";
}

// ---------- search ----------

let searchDebounce = null;

function openSearch() {
    const overlay = $("search-overlay");
    overlay.classList.remove("hidden");
    $("search-input").focus();
    $("search-input").select();
}
function closeSearch() {
    const overlay = $("search-overlay");
    overlay.classList.add("hidden");
    $("search-input").value = "";
    $("search-results").innerHTML = "";
}

function bindSearch() {
    const input = $("search-input");
    const results = $("search-results");
    input.addEventListener("input", () => {
        clearTimeout(searchDebounce);
        const q = input.value.trim();
        if (q.length < 2) { results.innerHTML = ""; return; }
        searchDebounce = setTimeout(async () => {
            try {
                const hits = await window.go.main.App.SearchMessages(q, 30);
                renderSearchResults(hits || []);
            } catch (e) {
                results.innerHTML = `<div class="search-empty">search: ${escapeAttr(String(e))}</div>`;
            }
        }, 200);
    });
    document.addEventListener("keydown", (e) => {
        if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
            e.preventDefault();
            openSearch();
        } else if (e.key === "Escape" && !$("search-overlay").classList.contains("hidden")) {
            closeSearch();
        }
    });
    document.addEventListener("click", (e) => {
        const overlay = $("search-overlay");
        if (!overlay.classList.contains("hidden") && !overlay.contains(e.target) && e.target.id !== "searchBtn") {
            closeSearch();
        }
    });
    $("searchBtn").addEventListener("click", openSearch);
}

function renderSearchResults(hits) {
    const results = $("search-results");
    if (hits.length === 0) {
        results.innerHTML = `<div class="search-empty">no matches</div>`;
        return;
    }
    results.innerHTML = "";
    for (const h of hits) {
        const div = document.createElement("div");
        div.className = "search-hit";
        const where = h.boothId
            ? `Booth · ${escapeAttr((state.booths.find(b => b.id === h.boothId)?.name) || h.boothId.slice(0, 8))}`
            : (h.direction === "out" ? `to ${escapeAttr(h.peer)}` : `from ${escapeAttr(h.peer)}`);
        const when = shortTime(h.at);
        div.innerHTML = `
            <div class="where">${where} · ${escapeAttr(when)}</div>
            <div class="snippet">${h.snippet || escapeAttr((h.text || "").slice(0, 120))}</div>
        `;
        div.addEventListener("click", () => {
            closeSearch();
            if (h.boothId) selectBooth(h.boothId);
            else if (h.peer) selectPeer(h.peer);
            if (h.uuid) setTimeout(() => jumpToMessage(h.uuid), 250);
        });
        results.appendChild(div);
    }
    results.classList.remove("hidden");
}

function bindBackstage() {
    $("backstageBtn").addEventListener("click", openBackstage);
    $("bs-close").addEventListener("click", closeBackstage);
    document.querySelectorAll('input[name="theme"]').forEach(r => {
        r.addEventListener("change", async () => {
            const t = r.value;
            applyTheme(t);
            try { await window.go.main.App.SetPref("theme", t); } catch (e) { toast("theme: " + e); }
        });
    });
    $("bs-sounds").addEventListener("change", async () => {
        try { await window.go.main.App.SetPref("sounds_on", $("bs-sounds").checked ? "1" : "0"); }
        catch (e) { toast("sounds: " + e); }
    });
    $("bs-twin-save").addEventListener("click", async () => {
        const v = $("bs-twin-input").value.trim();
        if (!v) return;
        try {
            await window.go.main.App.SetTwin(v);
            await refreshTwin();
            $("bs-twin-current").textContent = "currently paired with " + v;
            toast("paired with " + v);
        } catch (e) { toast("twin: " + e); }
    });
    $("bs-twin-clear").addEventListener("click", async () => {
        try {
            await window.go.main.App.ClearTwin();
            await refreshTwin();
            $("bs-twin-input").value = "";
            $("bs-twin-current").textContent = "not paired";
            toast("unpaired");
        } catch (e) { toast("twin: " + e); }
    });
    $("bs-replay-tour").addEventListener("click", async () => {
        await window.go.main.App.SetPref("tour_done", "");
        closeBackstage();
        showTour(0);
    });
    // Burn-everything: arm the button only when the user types the magic phrase.
    $("bs-burn-confirm").addEventListener("input", () => {
        $("bs-burn-go").disabled = $("bs-burn-confirm").value !== "burn everything";
    });
    $("bs-burn-go").addEventListener("click", async () => {
        if ($("bs-burn-confirm").value !== "burn everything") return;
        if (!confirm("This will permanently wipe ALL of this device's Fliporium data.\n\nLast chance to cancel. Proceed?")) return;
        try {
            await window.go.main.App.BurnEverything("burn everything");
        } catch (e) { toast("burn: " + e); }
    });
}

// ---------- status auto-detect ----------

let statusTimer = null;
let lastActivity = Date.now();

function trackActivity() {
    lastActivity = Date.now();
    if (state.peerStatus.__self !== "active") {
        state.peerStatus.__self = "active";
        try { window.go.main.App.SetStatus("active"); } catch (e) {}
    }
}

function evaluateStatus() {
    const idleMs = Date.now() - lastActivity;
    let next;
    if (document.hidden) next = "away";
    else if (idleMs > 15 * 60 * 1000) next = "away";
    else if (idleMs > 5 * 60 * 1000) next = "idle";
    else next = "active";
    if (next !== state.peerStatus.__self) {
        state.peerStatus.__self = next;
        try { window.go.main.App.SetStatus(next); } catch (e) {}
    }
}

function bindStatusTracking() {
    ["mousemove", "keydown", "wheel", "touchstart"].forEach(ev => {
        document.addEventListener(ev, trackActivity, { passive: true });
    });
    document.addEventListener("visibilitychange", evaluateStatus);
    statusTimer = setInterval(evaluateStatus, 30 * 1000);
    // Initial active broadcast once Wails runtime is up.
    setTimeout(() => trackActivity(), 1000);
}

// ---------- theme ----------

function applyTheme(t) {
    if (t === "light") document.documentElement.setAttribute("data-theme", "light");
    else document.documentElement.removeAttribute("data-theme");
}

async function loadTheme() {
    try {
        const t = await window.go.main.App.GetPref("theme");
        if (t) applyTheme(t);
    } catch (e) {}
}

// ---------- tour ----------

const TOUR_STEPS = [
    { icon: "🎪", title: "Welcome to Fliporium", body: "A small private place for you and the people you care about. Create a room, share the invite link, and your chats and files go peer-to-peer — straight between your devices." },
    { icon: "🪁", title: "Flip a file", body: "Drag any file onto a peer's chat to send it. There's no size cap. Files land in the other person's Catch folder, right next to their copy of the app." },
    { icon: "📥", title: "Catch", body: "Caught files render inline when they can — images, video, audio, PDFs, code. Click 'open' to hand them off to your default app." },
    { icon: "🎪", title: "Booths", body: "A Booth is a named group room. Anyone in a Booth can post, drop files, take notes together, or fire up a Watch Party. The + next to BOOTHS makes a new one." },
];
let tourStep = 0;

function showTour(step = 0) {
    tourStep = step;
    renderTourStep();
    $("tour-overlay").classList.remove("hidden");
}
function hideTour() { $("tour-overlay").classList.add("hidden"); }

function renderTourStep() {
    const s = TOUR_STEPS[tourStep];
    $("tour-icon").textContent = s.icon;
    $("tour-title").textContent = s.title;
    $("tour-body").textContent = s.body;
    document.querySelectorAll(".tour-dots span").forEach(d => {
        d.classList.toggle("active", parseInt(d.dataset.step, 10) === tourStep);
    });
    $("tour-next").textContent = (tourStep === TOUR_STEPS.length - 1) ? "got it" : "next";
}

function bindTour() {
    $("tour-next").addEventListener("click", async () => {
        if (tourStep === TOUR_STEPS.length - 1) {
            hideTour();
            try { await window.go.main.App.SetPref("tour_done", "1"); } catch (e) {}
            return;
        }
        tourStep++;
        renderTourStep();
    });
    $("tour-skip").addEventListener("click", async () => {
        hideTour();
        try { await window.go.main.App.SetPref("tour_done", "1"); } catch (e) {}
    });
}

async function maybeShowTourOnBoot() {
    try {
        const done = await window.go.main.App.GetPref("tour_done");
        if (done !== "1") showTour(0);
    } catch (e) {}
}

// ---------- notification sound ----------

let audioCtx = null;
function playChime() {
    try {
        if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
        const osc = audioCtx.createOscillator();
        const gain = audioCtx.createGain();
        osc.type = "sine";
        osc.frequency.setValueAtTime(660, audioCtx.currentTime);
        osc.frequency.exponentialRampToValueAtTime(440, audioCtx.currentTime + 0.18);
        gain.gain.setValueAtTime(0, audioCtx.currentTime);
        gain.gain.linearRampToValueAtTime(0.08, audioCtx.currentTime + 0.02);
        gain.gain.exponentialRampToValueAtTime(0.0001, audioCtx.currentTime + 0.25);
        osc.connect(gain).connect(audioCtx.destination);
        osc.start();
        osc.stop(audioCtx.currentTime + 0.3);
    } catch (e) {}
}

async function maybeChime() {
    try {
        const on = await window.go.main.App.GetPref("sounds_on");
        if (on !== "1") return;
        playChime();
    } catch (e) {}
}

// ---------- confetti ----------

function confettiBurst(count = 60) {
    const layer = $("confetti-layer");
    if (!layer) return;
    const colors = ["#c9a7ff", "#4ade80", "#ffd166", "#ef4444", "#5bc0eb"];
    for (let i = 0; i < count; i++) {
        const p = document.createElement("div");
        p.className = "confetti-piece";
        p.style.left = Math.random() * 100 + "vw";
        p.style.background = colors[i % colors.length];
        p.style.animationDelay = (Math.random() * 0.3) + "s";
        p.style.animationDuration = (1.2 + Math.random() * 0.8) + "s";
        p.style.transform = `rotate(${Math.random() * 360}deg)`;
        layer.appendChild(p);
        setTimeout(() => p.remove(), 2200);
    }
}

// ---------- composer + attach + drop ----------

function bindNotepad() {
    $("notepad-text").addEventListener("input", () => {
        const ta = $("notepad-text");
        const boothId = ta.dataset.boothId;
        if (!boothId) return;
        if (notepadDebounce) clearTimeout(notepadDebounce);
        notepadDebounce = setTimeout(async () => {
            try {
                const rec = await window.go.main.App.UpdateNotepad(boothId, ta.value);
                if (rec) state.notepadByBooth.set(boothId, rec);
                const metaTxt = rec && rec.lastEditor
                    ? "v" + rec.version + " by " + rec.lastEditor + (rec.lastModified ? " - " + shortTime(rec.lastModified) : "")
                    : "(empty)";
                $("notepad-meta").textContent = metaTxt;
            } catch (e) {
                toast("notepad: " + e);
            }
        }, NOTEPAD_DEBOUNCE_MS);
    });
}

function bindComposer() {
    $("composer").addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const text = $("composerInput").value;
        if (!text.trim() || !state.selection) return;
        const replyTo = state.replyingTo ? state.replyingTo.uuid : "";
        try {
            if (state.selection.kind === "booth") {
                // Booth replies not yet supported on the Go side as a distinct
                // method; the BoothMessage carries no parentUUID. For now, just
                // dispatch as a normal booth message and clear the reply state.
                await window.go.main.App.SendBoothMessage(state.selection.key, text);
            } else {
                if (replyTo) {
                    await window.go.main.App.SendReply(state.selection.key, text, replyTo);
                } else {
                    await window.go.main.App.SendMessage(state.selection.key, text);
                }
            }
            $("composerInput").value = "";
            cancelReply();
        } catch (e) {
            toast("send: " + e);
        }
    });

    $("reply-cancel").addEventListener("click", cancelReply);

    $("attachBtn").addEventListener("click", async () => {
        if (!state.selection) return;
        try {
            if (state.selection.kind === "booth") {
                await window.go.main.App.PickAndSendBoothFlip(state.selection.key);
            } else {
                await window.go.main.App.PickAndSendFlip(state.selection.key);
            }
        } catch (e) {
            toast("flip: " + e);
        }
    });

    $("createBoothBtn").addEventListener("click", async () => {
        const name = prompt("Name your room");
        if (!name) return;
        try {
            const room = await window.go.main.App.CreateRoom(name);
            await refreshBooths();
            if (room && room.id) await selectBooth(room.id);
            toast("Room created — hit “copy invite link” to invite people", 4000);
        } catch (e) {
            toast("create room: " + e);
        }
    });

    $("copyInviteBtn").addEventListener("click", async () => {
        if (!state.selection || state.selection.kind !== "booth") return;
        try {
            const link = await window.go.main.App.RoomLinkFor(state.selection.key);
            await navigator.clipboard.writeText(link);
            const btn = $("copyInviteBtn");
            const old = btn.textContent;
            btn.textContent = "link copied ✓";
            setTimeout(() => { btn.textContent = old; }, 1600);
        } catch (e) {
            toast("copy invite: " + e);
        }
    });

    $("connectBtn").addEventListener("click", async () => {
        const link = $("connectInput").value.trim();
        if (!link) return;
        try {
            const room = await window.go.main.App.JoinRoomByLink(link);
            $("connectInput").value = "";
            await refreshBooths();
            if (room && room.id) await selectBooth(room.id);
            toast("joined " + (room && room.name ? room.name : "room"));
        } catch (e) {
            toast("join: " + e);
        }
    });
    $("connectInput").addEventListener("keydown", (e) => {
        if (e.key === "Enter") $("connectBtn").click();
    });
}

function bindDragDrop() {
    // Wails native file drop hands us OS paths.
    if (window.runtime && window.runtime.OnFileDrop) {
        let hideT = null;
        window.runtime.OnFileDropHover && window.runtime.OnFileDropHover((x, y) => {
            $("drop-overlay").classList.remove("hidden");
            clearTimeout(hideT);
            hideT = setTimeout(() => $("drop-overlay").classList.add("hidden"), 800);
        });
        // useDropTarget=false → drops anywhere in the window fire this, rather
        // than only on elements tagged with a special CSS property (the Wails
        // default, which silently swallowed our drops).
        window.runtime.OnFileDrop(async (x, y, paths) => {
            $("drop-overlay").classList.add("hidden");
            if (!state.selection) {
                toast("open a room or peer first, then drop the file");
                return;
            }
            if (!paths || paths.length === 0) {
                toast("couldn't read the dropped file's path");
                return;
            }
            for (const p of paths) {
                try {
                    if (state.selection.kind === "booth") {
                        await window.go.main.App.SendBoothFlip(state.selection.key, p);
                    } else {
                        await window.go.main.App.SendFlip(state.selection.key, p);
                    }
                    toast("sending " + p.split(/[\\/]/).pop() + "…");
                } catch (e) {
                    toast("flip failed: " + e);
                }
            }
        }, false);
    }
    // Show overlay on plain HTML drag-over too, as a UX hint.
    document.addEventListener("dragover", (e) => {
        e.preventDefault();
        $("drop-overlay").classList.remove("hidden");
    });
    document.addEventListener("dragleave", (e) => {
        if (e.target === document || e.target === document.body) {
            $("drop-overlay").classList.add("hidden");
        }
    });
    document.addEventListener("drop", (e) => {
        e.preventDefault();
        $("drop-overlay").classList.add("hidden");
    });
}

// ---------- events from Go ----------

function bindEvents() {
    window.runtime.EventsOn("app-state", async (status) => {
        state.appState = status.state;
        state.self = status.self;
        renderSelf();
        if (status.state === "ready") {
            await refreshPeers();
            await refreshBooths();
        } else if (status.message) {
            toast(status.message, 4000);
        }
    });

    window.runtime.EventsOn("peer-state-changed", async (ev) => {
        await refreshPeers();
        renderChat();
        // Confetti the first time we successfully connect to a brand-new peer.
        if (ev && ev.connected && ev.peer) {
            try {
                const fresh = await window.go.main.App.IsPeerNew(ev.peer);
                if (fresh) confettiBurst();
            } catch (e) {}
        }
    });

    window.runtime.EventsOn("message", (m) => {
        appendMessage(m);
        // Chime if this is an inbound message for a chat we're not currently viewing.
        if (m.direction === "in") {
            const focused = (m.boothId && isBoothSelected(m.boothId)) || (!m.boothId && isPeerSelected(m.peer));
            if (!focused) maybeChime();
        }
    });

    window.runtime.EventsOn("reaction", (r) => applyReaction(r));
    window.runtime.EventsOn("message-edited", (e) => applyMessageEdit(e));
    window.runtime.EventsOn("message-deleted", (d) => applyMessageDelete(d));
    window.runtime.EventsOn("message-pinned", (p) => applyMessagePin(p));
    window.runtime.EventsOn("peer-status", (s) => {
        state.peerStatus.set(s.peer, s.status);
        renderFloor();
    });

    window.runtime.EventsOn("flip", (f) => upsertFlip(f));
    window.runtime.EventsOn("notice", (msg) => toast(msg, 4000));

    window.runtime.EventsOn("booth", (b) => upsertBooth(b));

    window.runtime.EventsOn("showtime-started", (s) => {
        state.showtimeByBooth.set(s.boothId, {
            sessionId: s.sessionId,
            flipId: s.flipId,
            leader: s.leader,
            filename: s.filename,
            mime: s.mime,
            position: 0,
            playing: false,
        });
        if (isBoothSelected(s.boothId)) renderShowtimePanel(s.boothId);
        else toast("Showtime in " + (state.booths.find(b => b.id === s.boothId)?.name || s.boothId) + " - " + (s.filename || s.flipId));
    });

    window.runtime.EventsOn("showtime-state", (s) => {
        const session = state.showtimeByBooth.get(s.boothId);
        if (!session || session.sessionId !== s.sessionId) return;
        session.playing = s.playing;
        session.position = s.position;
        if (isBoothSelected(s.boothId)) applyShowtimeState(session);
    });

    window.runtime.EventsOn("showtime-ended", (s) => {
        state.showtimeByBooth.delete(s.boothId);
        if (isBoothSelected(s.boothId)) renderShowtimePanel(s.boothId);
    });

    window.runtime.EventsOn("notepad", (rec) => applyIncomingNotepad(rec));

    window.runtime.EventsOn("flip-progress", (p) => {
        updateFlipProgress(p.id, p.peer, p.bytes, p.size);
    });

    window.runtime.EventsOn("info", (info) => {
        if (isPeerSelected(info.peer)) {
            appendMessage({ peer: info.peer, direction: "info", text: info.text, at: info.at });
        } else {
            toast(info.peer + ": " + info.text, 3000);
        }
    });
}

// ---------- boot ----------

async function boot() {
    await loadTheme();
    bindComposer();
    bindNotepad();
    bindSearch();
    bindBackstage();
    bindTour();
    bindDragDrop();
    bindEvents();
    bindStatusTracking();
    renderSelf();
    refreshTwin();
    maybeShowTourOnBoot();

    try {
        const status = await window.go.main.App.Status();
        state.appState = status.state;
        state.self = status.self;
        renderSelf();
        if (status.state === "ready") {
            await refreshPeers();
            await refreshBooths();
        } else if (status.message) {
            toast(status.message, 4000);
        }
    } catch (e) { console.warn("initial status:", e); }

    setInterval(refreshPeers, 15000);
    setInterval(refreshBooths, 30000);
}

window.addEventListener("DOMContentLoaded", boot);
