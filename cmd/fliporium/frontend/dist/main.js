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
    appState: "initializing",
};

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
            dot.className = "dot " + (p.connected ? "connected" : p.tailnetOnline ? "online" : "offline");
            li.appendChild(dot);

            const name = document.createElement("span");
            name.className = "name";
            name.textContent = p.name;
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

function renderMessage(m, container) {
    const li = document.createElement("li");
    li.className = "msg " + (m.direction === "out" ? "out" : m.direction === "info" ? "info" : "in");
    if (m.direction !== "info") {
        const meta = document.createElement("div");
        meta.className = "meta";
        meta.textContent = (m.direction === "out" ? "me" : m.peer) + " - " + shortTime(m.at);
        li.appendChild(meta);
    }
    const body = document.createElement("div");
    body.className = "body";
    body.innerHTML = renderMarkdown(m.text);
    li.appendChild(body);
    container.appendChild(li);
}

function renderFlipCard(f, container) {
    let card = container.querySelector(`[data-flip="${escapeAttr(f.id)}"]`);
    if (!card) {
        card = document.createElement("li");
        card.dataset.flip = f.id;
        container.appendChild(card);
    }
    card.className = "flip " + (f.direction === "out" ? "out" : "in") + " " + (f.status || "");
    const senderLabel = f.direction === "out" ? "me" : f.peer;
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
    if (f.status !== "complete" || f.direction !== "in" || !f.catchUrl) return "";
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
    if (!state.selection) {
        $("chat-title").textContent = "select a peer or booth on The Floor";
        $("chat-state").textContent = "";
        $("composerInput").disabled = true;
        $("sendBtn").disabled = true;
        $("attachBtn").disabled = true;
        return;
    }
    if (state.selection.kind === "peer") {
        renderChatPeer(state.selection.key, list);
    } else if (state.selection.kind === "booth") {
        renderChatBooth(state.selection.key, list);
    }
    list.scrollTop = list.scrollHeight;
}

