// Fliporium frontend. Talks to the Go App via the Wails-injected window.go.main.App
// surface and listens for "message", "flip", "flip-progress", "peer-state-changed",
// "app-state", "info" events.

const $ = (id) => document.getElementById(id);

const state = {
    self: null,
    peers: [],
    booths: [],                                  // [BoothRecord]
    selection: null,                             // {kind: 'peer'|'booth', key: name|id}
    unread: new Map(),                           // peer name -> unread count
    unreadBooths: new Map(),                     // booth id -> unread count
    muted: new Set(),                            // "booth:<id>" | "peer:<name>" keys
    msgsByPeer: new Map(),                       // peer -> [MessageRecord]
    msgsByBooth: new Map(),                      // boothId -> [MessageRecord]
    flipsByPeer: new Map(),                      // peer -> Map<id, FlipRecord>
    peerStatus: new Map(),                       // peerName -> "active"|"idle"|"away"
    replyingTo: null,                            // { uuid, text, peer } when composing a reply
    appState: "initializing",
    floorFilter: "",                             // text typed in the Floor filter box
    showStalePeers: false,                       // reveal long-inactive people
    showStaleBooths: false,                      // reveal long-inactive rooms
};

const QUICK_REACTIONS = ["👍","❤️","😂","🎉","🔥","🙏"];

// A room/person is "inactive" once it has had no signal for this long (no
// connection, no unread, not the open conversation). Inactive items are tucked
// behind a "show inactive" toggle so The Floor doesn't grow without bound —
// nothing is deleted, just hidden until you ask for it (or filter for it).
const FLOOR_STALE_DAYS = 30;


function isPeerSelected(name) {
    return state.selection && state.selection.kind === "peer" && state.selection.key === name;
}
function isBoothSelected(id) {
    return state.selection && state.selection.kind === "booth" && state.selection.key === id;
}

// ---------- avatars ----------

// avatarFor resolves a routing id to its avatar data URI ("" if none). Reads
// live from state.self / state.peers, so it's always current.
function avatarFor(name) {
    if (state.self && name === state.self.hostname) return state.self.avatar || "";
    const p = state.peers.find(p => p.name === name);
    return (p && p.avatar) || "";
}

// isSafeAvatar guards what we drop into an <img src>: only image data: URIs.
// (The backend already validates inbound avatars, but defend in depth.)
function isSafeAvatar(uri) {
    return typeof uri === "string" && /^data:image\/(png|jpe?g|webp|gif);base64,/.test(uri);
}

function avatarInitials(s) {
    s = (s || "").trim();
    if (s.startsWith("fp-")) s = s.slice(3);
    if (!s) return "?";
    const parts = s.split(/[\s_-]+/).filter(Boolean);
    if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
    return s.slice(0, 2).toUpperCase();
}

// avatarColor derives a stable, distinguishable chip color from the id.
function avatarColor(seed) {
    seed = seed || "";
    let h = 0;
    for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) >>> 0;
    return `hsl(${h % 360} 42% 42%)`;
}

// fillAvatar paints an existing .avatar element with name's picture, or colored
// initials as a fallback.
function fillAvatar(host, name, displayName) {
    if (!host) return;
    host.innerHTML = "";
    host.classList.remove("avatar-initials");
    host.style.background = "";
    const uri = avatarFor(name);
    if (isSafeAvatar(uri)) {
        const img = document.createElement("img");
        img.src = uri;
        img.alt = "";
        img.draggable = false;
        host.appendChild(img);
    } else {
        host.classList.add("avatar-initials");
        host.textContent = avatarInitials(displayName || name);
        host.style.background = avatarColor(name);
    }
}

// avatarEl builds a round avatar element: the picture if we have one, else
// colored initials. size is an optional pixel diameter override.
function avatarEl(name, displayName, size) {
    const el = document.createElement("span");
    el.className = "avatar";
    if (size) { el.style.width = size + "px"; el.style.height = size + "px"; }
    fillAvatar(el, name, displayName);
    return el;
}

// startDM opens a private 2-person booth with a connected peer and focuses it.
async function startDM(name) {
    try {
        const room = await window.go.main.App.StartDM(name);
        await refreshBooths();
        if (room && room.id) selectBooth(room.id);
        toast("private booth opened — they'll get an invite to accept");
    } catch (e) {
        toast("couldn't start private booth: " + e);
    }
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

// escapeAttr escapes a string for safe insertion into HTML — both attribute
// values AND element text. It must escape <, >, & and quotes: peers control
// strings like display names and filenames, and these flow into innerHTML, so
// missing < / > here is a stored-XSS hole (and in a Wails webview, XSS can call
// every bound Go method). Used everywhere untrusted text meets innerHTML.
function escapeAttr(s) {
    return String(s)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;")
        .replace(/'/g, "&#39;");
}

function toast(text, ms = 2500) {
    const el = $("toast");
    el.textContent = text;
    el.classList.remove("hidden");
    clearTimeout(toast._t);
    toast._t = setTimeout(() => el.classList.add("hidden"), ms);
}

// ---------- links: bare-URL detection + open in system browser ----------

const TRAIL_PUNCT = /[.,!?:;)\]}'"]+$/;

function hostOf(url) {
    try { return new URL(url).hostname.replace(/^www\./, ""); }
    catch (e) { return url; }
}

function externalURL(raw) {
    try {
        const u = new URL(String(raw || ""), window.location.href);
        if (u.protocol === "http:" || u.protocol === "https:") return u.href;
    } catch (e) {}
    return "";
}

function cardImageSrc(raw) {
    const s = String(raw || "");
    if (s.length > 128 * 1024) return "";
    return /^data:image\/(?:jpeg|jpg|png|gif|webp);base64,[a-z0-9+/=\s]+$/i.test(s) ? s : "";
}

function safeDomId(raw) {
    return String(raw || "").replace(/[^A-Za-z0-9_-]/g, "_").slice(0, 120) || "x";
}

function findByDataset(container, key, value) {
    const needle = String(value || "");
    return Array.from(container.querySelectorAll("[data-" + key + "]"))
        .find(el => el.dataset && el.dataset[key] === needle) || null;
}

// openExternal hands a URL to the OS default browser (Wails runtime) rather
// than navigating the app's own webview to it.
function openExternal(href) {
    const safe = externalURL(href);
    if (!safe) return;
    if (window.runtime && window.runtime.BrowserOpenURL) window.runtime.BrowserOpenURL(safe);
    else window.open(safe, "_blank", "noopener,noreferrer");
}

// linkify turns bare http(s) URLs inside an element's text nodes into anchors,
// skipping text already inside <a>, <code>, or <pre>. Markdown links are
// already anchors by the time this runs, so they're left alone.
function linkify(root) {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
        acceptNode(node) {
            for (let p = node.parentElement; p && p !== root; p = p.parentElement) {
                const t = p.tagName;
                if (t === "A" || t === "CODE" || t === "PRE") return NodeFilter.FILTER_REJECT;
            }
            return /https?:\/\//.test(node.nodeValue) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
        },
    });
    const targets = [];
    for (let n = walker.nextNode(); n; n = walker.nextNode()) targets.push(n);
    for (const node of targets) {
        const text = node.nodeValue;
        const re = /https?:\/\/[^\s<>()]+/g;
        const frag = document.createDocumentFragment();
        let last = 0, m;
        while ((m = re.exec(text))) {
            const full = m[0];
            const trail = (full.match(TRAIL_PUNCT) || [""])[0];
            const url = trail ? full.slice(0, full.length - trail.length) : full;
            if (!url) continue;
            if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
            const a = document.createElement("a");
            a.href = externalURL(url);
            a.textContent = url;
            a.className = "ext-link";
            a.rel = "noopener noreferrer";
            frag.appendChild(a);
            if (trail) frag.appendChild(document.createTextNode(trail));
            last = m.index + full.length;
        }
        if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
        if (frag.childNodes.length) node.parentNode.replaceChild(frag, node);
    }
}

// ---------- link / YouTube preview cards ----------

function renderCardEl(card) {
    if (!card || !card.url) return null;
    if (card.kind === "youtube" && card.videoId) return renderYouTubeCard(card);
    return renderLinkCard(card);
}

function renderLinkCard(card) {
    const href = externalURL(card.url);
    if (!href) return null;
    const el = document.createElement("a");
    el.className = "link-card";
    el.href = href;
    el.rel = "noopener noreferrer";
    if (card.image) {
        const src = cardImageSrc(card.image);
        if (!src) return null;
        const img = document.createElement("img");
        img.className = "link-card-img";
        img.src = src;
        img.alt = "";
        el.appendChild(img);
    }
    const bodyEl = document.createElement("div");
    bodyEl.className = "link-card-body";
    const site = document.createElement("div");
    site.className = "link-card-site";
    site.textContent = card.siteName || hostOf(card.url);
    bodyEl.appendChild(site);
    if (card.title) {
        const t = document.createElement("div");
        t.className = "link-card-title";
        t.textContent = card.title;
        bodyEl.appendChild(t);
    }
    if (card.description) {
        const d = document.createElement("div");
        d.className = "link-card-desc";
        d.textContent = card.description;
        bodyEl.appendChild(d);
    }
    el.appendChild(bodyEl);
    return el;
}

// renderYouTubeCard shows a thumbnail + play button. The actual YouTube player
// (and any contact with Google) loads only when the user clicks play.
function renderYouTubeCard(card) {
    const href = externalURL(card.url);
    if (!href) return null;
    const el = document.createElement("div");
    el.className = "yt-card";
    const thumb = document.createElement("div");
    thumb.className = "yt-thumb";
    if (card.image) {
        const src = cardImageSrc(card.image);
        if (!src) return null;
        const img = document.createElement("img");
        img.src = src;
        img.alt = "";
        thumb.appendChild(img);
    }
    const play = document.createElement("button");
    play.type = "button";
    play.className = "yt-play";
    play.setAttribute("aria-label", "Play video");
    play.innerHTML = "&#9654;";
    thumb.appendChild(play);
    el.appendChild(thumb);
    if (card.title) {
        const t = document.createElement("div");
        t.className = "yt-title";
        t.textContent = card.title;
        el.appendChild(t);
    }
    const fallback = document.createElement("a");
    fallback.className = "yt-link";
    fallback.href = href;
    fallback.rel = "noopener noreferrer";
    fallback.textContent = "Watch on YouTube ↗";
    el.appendChild(fallback);

    const load = (e) => {
        e.preventDefault();
        e.stopPropagation();
        const frame = document.createElement("iframe");
        frame.className = "yt-frame";
        frame.src = "https://www.youtube-nocookie.com/embed/" + encodeURIComponent(card.videoId) + "?autoplay=1&rel=0&playsinline=1";
        frame.setAttribute("allow", "autoplay; encrypted-media; picture-in-picture; fullscreen");
        frame.setAttribute("allowfullscreen", "");
        frame.setAttribute("title", "YouTube video");
        thumb.replaceWith(frame);
    };
    thumb.addEventListener("click", load);
    return el;
}

function applyMessageCard(mc) {
    if (!mc || !mc.uuid) return;
    const found = findMessageEverywhere(mc.uuid, mc.boothId);
    if (!found) return;
    found.msg.card = mc.card;
    if ((mc.boothId && isBoothSelected(mc.boothId)) || (!mc.boothId && isPeerSelected(found.peer || found.msg.peer))) {
        renderMessage(found.msg, $("messages"));
    }
}

// ---------- clipboard image paste ----------

async function blobToBase64(blob) {
    const buf = await blob.arrayBuffer();
    const bytes = new Uint8Array(buf);
    let binary = "";
    const chunk = 0x8000;
    for (let i = 0; i < bytes.length; i += chunk) {
        binary += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
    }
    return btoa(binary);
}

// sendImageBlob writes a pasted image into the app dir and flips it to the
// current room/peer (parks it if you're alone, like a dropped file).
async function sendImageBlob(blob, mime) {
    if (!state.selection) { toast("open a room or peer first, then paste"); return; }
    try {
        const b64 = await blobToBase64(blob);
        const isBooth = state.selection.kind === "booth";
        await window.go.main.App.SendPastedImage(state.selection.key, isBooth, b64, mime || blob.type || "image/png");
        toast("sending pasted image…");
    } catch (e) {
        toast("paste image: " + e);
    }
}

// ---------- right-click context menu ----------