function renderChatPeer(name, list) {
    $("notepad-panel").classList.add("hidden");
    const peer = state.peers.find(p => p.name === name);
    $("chat-title").textContent = peer ? peer.name : name;
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
    if (isPeerSelected(f.peer)) {
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
    if (isPeerSelected(peer)) renderFlipCard(f, $("messages"));
}

function upsertBooth(b) {
    const existing = state.booths.findIndex(x => x.id === b.id);
    if (existing >= 0) state.booths[existing] = b;
    else state.booths.unshift(b);
    renderFloor();
}

// ---------- top bar / self ----------

function renderSelf() {
    if (!state.self) {
        $("selfName").textContent = "starting...";
        $("selfDot").className = "dot offline";
        $("selfIp").textContent = "";
        return;
    }
    $("selfName").textContent = state.self.hostname || "-";
    $("selfDot").className = "dot " + (state.self.online ? "connected" : "offline");
    $("selfIp").textContent = (state.self.ips && state.self.ips[0]) ? "- " + state.self.ips[0] : "";
    renderTwinBtn();
}

let currentTwin = null;
async function refreshTwin() {
    try {
        currentTwin = await window.go.main.App.GetTwin();
    } catch (e) {
        currentTwin = null;
    }
    renderTwinBtn();
}
function renderTwinBtn() {
    const btn = $("twinBtn");
    if (!btn) return;
    if (currentTwin) {
        btn.textContent = "twin: " + currentTwin;
        btn.classList.add("active");
    } else {
        btn.textContent = "twin";
        btn.classList.remove("active");
    }
}
function bindTwin() {
    $("twinBtn").addEventListener("click", async () => {
        const cur = currentTwin || "";
        const next = prompt(
            "Twin Mode pairs another of your own Fliporium instances so 1:1 chat history syncs both ways. " +
            "Enter the other instance's hostname, or leave blank to unpair.\n\nCurrent: " + (cur || "(none)"),
            cur);
        if (next === null) return;
        try {
            if (next.trim() === "") {
                await window.go.main.App.ClearTwin();
            } else {
                await window.go.main.App.SetTwin(next.trim());
            }
            await refreshTwin();
        } catch (e) {
            toast("twin: " + e);
        }
    });
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
        try {
            if (state.selection.kind === "booth") {
                await window.go.main.App.SendBoothMessage(state.selection.key, text);
            } else {
                await window.go.main.App.SendMessage(state.selection.key, text);
            }
            $("composerInput").value = "";
        } catch (e) {
            toast("send: " + e);
        }
    });

    $("attachBtn").addEventListener("click", async () => {
        if (!state.selection) return;
        try {
            if (state.selection.kind === "booth") {
                // Pick a file then booth-flip it (sends to all booth members).
                // The Go side has SendBoothFlip but no pick-then-booth-flip
                // helper; mimic it here via PickAndSendFlip with the booth's
                // first connected member, but that's a single-peer flip.
                // For Phase 7 keep this simple: nudge user toward CLI.
                toast("booth-flip: drag a file onto the window instead");
            } else {
                await window.go.main.App.PickAndSendFlip(state.selection.key);
            }
        } catch (e) {
            toast("flip: " + e);
        }
    });

    $("createBoothBtn").addEventListener("click", async () => {
        const name = prompt("Booth name?");
        if (!name) return;
        const onlinePeers = state.peers.filter(p => p.tailnetOnline).map(p => p.name);
        const memberStr = prompt(
            "Members (comma-separated peer names).\n" +
            (onlinePeers.length ? "Online peers: " + onlinePeers.join(", ") : "(no peers visible yet)"),
            onlinePeers.join(", "));
        if (memberStr === null) return;
        const members = memberStr.split(",").map(s => s.trim()).filter(Boolean);
        try {
            const id = await window.go.main.App.CreateBooth(name, members);
            await refreshBooths();
            if (id) selectBooth(id);
        } catch (e) {
            toast("create booth: " + e);
        }
    });

    $("connectBtn").addEventListener("click", async () => {
        const name = $("connectInput").value.trim();
        if (!name) return;
        try {
            await window.go.main.App.Connect(name);
            $("connectInput").value = "";
            toast("connecting to " + name + "...");
            await refreshPeers();
            selectPeer(name);
        } catch (e) {
            toast("connect: " + e);
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
        window.runtime.OnFileDrop(async (x, y, paths) => {
            $("drop-overlay").classList.add("hidden");
            if (!state.selected) {
                toast("select a peer first");
                return;
            }
            for (const p of (paths || [])) {
                try {
                    await window.go.main.App.SendFlip(state.selected, p);
                } catch (e) {
                    toast("flip " + p + ": " + e);
                }
            }
        });
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
            if (!state.selection && state.peers.length > 0) {
                const pick = state.peers.find(p => p.connected)
                    || state.peers.find(p => p.tailnetOnline)
                    || state.peers[0];
                if (pick) selectPeer(pick.name);
            }
        } else if (status.message) {
            toast(status.message, 4000);
        }
    });

    window.runtime.EventsOn("peer-state-changed", async () => {
        await refreshPeers();
        renderChat();
    });

    window.runtime.EventsOn("message", (m) => appendMessage(m));

    window.runtime.EventsOn("flip", (f) => upsertFlip(f));

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
        if (info.peer === state.selected) {
            appendMessage({ peer: info.peer, direction: "info", text: info.text, at: info.at });
        } else {
            toast(info.peer + ": " + info.text, 3000);
        }
    });
}

// ---------- boot ----------

async function boot() {
    bindComposer();
    bindNotepad();
    bindTwin();
    bindDragDrop();
    bindEvents();
    renderSelf();
    refreshTwin();

    try {
        const status = await window.go.main.App.Status();
        state.appState = status.state;
        state.self = status.self;
        renderSelf();
        if (status.state === "ready") {
            await refreshPeers();
            await refreshBooths();
        } else if (status.message) toast(status.message, 4000);
    } catch (e) { console.warn("initial status:", e); }

    setInterval(refreshPeers, 15000);
    setInterval(refreshBooths, 30000);
}

window.addEventListener("DOMContentLoaded", boot);