function bindContextMenu() {
    let menu = null;
    const hide = () => { if (menu) { menu.remove(); menu = null; } };
    document.addEventListener("click", hide);
    document.addEventListener("scroll", hide, true);
    window.addEventListener("blur", hide);
    window.addEventListener("resize", hide);

    document.addEventListener("contextmenu", (e) => {
        const items = buildContextItems(e);
        if (!items.length) { hide(); return; } // let native menu handle media/empty space
        e.preventDefault();
        hide();
        menu = document.createElement("div");
        menu.className = "ctx-menu";
        for (const it of items) {
            const b = document.createElement("button");
            b.type = "button";
            b.className = "ctx-item";
            b.textContent = it.label;
            b.addEventListener("click", (ev) => { ev.stopPropagation(); hide(); it.action(); });
            menu.appendChild(b);
        }
        document.body.appendChild(menu);
        let x = e.clientX, y = e.clientY;
        if (x + menu.offsetWidth > window.innerWidth) x = window.innerWidth - menu.offsetWidth - 6;
        if (y + menu.offsetHeight > window.innerHeight) y = window.innerHeight - menu.offsetHeight - 6;
        menu.style.left = Math.max(4, x) + "px";
        menu.style.top = Math.max(4, y) + "px";
    });
}

function buildContextItems(e) {
    const items = [];
    const target = e.target;
    const img = (target.closest && target.closest("img")) || null;
    const link = target.closest && target.closest('a[href]');
    const input = target.closest && target.closest('input, textarea, [contenteditable="true"]');
    const selection = (window.getSelection && window.getSelection().toString()) || "";

    if (img && (img.currentSrc || img.getAttribute("src"))) {
        items.push({ label: "Copy image", action: () => copyImage(img) });
    }
    if (link) {
        const href = link.getAttribute("href");
        items.push({ label: "Open link", action: () => openExternal(href) });
        items.push({ label: "Copy link", action: () => copyText(href) });
        if (selection.trim()) items.push({ label: "Copy text", action: () => copyText(selection) });
        return items;
    }
    if (items.length) return items; // a plain image (no enclosing link)
    if (input) {
        items.push({ label: "Cut", action: () => cutFromInput(input) });
        items.push({ label: "Copy", action: () => copyText(selectionInInput(input)) });
        items.push({ label: "Paste", action: () => pasteIntoInput(input) });
        items.push({ label: "Select all", action: () => input.select && input.select() });
        return items;
    }
    // Floor rows: room / person management.
    const floorBooth = target.closest && target.closest('#booths li[data-booth-id]');
    if (floorBooth) {
        const id = floorBooth.dataset.boothId;
        const booth = state.booths.find(b => b.id === id);
        if (booth && booth.pending) {
            items.push({ label: "Accept invite", action: () => acceptInvite(id) });
            items.push({ label: "Decline", action: () => declineInvite(id) });
            return items;
        }
        const mkey = "booth:" + id;
        items.push({ label: "Copy invite link", action: () => copyRoomLink(id) });
        items.push({ label: state.muted.has(mkey) ? "Unmute" : "Mute", action: () => toggleMute(mkey) });
        items.push({ label: "Leave room", action: () => leaveRoom(id, booth) });
        items.push({ label: "Delete (purge history)", action: () => deleteRoom(id, booth) });
        return items;
    }
    const floorPeer = target.closest && target.closest('#peers li[data-name]');
    if (floorPeer) {
        const name = floorPeer.dataset.name;
        const mkey = "peer:" + name;
        const p = state.peers.find(x => x.name === name);
        if (p && p.connected) {
            items.push({ label: "Message privately", action: () => startDM(name) });
        }
        items.push({ label: state.muted.has(mkey) ? "Unmute" : "Mute", action: () => toggleMute(mkey) });
        items.push({ label: "Block", action: () => blockPeer(name) });
        return items;
    }
    if (selection.trim()) {
        items.push({ label: "Copy", action: () => copyText(selection) });
        return items;
    }
    // Right-click on a message or file card with nothing selected: copy the
    // whole thing's text.
    const block = target.closest && target.closest(".msg, .flip");
    if (block) {
        const body = block.querySelector(".body") || block;
        const text = (body.innerText || body.textContent || "").trim();
        if (text) items.push({ label: "Copy", action: () => copyText(text) });
    }
    return items;
}

// copyText writes to the clipboard via the async API, falling back to a hidden
// textarea + execCommand when the webview blocks clipboard-write.
async function copyText(text) {
    if (!text) return;
    try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
            await navigator.clipboard.writeText(text);
            return;
        }
    } catch (e) { /* fall through to execCommand */ }
    try {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.top = "-1000px";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
    } catch (e) {
        toast("couldn't copy");
    }
}

// copyImage writes an <img> to the clipboard as a PNG. The image is drawn to a
// canvas first (which also normalizes any format to PNG, the only image type
// the clipboard reliably accepts). Works for caught-file previews (/catch, same
// origin) and baked-in card thumbnails (data: URIs) — neither taints the canvas.
async function copyImage(imgEl) {
    if (!(window.ClipboardItem && navigator.clipboard && navigator.clipboard.write)) {
        toast("copying images isn't supported here");
        return;
    }
    try {
        const blob = await imageToPngBlob(imgEl);
        if (!blob) { toast("couldn't read that image"); return; }
        await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
        toast("image copied");
    } catch (e) {
        toast("couldn't copy image: " + e);
    }
}

function imageToPngBlob(imgEl) {
    return new Promise((resolve) => {
        try {
            const w = imgEl.naturalWidth || imgEl.width;
            const h = imgEl.naturalHeight || imgEl.height;
            if (!w || !h) { resolve(null); return; }
            const canvas = document.createElement("canvas");
            canvas.width = w;
            canvas.height = h;
            canvas.getContext("2d").drawImage(imgEl, 0, 0, w, h);
            canvas.toBlob((b) => resolve(b), "image/png");
        } catch (e) {
            resolve(null);
        }
    });
}

function selectionInInput(input) {
    if (input.selectionStart != null && input.selectionStart !== input.selectionEnd) {
        return input.value.slice(input.selectionStart, input.selectionEnd);
    }
    return input.value || "";
}

function cutFromInput(input) {
    const s = input.selectionStart, en = input.selectionEnd;
    let text;
    if (s != null && en != null && s !== en) {
        text = input.value.slice(s, en);
        input.setRangeText("", s, en, "end");
    } else {
        text = input.value;
        input.value = "";
    }
    copyText(text);
}

function insertAtCursor(input, text) {
    const s = input.selectionStart != null ? input.selectionStart : input.value.length;
    const en = input.selectionEnd != null ? input.selectionEnd : input.value.length;
    input.setRangeText(text, s, en, "end");
    input.focus();
}

// pasteIntoInput routes a clipboard image to a flip; otherwise pastes text.
async function pasteIntoInput(input) {
    try {
        if (navigator.clipboard && navigator.clipboard.read) {
            const items = await navigator.clipboard.read();
            for (const it of items) {
                const imgType = (it.types || []).find(t => t.startsWith("image/"));
                if (imgType) {
                    const blob = await it.getType(imgType);
                    await sendImageBlob(blob, imgType);
                    return;
                }
            }
        }
    } catch (e) { /* fall through to text paste */ }
    try {
        const text = await navigator.clipboard.readText();
        if (text) insertAtCursor(input, text);
    } catch (e) {
        toast("couldn't read clipboard");
    }
}

// ---------- room / people controls ----------

async function copyRoomLink(id) {
    try {
        const link = await window.go.main.App.RoomLinkFor(id);
        await copyText(link);
        toast("invite link copied");
    } catch (e) { toast("copy invite: " + e); }
}

async function leaveRoom(id, booth) {
    const nm = (booth && booth.name) || "this room";
    if (!confirm("Leave " + nm + "? You'll stop getting its messages and it leaves your list. Your history stays — rejoin anytime with the invite link.")) return;
    try { await window.go.main.App.LeaveRoom(id); toast("left " + nm); }
    catch (e) { toast("leave: " + e); }
}

async function deleteRoom(id, booth) {
    const nm = (booth && booth.name) || "this room";
    if (!confirm("Delete " + nm + " and PURGE its history from this device? This can't be undone.")) return;
    try { await window.go.main.App.DeleteRoom(id); toast("deleted " + nm); }
    catch (e) { toast("delete: " + e); }
}

async function blockPeer(name) {
    if (!confirm("Block " + peerLabel(name) + "? They won't be able to connect to you in any room and you won't see their messages. Reversible in Backstage.")) return;
    try { await window.go.main.App.BlockPeer(name); toast("blocked"); }
    catch (e) { toast("block: " + e); }
}

async function acceptInvite(id) {
    try {
        await window.go.main.App.AcceptInvite(id);
        await refreshBooths();
        await selectBooth(id);
    } catch (e) { toast("accept: " + e); }
}

async function declineInvite(id) {
    try { await window.go.main.App.DeclineInvite(id); }
    catch (e) { toast("decline: " + e); }
}

// ---------- mute (local, persisted) ----------

async function loadMuted() {
    try {
        const raw = await window.go.main.App.GetPref("muted_keys");
        state.muted = new Set((raw || "").split(",").filter(Boolean));
    } catch (e) { state.muted = new Set(); }
}
function persistMuted() {
    window.go.main.App.SetPref("muted_keys", Array.from(state.muted).join(",")).catch(() => {});
}
function toggleMute(key) {
    if (state.muted.has(key)) state.muted.delete(key);
    else state.muted.add(key);
    persistMuted();
    renderFloor();
}
function conversationMuted(m) {
    const key = m.boothId ? ("booth:" + m.boothId) : ("peer:" + m.peer);
    return state.muted.has(key);
}

// ---------- The Floor (peer list) ----------

// ---------- Floor filtering + stale-hiding ----------

// withinDays reports whether an ISO timestamp is within `days` of now.
function withinDays(iso, days) {
    if (!iso) return false;
    const t = new Date(iso).getTime();
    return !isNaN(t) && (Date.now() - t) < days * 86400000;
}

// floorMatch tests a label against the current filter text.
function floorMatch(...labels) {
    const f = state.floorFilter.trim().toLowerCase();
    if (!f) return true;
    return labels.some(l => (l || "").toLowerCase().includes(f));
}

// peerActive / boothActive decide what stays visible with no filter typed:
// anything connected, unread, currently open, pending, or seen recently.
// Everything else is "inactive" and tucked behind the show-more toggle.
function peerActive(p) {
    return !!p.connected
        || (state.unread.get(p.name) || 0) > 0
        || isPeerSelected(p.name)
        || withinDays(p.lastSeen, FLOOR_STALE_DAYS);
}
function boothActive(b) {
    return !!b.pending
        || boothOnlineCount(b) > 0
        || (state.unreadBooths.get(b.id) || 0) > 0
        || isBoothSelected(b.id)
        || withinDays(b.lastActivity, FLOOR_STALE_DAYS);
}

function floorMoreRow(hiddenCount, expanded, onToggle) {
    const li = document.createElement("li");
    li.className = "floor-more";
    li.textContent = expanded ? "show fewer" : ("show " + hiddenCount + " inactive");
    li.title = expanded ? "Hide inactive rooms and people again" : "Show rooms and people that have been quiet for a while";
    li.addEventListener("click", onToggle);
    return li;
}

function buildPeerRow(p) {
    const li = document.createElement("li");
    li.dataset.name = p.name;
    if (isPeerSelected(p.name)) li.classList.add("selected");
    if (state.muted.has("peer:" + p.name)) li.classList.add("muted");
    const unread = state.unread.get(p.name) || 0;
    if (unread > 0) li.classList.add("unread");

    const dot = document.createElement("span");
    // Status from peer-status events overrides "online" if available.
    const liveStatus = state.peerStatus.get(p.name);
    let dotClass = "offline";
    if (p.connected) {
        if (liveStatus === "idle") dotClass = "idle";
        else if (liveStatus === "away") dotClass = "away";
        else dotClass = "connected";
    }
    dot.className = "dot " + dotClass;
    dot.title = p.connected ? (liveStatus || "online") : "offline";
    li.appendChild(dot);

    li.appendChild(avatarEl(p.name, p.displayName, 26));

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = p.displayName || p.name;
    li.appendChild(name);

    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = p.connected ? "live" : "";
    li.appendChild(meta);

    li.title = "Open your chat with " + (p.displayName || p.name) + (p.connected ? "" : " (offline)");
    appendUnreadBadge(li, unread);
    li.addEventListener("click", () => selectPeer(p.name));
    return li;
}

function buildBoothRow(b) {
    const li = document.createElement("li");
    li.dataset.boothId = b.id;

    if (b.pending) {
        li.classList.add("pending");
        const pname = document.createElement("span");
        pname.className = "name";
        pname.textContent = b.name;
        li.appendChild(pname);
        const tag = document.createElement("span");
        tag.className = "meta";
        tag.textContent = "invite";
        li.appendChild(tag);
        const ok = document.createElement("button");
        ok.className = "invite-btn ok";
        ok.textContent = "✓";
        ok.title = "accept";
        ok.addEventListener("click", (e) => { e.stopPropagation(); acceptInvite(b.id); });
        const no = document.createElement("button");
        no.className = "invite-btn no";
        no.textContent = "✕";
        no.title = "decline";
        no.addEventListener("click", (e) => { e.stopPropagation(); declineInvite(b.id); });
        li.appendChild(ok);
        li.appendChild(no);
        li.title = "Invite to “" + b.name + "” — accept or decline";
        return li;
    }

    if (isBoothSelected(b.id)) li.classList.add("selected");
    if (state.muted.has("booth:" + b.id)) li.classList.add("muted");
    const unread = state.unreadBooths.get(b.id) || 0;
    if (unread > 0) li.classList.add("unread");

    const online = boothOnlineCount(b);
    const icon = document.createElement("span");
    icon.className = "dot " + (online > 0 ? "connected" : "online");
    icon.title = online > 0 ? (online + " here now") : "no one here right now";
    li.appendChild(icon);

    const name = document.createElement("span");
    name.className = "name";
    name.textContent = b.name;
    li.appendChild(name);

    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = online > 0 ? (online + " here") : ((b.members || []).length + "m");
    li.appendChild(meta);

    li.title = "Open room: " + b.name;
    appendUnreadBadge(li, unread);
    li.addEventListener("click", () => selectBooth(b.id));
    return li;
}

function renderFloor() {
    const filtering = state.floorFilter.trim() !== "";

    // ----- Rooms -----
    const boothsUl = $("booths");
    boothsUl.innerHTML = "";
    const boothCandidates = state.booths.filter(b => floorMatch(b.name));
    let boothsShown = boothCandidates;
    if (!filtering && !state.showStaleBooths) boothsShown = boothCandidates.filter(boothActive);
    for (const b of boothsShown) boothsUl.appendChild(buildBoothRow(b));
    if (!filtering) {
        const hidden = boothCandidates.filter(b => !boothActive(b)).length;
        if (hidden > 0 || (state.showStaleBooths && boothCandidates.some(b => !boothActive(b)))) {
            boothsUl.appendChild(floorMoreRow(hidden, state.showStaleBooths, () => {
                state.showStaleBooths = !state.showStaleBooths;
                renderFloor();
            }));
        }
    }
    const boothEmpty = $("booths-empty");
    boothEmpty.textContent = filtering ? "no rooms match" : "no rooms yet — create one or join via a link";
    boothEmpty.classList.toggle("hidden", boothCandidates.length !== 0);

    // ----- People -----
    const peersUl = $("peers");
    peersUl.innerHTML = "";
    const peerCandidates = state.peers.filter(p => floorMatch(p.name, p.displayName));
    let peersShown = peerCandidates;
    if (!filtering && !state.showStalePeers) peersShown = peerCandidates.filter(peerActive);
    for (const p of peersShown) peersUl.appendChild(buildPeerRow(p));
    if (!filtering) {
        const hidden = peerCandidates.filter(p => !peerActive(p)).length;
        if (hidden > 0 || (state.showStalePeers && peerCandidates.some(p => !peerActive(p)))) {
            peersUl.appendChild(floorMoreRow(hidden, state.showStalePeers, () => {
                state.showStalePeers = !state.showStalePeers;
                renderFloor();
            }));
        }
    }
    const peerEmpty = $("floor-empty");
    peerEmpty.textContent = filtering ? "no people match" : "no one connected yet";
    peerEmpty.classList.toggle("hidden", peerCandidates.length !== 0);
}

// bindFloorFilter wires the "filter rooms & people" box. While text is present,
// the stale-hiding is bypassed so you can find anything, inactive or not.
function bindFloorFilter() {
    const input = $("floorFilter");
    if (!input) return;
    input.addEventListener("input", () => { state.floorFilter = input.value; renderFloor(); });
    input.addEventListener("keydown", (e) => {
        if (e.key === "Escape") { input.value = ""; state.floorFilter = ""; renderFloor(); input.blur(); }
    });
}

// bindWindowControls wires our custom minimize/maximize/close buttons (the
// window is frameless, so there are no native ones) and double-click-to-maximize
// on the title bar, mirroring native behavior.
function bindWindowControls() {
    const r = window.runtime;
    if (!r) return;
    const min = $("win-min"), max = $("win-max"), close = $("win-close");
    if (min) min.addEventListener("click", () => r.WindowMinimise && r.WindowMinimise());
    if (max) max.addEventListener("click", () => r.WindowToggleMaximise && r.WindowToggleMaximise());
    if (close) close.addEventListener("click", () => r.Quit && r.Quit());
    const bar = $("topbar");
    if (bar) bar.addEventListener("dblclick", (e) => {
        if (e.target.closest("button, input, a, .avatar")) return;
        r.WindowToggleMaximise && r.WindowToggleMaximise();
    });
}

// bindHelp wires the Help button + overlay and the welcome-screen actions, so a
// brand-new user has an obvious way to learn the app and to take a first action.
function bindHelp() {
    const overlay = $("help-overlay");
    const open = () => overlay.classList.remove("hidden");
    const close = () => overlay.classList.add("hidden");
    $("helpBtn").addEventListener("click", open);
    $("help-close").addEventListener("click", close);
    $("help-tour").addEventListener("click", () => { close(); showTour(0); });
    overlay.addEventListener("click", (e) => { if (e.target === overlay) close(); }); // backdrop click
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && !overlay.classList.contains("hidden")) close();
    });
    // Welcome-screen actions (shown when no conversation is open).
    const create = $("welcome-create");
    if (create) create.addEventListener("click", () => $("createBoothBtn").click());
    const paste = $("welcome-paste");
    if (paste) paste.addEventListener("click", () => {
        const inp = $("connectInput");
        if (inp) { inp.focus(); inp.scrollIntoView({ block: "nearest" }); }
        toast("paste your invite link in the box on the left ↙");
    });
}

// boothOnlineCount counts how many of a booth's members are connected right now
// (across our live rooms). Self is never in state.peers, so it's excluded.
function boothOnlineCount(booth) {
    const members = new Set(booth.members || []);
    let n = 0;
    for (const p of state.peers) {
        if (p.connected && members.has(p.name)) n++;
    }
    return n;
}

// appendUnreadBadge adds a Discord-style numeric pill to a Floor row.
function appendUnreadBadge(li, count) {
    if (!count || count <= 0) return;
    const badge = document.createElement("span");
    badge.className = "badge";
    badge.textContent = count > 99 ? "99+" : String(count);
    li.appendChild(badge);
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
            if (f.boothId) continue; // booth flips belong to their room, not the 1:1
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
    let li = m.uuid ? findByDataset(container, "msg", m.uuid) : null;
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
        if (m.direction !== "out") meta.appendChild(avatarEl(m.peer, m.displayName, 18));
        const who = document.createElement("span");
        who.className = "who";
        who.innerHTML = (m.direction === "out" ? "me" : escapeAttr(m.displayName || m.peer)) + " - " + escapeAttr(shortTime(m.at)) + editedTag;
        meta.appendChild(who);
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
    if (!m.deletedAt) linkify(body);

    // Link / YouTube card. The sender unfurled this and baked in the thumbnail,
    // so rendering it here never contacts the third-party site.
    if (!m.deletedAt && m.card) {
        const cardEl = renderCardEl(m.card);
        if (cardEl) li.appendChild(cardEl);
    }

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
            tb.appendChild(button("🗑", "delete for everyone", () => deleteMessageFlow(m)));
        }
        // Available on ANY message (yours or theirs): drop your local copy.
        tb.appendChild(button("✕", "remove from my device", () => removeMessageLocal(m)));
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

// removeMessageLocal hard-deletes a message from THIS device only (any
// message, received or sent). The other person keeps their copy.
async function removeMessageLocal(m) {
    if (!confirm("Remove this from your device? Your copy only — the other person keeps theirs. This can't be undone.")) return;
    try {
        await window.go.main.App.RemoveMessageLocally(m.id);
        // Drop it from local state and refresh the view.
        for (const list of state.msgsByPeer.values()) {
            const i = list.findIndex(x => x.id === m.id);
            if (i >= 0) list.splice(i, 1);
        }
        for (const list of state.msgsByBooth.values()) {
            const i = list.findIndex(x => x.id === m.id);
            if (i >= 0) list.splice(i, 1);
        }
        renderChat();
    } catch (e) { toast("remove: " + e); }
}

// removeFlip removes a file transfer from THIS device. For received files it
// also deletes the downloaded copy; for sent/parked files it just forgets the
// record (your original is untouched).
async function removeFlip(id, direction) {
    const msg = direction === "in"
        ? "Delete this file from your device? This removes your downloaded copy and can't be undone."
        : "Remove this from the room? Your original file on disk is not touched.";
    if (!confirm(msg)) return;
    try {
        await window.go.main.App.RemoveFlipLocally(id);
        for (const m of state.flipsByPeer.values()) m.delete(id);
        const card = document.querySelector(`[data-flip="${id}"]`);
        if (card) card.remove();
    } catch (e) { toast("remove: " + e); }
}

async function deleteMessageFlow(m) {
    if (!confirm("Delete this message for everyone? They'll see [deleted].")) return;
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

// flipVisibleNow reports whether a flip belongs in the currently open view,
// scoped strictly by conversation: a 1:1 flip (no boothId) shows only in the
// 1:1 with its peer; a booth flip shows only in that booth. This is what keeps
// a file sent in one room from leaking into another that shares a member.
function flipVisibleNow(f) {
    if (!state.selection) return false;
    if (state.selection.kind === "peer") {
        return !f.boothId && f.peer === state.selection.key;
    }
    return f.boothId === state.selection.key;
}

function renderFlipCard(f, container) {
    let card = findByDataset(container, "flip", f.id);
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
    } else if (f.status === "queued") {
        statusLine = "waiting · sends when someone joins";
    } else if (f.status === "failed") {
        statusLine = "failed";
    } else if (f.status === "cancelled") {
        statusLine = "cancelled";
    }

    const progressPct = (f.size > 0 && f.bytes != null) ? Math.min(100, Math.round((f.bytes / f.size) * 100)) : 0;
    const showProgress = f.status === "started" || (f.status !== "complete" && f.bytes != null);

    const preview = renderFlipPreview(f);

    const actionItems = [];
    if (f.status === "complete" && f.direction === "in") {
        actionItems.push({ label: "open", className: "", title: "", action: () => openFlipExt(f.id, f.filename) });
    }
    // Remove-from-my-device: available on any finished/parked transfer.
    if (f.status === "complete" || f.status === "queued" || f.status === "failed" || f.status === "cancelled") {
        actionItems.push({ label: "remove", className: "flip-remove", title: "remove from my device", action: () => removeFlip(f.id, f.direction) });
    }

    card.innerHTML = `
        <div class="meta">${escapeAttr(senderLabel)} - ${escapeAttr(ts)}</div>
        <div class="file-row">
            <span class="file-icon">📎</span>
            <span class="file-name">${escapeAttr(f.filename || "")}</span>
            <span class="file-size">${escapeAttr(sizeStr)}</span>
        </div>
        ${showProgress ? `<div class="progress"><div style="width:${progressPct}%"></div></div>` : ""}
        <div class="file-status">${escapeAttr(statusLine)}</div>
        ${preview}
        <div class="actions"></div>
    `;
    const actions = card.querySelector(".actions");
    for (const item of actionItems) {
        const b = document.createElement("button");
        if (item.className) b.className = item.className;
        if (item.title) b.title = item.title;
        b.textContent = item.label;
        b.addEventListener("click", (e) => { e.stopPropagation(); item.action(); });
        actions.appendChild(b);
    }
    if (!actionItems.length) actions.remove();
}

function renderFlipPreview(f) {
    if ((f.status !== "complete" && f.status !== "queued") || !f.catchUrl) return "";
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
        const phid = "txt-" + safeDomId(f.id);
        queueMicrotask(() => fillTextPreview(phid, url));
        return `<div class="preview"><pre id="${escapeAttr(phid)}">(loading)</pre></div>`;
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

// Extensions that ShellExecute would RUN rather than just open in a viewer.
// "open" goes through the OS handler, so an attacker-named file like
// "vacation.jpg.exe" or a .lnk/.bat could execute on a single click — warn
// first so a received file can't quietly run code.
const DANGEROUS_OPEN_EXT = new Set([
    "exe","scr","com","pif","bat","cmd","msi","msp","cpl","jar",
    "js","jse","vbs","vbe","wsf","wsh","ps1","psm1","hta","reg",
    "lnk","url","scf","inf","dll","sys","gadget","mst","msc"
]);

async function openFlipExt(id, filename) {
    const ext = String(filename || "").split(".").pop().toLowerCase();
    if (DANGEROUS_OPEN_EXT.has(ext)) {
        if (!confirm(`"${filename}" can run programs on your computer. Files from other people can be harmful — only open this if you trust who sent it. Open anyway?`)) return;
    }
    try { await window.go.main.App.OpenFlipExternally(id); }
    catch (e) { toast("open: " + e); }
}
window.openFlipExt = openFlipExt;

function renderChat() {
    const list = $("messages");
    list.innerHTML = "";
    renderPinnedBanner();
    const welcome = $("welcome");
    if (!state.selection) {
        $("chat-title").textContent = "Welcome";
        $("chat-state").textContent = "";
        $("composerInput").disabled = true;
        $("sendBtn").disabled = true;
        $("attachBtn").disabled = true;
        $("copyInviteBtn").classList.add("hidden");
        if (welcome) welcome.classList.remove("hidden");
        list.classList.add("hidden");
        return;
    }
    if (welcome) welcome.classList.add("hidden");
    list.classList.remove("hidden");
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
    const peer = state.peers.find(p => p.name === name);
    $("chat-title").textContent = peer ? (peer.displayName || peer.name) : name;
    $("chat-state").textContent = peer && peer.connected ? "live" : "offline";
    const canSend = !!(peer && peer.connected);
    $("composerInput").disabled = !canSend;
    $("sendBtn").disabled = !canSend;
    $("attachBtn").disabled = !canSend;
    $("composerInput").placeholder = canSend
        ? "type a message… Shift+Enter for a new line"
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
        return;
    }
    $("chat-title").textContent = booth.name;
    // Show who's actually HERE now (connected members), not the full historical
    // roster — that roster accumulates everyone who ever joined, including stale
    // ids from old builds (e.g. the pre-fix default "fliporium-gui").
    const selfId = state.self && state.self.hostname;
    const headerMembers = new Set(booth.members || []);
    const hereNames = state.peers
        .filter(p => p.connected && p.name !== selfId && headerMembers.has(p.name))
        .map(p => p.displayName || p.name);
    $("chat-state").textContent = hereNames.length
        ? (hereNames.join(", ") + " here")
        : "just you right now — share the invite link to bring people in";
    $("composerInput").disabled = false;
    $("sendBtn").disabled = false;
    $("attachBtn").disabled = false; // booth-flip
    $("composerInput").placeholder = "post to " + booth.name + " … Shift+Enter for a new line";
    // Flip cards for this room, scoped strictly by booth id (each flip carries
    // the room it was sent in), so files never bleed across rooms sharing a member.
    const flipCards = [];
    for (const m of state.flipsByPeer.values()) {
        for (const f of m.values()) {
            if (f.boothId === boothId) {
                flipCards.push({ kind: "flip", at: f.startedAt || f.at, data: f });
            }
        }
    }
    const items = timelineForBooth(boothId).concat(flipCards);
    items.sort((a, b) => new Date(a.at) - new Date(b.at));
    for (const item of items) {
        if (item.kind === "msg") renderMessage(item.data, list);
        else if (item.kind === "flip") renderFlipCard(item.data, list);
    }
}

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
    // Reload the room's media history so files persist across restarts, not
    // just text. Flips are stored per-peer, so seed each sender's map.
    try {
        const flips = await window.go.main.App.ListBoothFlips(boothId);
        for (const f of (flips || [])) {
            if (!state.flipsByPeer.has(f.peer)) state.flipsByPeer.set(f.peer, new Map());
            state.flipsByPeer.get(f.peer).set(f.id, f);
        }
    } catch (e) { /* leave media empty on error */ }

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
            state.unreadBooths.set(m.boothId, (state.unreadBooths.get(m.boothId) || 0) + 1);
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
        state.unread.set(m.peer, (state.unread.get(m.peer) || 0) + 1);
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
        if (f.boothId) state.unreadBooths.set(f.boothId, (state.unreadBooths.get(f.boothId) || 0) + 1);
        else state.unread.set(f.peer, (state.unread.get(f.peer) || 0) + 1);
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

// removeBoothLocal drops a room from the UI after a leave/delete/decline.
function removeBoothLocal(id) {
    state.booths = state.booths.filter(b => b.id !== id);
    state.msgsByBooth.delete(id);
    state.unreadBooths.delete(id);
    if (isBoothSelected(id)) {
        state.selection = null;
        renderChat();
    }
    renderFloor();
}

// ---------- top bar / self ----------

function renderSelf() {
    const el = $("selfName");
    const av = $("selfAvatar");
    if (!state.self) {
        el.textContent = "starting...";
        $("selfDot").className = "dot offline";
        if (av) av.innerHTML = "";
        $("selfIp").textContent = "";
        return;
    }
    el.textContent = state.self.displayName || state.self.hostname || "-";
    el.title = "click to change your name";
    el.style.cursor = "pointer";
    el.onclick = editDisplayName;
    $("selfDot").className = "dot " + (state.self.online ? "connected" : "offline");
    if (av) {
        fillAvatar(av, state.self.hostname, state.self.displayName);
        av.title = "change your picture in Backstage";
    }
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
        $("bs-self").innerHTML = `<strong>${escapeAttr(state.self.displayName || state.self.hostname || "")}</strong>`;
        fillAvatar($("bs-avatar"), state.self.hostname, state.self.displayName);
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
    // Blocked
    try {
        const blocked = await window.go.main.App.ListBlocked();
        renderBlockedList(blocked || []);
    } catch (e) { renderBlockedList([]); }
}

function renderBlockedList(list) {
    const el = $("bs-blocked");
    if (!el) return;
    if (!list.length) {
        el.innerHTML = `<div class="bs-hint">No one blocked.</div>`;
        return;
    }
    el.innerHTML = "";
    for (const p of list) {
        const row = document.createElement("div");
        row.className = "bs-row";
        const label = document.createElement("span");
        label.style.flex = "1";
        label.style.minWidth = "0";
        label.style.overflow = "hidden";
        label.style.textOverflow = "ellipsis";
        label.textContent = p.displayName || p.name;
        const btn = document.createElement("button");
        btn.className = "ghost";
        btn.textContent = "unblock";
        btn.addEventListener("click", async () => {
            try { await window.go.main.App.UnblockPeer(p.name); paintBackstage(); }
            catch (e) { toast("unblock: " + e); }
        });
        row.appendChild(label);
        row.appendChild(btn);
        el.appendChild(row);
    }
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
        // The FTS snippet wraps matches in sentinel control chars (\x02 .. \x03)
        // rather than real <mark> tags, so we HTML-escape the whole thing
        // (closing the XSS hole where message text would be raw HTML) and only
        // THEN turn the sentinels into <mark>.
        const openMark = String.fromCharCode(2), closeMark = String.fromCharCode(3);
        const snip = h.snippet
            ? escapeAttr(h.snippet).split(openMark).join("<mark>").split(closeMark).join("</mark>")
            : escapeAttr((h.text || "").slice(0, 120));
        div.innerHTML = `
            <div class="where">${where} · ${escapeAttr(when)}</div>
            <div class="snippet">${snip}</div>
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
    $("bs-avatar-pick").addEventListener("click", async () => {
        try {
            const uri = await window.go.main.App.PickAvatar();
            if (state.self) state.self.avatar = uri || "";
            renderSelf(); paintBackstage(); renderFloor(); renderChat();
            if (uri) toast("picture updated — friends see it on reconnect");
        } catch (e) { toast("couldn't set picture: " + e); }
    });
    $("bs-avatar-clear").addEventListener("click", async () => {
        try {
            await window.go.main.App.ClearAvatar();
            if (state.self) state.self.avatar = "";
            renderSelf(); paintBackstage(); renderFloor(); renderChat();
            toast("picture removed");
        } catch (e) { toast("couldn't remove picture: " + e); }
    });
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
    { icon: "🎪", title: "Booths", body: "A Booth is a named group room. Share its invite link and everyone who has it can chat and flip files together. The + next to Rooms starts a new one." },
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

// ---------- first-launch name prompt ----------

function showNameError(msg) {
    const el = $("name-error");
    el.textContent = msg;
    el.classList.remove("hidden");
}

function bindNameSetup() {
    const submit = async () => {
        const v = $("name-input").value.trim();
        if (!v) { showNameError("pick a name — anything you like"); return; }
        try {
            await window.go.main.App.SetDisplayName(v);
            if (state.self) state.self.displayName = v;
            renderSelf();
        } catch (e) { showNameError(String(e)); return; }
        $("name-overlay").classList.add("hidden");
        maybeShowTourOnBoot();
    };
    $("name-go").addEventListener("click", submit);
    $("name-input").addEventListener("keydown", (e) => {
        if (e.key === "Enter") { e.preventDefault(); submit(); }
    });
}

// maybeFirstRun shows the name prompt the very first time (no name chosen yet);
// otherwise it goes straight to the tour. Gating on an unset display_name means
// existing installs (which never persisted a name — they were borrowing the OS
// username) also get prompted once.
async function maybeFirstRun() {
    let name = "";
    try { name = await window.go.main.App.GetPref("display_name"); } catch (e) {}
    if (!name) {
        $("name-overlay").classList.remove("hidden");
        $("name-input").focus();
    } else {
        maybeShowTourOnBoot();
    }
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
    const colors = ["#5cb2f2", "#4ade80", "#ffd166", "#ef4444", "#5bc0eb"];
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

// autoGrowComposer resizes the message textarea to fit its content, up to a cap
// (then it scrolls). Called on input and after sending/clearing.
function autoGrowComposer() {
    const ci = $("composerInput");
    if (!ci) return;
    ci.style.height = "auto";
    ci.style.height = Math.min(ci.scrollHeight, 160) + "px";
}

function bindComposer() {
    $("composer").addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const text = $("composerInput").value;
        if (!text.trim() || !state.selection) return;
        const replyTo = state.replyingTo ? state.replyingTo.uuid : "";
        try {
            if (state.selection.kind === "booth") {
                await window.go.main.App.SendBoothMessage(state.selection.key, text, replyTo);
            } else if (replyTo) {
                await window.go.main.App.SendReply(state.selection.key, text, replyTo);
            } else {
                await window.go.main.App.SendMessage(state.selection.key, text);
            }
            $("composerInput").value = "";
            autoGrowComposer();
            cancelReply();
        } catch (e) {
            toast("send: " + e);
        }
    });

    const composerInput = $("composerInput");
    composerInput.addEventListener("input", autoGrowComposer);
    composerInput.addEventListener("keydown", (e) => {
        // Enter sends; Shift+Enter inserts a newline (for multi-line messages and
        // code fences). isComposing guards against submitting mid-IME-composition.
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            $("composer").requestSubmit();
        }
    });

    $("reply-cancel").addEventListener("click", cancelReply);

    // Paste an image straight into the chat: route it to a flip rather than the
    // text field. Plain-text paste falls through to default behavior.
    $("composerInput").addEventListener("paste", async (e) => {
        const items = (e.clipboardData && e.clipboardData.items) || [];
        for (const it of items) {
            if (it.kind === "file" && it.type && it.type.startsWith("image/")) {
                const blob = it.getAsFile();
                if (blob) {
                    e.preventDefault();
                    await sendImageBlob(blob, it.type);
                    return;
                }
            }
        }
    });

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
        // Chime if this is an inbound message for a chat we're not currently
        // viewing — unless that conversation is muted.
        if (m.direction === "in") {
            const focused = (m.boothId && isBoothSelected(m.boothId)) || (!m.boothId && isPeerSelected(m.peer));
            if (!focused && !conversationMuted(m)) maybeChime();
        }
    });

    window.runtime.EventsOn("reaction", (r) => applyReaction(r));
    window.runtime.EventsOn("message-edited", (e) => applyMessageEdit(e));
    window.runtime.EventsOn("message-deleted", (d) => applyMessageDelete(d));
    window.runtime.EventsOn("message-pinned", (p) => applyMessagePin(p));
    window.runtime.EventsOn("message-card", (mc) => applyMessageCard(mc));
    window.runtime.EventsOn("peer-status", (s) => {
        state.peerStatus.set(s.peer, s.status);
        renderFloor();
    });

    window.runtime.EventsOn("flip", (f) => upsertFlip(f));
    window.runtime.EventsOn("notice", (msg) => toast(msg, 4000));

    window.runtime.EventsOn("booth", (b) => upsertBooth(b));
    window.runtime.EventsOn("booth-removed", (id) => removeBoothLocal(id));
    window.runtime.EventsOn("blocklist-changed", () => { refreshPeers(); });
    window.runtime.EventsOn("invite-resolved", () => { refreshBooths(); });

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
    await loadMuted();
    bindComposer();
    bindSearch();
    bindBackstage();
    bindTour();
    bindDragDrop();
    bindEvents();
    bindStatusTracking();
    bindContextMenu();
    bindNameSetup();
    bindFloorFilter();
    bindHelp();
    bindWindowControls();
    // Open http(s) links (markdown links and linkified bare URLs) in the OS
    // browser instead of navigating the app's own webview.
    document.addEventListener("click", (e) => {
        const a = e.target.closest && e.target.closest('a[href]');
        if (!a) return;
        const href = a.getAttribute("href");
        if (externalURL(href)) {
            e.preventDefault();
            openExternal(href);
        } else {
            e.preventDefault();
        }
    });
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
        } else if (status.message) {
            toast(status.message, 4000);
        }
    } catch (e) { console.warn("initial status:", e); }

    await maybeFirstRun();

    setInterval(refreshPeers, 15000);
    setInterval(refreshBooths, 30000);
}

window.addEventListener("DOMContentLoaded", boot);
