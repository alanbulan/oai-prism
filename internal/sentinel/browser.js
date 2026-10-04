// 给 goja 里的 Sentinel SDK 搭一个"像 Chrome 打开 Prism 首页"的页面环境。
//
// sdk.js 本身原样执行（它自带 dx 虚拟机），这里只负责它会读到的那部分浏览器：
// navigator / screen / document / window 键集 / localStorage / 计时 / 字体测量……
// 取值全部来自 Profile（一台真实浏览器的指纹快照），形状尽量贴近 Chrome：
// 接口挂在原型上、Symbol.toStringTag、原生函数 toString、V8 风格的报错文案。
//
// 约定：Go 调 __sentinelBoot(profileJSON, host) 一次，拿回 {deliver, token, loaded}；
// 之后这个全局函数被删除。除 Chrome 页面本来就有的全局外不留任何痕迹。
"use strict";
globalThis.__sentinelBoot = function (profileJSON, host) {
  const P = JSON.parse(profileJSON);
  const G = globalThis;
  const define = Object.defineProperty;

  // ---------------- 原生函数外观 ----------------
  const nativeNames = new WeakMap();
  const fnToString = Function.prototype.toString;
  function native(fn, name) {
    const n = name === undefined ? fn.name : name;
    nativeNames.set(fn, n);
    try { define(fn, "name", { value: n, configurable: true }); } catch (e) { /* 忽略 */ }
    return fn;
  }
  // 方法简写定义的函数没有 prototype、不可 new，更像原生函数。
  function fnLike(name, impl) {
    const f = ({ [name](...a) { return impl.apply(this, a); } })[name];
    return native(f, name);
  }
  native(Function.prototype.toString = ({ toString() {
    if (nativeNames.has(this)) return "function " + nativeNames.get(this) + "() { [native code] }";
    return fnToString.call(this);
  } }).toString, "toString");
  define(Function.prototype, "toString", { enumerable: false });

  // goja 的报错文案改成 V8 的（dx 会把 ""+err 采进指纹）。
  const errToString = Error.prototype.toString;
  function v8msg(s) {
    return String(s)
      .replace(/Cannot read property '([^']*)' of (undefined|null)/g, "Cannot read properties of $2 (reading '$1')")
      .replace(/Cannot set property '([^']*)' of (undefined|null)/g, "Cannot set properties of $2 (setting '$1')");
  }
  define(Error.prototype, "toString", { value: native(({ toString() { return v8msg(errToString.call(this)); } }).toString, "toString"), writable: true, configurable: true });

  // ---------------- WebIDL 式接口 ----------------
  // iface 造一个"非法构造"的接口：属性/方法挂原型上且可枚举（Chrome 的 WebIDL 就是这样）。
  function iface(name, members, parent) {
    const ctor = native(function () { throw new TypeError("Illegal constructor"); }, name);
    const proto = Object.create(parent ? parent.prototype : Object.prototype);
    define(proto, Symbol.toStringTag, { value: name, configurable: true });
    define(proto, "constructor", { value: ctor, writable: true, configurable: true });
    define(ctor, "prototype", { value: proto, writable: false });
    if (members) addMembers(proto, members);
    return ctor;
  }
  // members: { 名字: 值 | getter(fnGet) | 方法(fnMethod) }
  const GET = Symbol("get"), METHOD = Symbol("method");
  function getter(fn) { return { [GET]: fn }; }
  function method(fn) { return { [METHOD]: fn }; }
  function addMembers(proto, members) {
    for (const k of Object.keys(members)) {
      const m = members[k];
      if (m && typeof m === "object" && METHOD in m) {
        define(proto, k, { value: fnLike(k, m[METHOD]), writable: true, enumerable: true, configurable: true });
      } else if (m && typeof m === "object" && GET in m) {
        define(proto, k, { get: native(({ get() { return m[GET].call(this); } }).get, "get " + k), enumerable: true, configurable: true });
      } else {
        define(proto, k, { get: native(({ get() { return m; } }).get, "get " + k), enumerable: true, configurable: true });
      }
    }
  }
  function instance(ctor) { return Object.create(ctor.prototype); }
  const objectOf = {};
  function opaque(name) { // [object Name] 形状的对象，同名接口复用
    if (!objectOf[name]) objectOf[name] = iface(name);
    return instance(objectOf[name]);
  }

  // ---------------- 时间与随机 ----------------
  const now = () => host.now();
  const timeOrigin = host.timeOrigin();
  Math.random = native(({ random() { return host.random(); } }).random, "random");

  const tzOffset = P.timezone.offset; // getTimezoneOffset() 的值，东八区 = -480
  const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  const pad = (n, w) => String(n).padStart(w || 2, "0");
  const DP = Date.prototype;
  const utc = {
    y: DP.getUTCFullYear, mo: DP.getUTCMonth, d: DP.getUTCDate, wd: DP.getUTCDay,
    h: DP.getUTCHours, mi: DP.getUTCMinutes, s: DP.getUTCSeconds, ms: DP.getUTCMilliseconds,
  };
  const valueOf = DP.valueOf;
  function local(dt) { return new Date(valueOf.call(dt) - tzOffset * 60000); }
  function gmt() {
    const o = -tzOffset, sign = o >= 0 ? "+" : "-", a = Math.abs(o);
    return "GMT" + sign + pad(Math.floor(a / 60)) + pad(a % 60);
  }
  function dateStr(l) { return DAYS[utc.wd.call(l)] + " " + MONTHS[utc.mo.call(l)] + " " + pad(utc.d.call(l)) + " " + utc.y.call(l); }
  function timeStr(l) { return pad(utc.h.call(l)) + ":" + pad(utc.mi.call(l)) + ":" + pad(utc.s.call(l)) + " " + gmt() + " (" + P.timezone.name + ")"; }
  const dateMethods = {
    toString() { if (isNaN(valueOf.call(this))) return "Invalid Date"; const l = local(this); return dateStr(l) + " " + timeStr(l); },
    toDateString() { return dateStr(local(this)); },
    toTimeString() { return timeStr(local(this)); },
    getTimezoneOffset() { return isNaN(valueOf.call(this)) ? NaN : tzOffset; },
    getFullYear() { return utc.y.call(local(this)); },
    getMonth() { return utc.mo.call(local(this)); },
    getDate() { return utc.d.call(local(this)); },
    getDay() { return utc.wd.call(local(this)); },
    getHours() { return utc.h.call(local(this)); },
    getMinutes() { return utc.mi.call(local(this)); },
    getSeconds() { return utc.s.call(local(this)); },
    getMilliseconds() { return utc.ms.call(local(this)); },
  };
  for (const k of Object.keys(dateMethods)) define(DP, k, { value: native(dateMethods[k], k), writable: true, configurable: true });

  // ---------------- 计时器 ----------------
  const timerFns = {
    setTimeout(fn, ms, ...a) { return typeof fn === "function" ? host.setTimeout(fn, +ms || 0, a, false) : 0; },
    clearTimeout(id) { host.clearTimeout(+id || 0); },
    setInterval(fn, ms, ...a) { return typeof fn === "function" ? host.setTimeout(fn, Math.max(+ms || 0, 1), a, true) : 0; },
    clearInterval(id) { host.clearTimeout(+id || 0); },
    requestIdleCallback(cb) {
      return host.setTimeout(() => {
        const end = now() + 49.9;
        cb({ didTimeout: false, timeRemaining: () => Math.max(0, end - now()) });
      }, 0, [], false);
    },
    cancelIdleCallback(id) { host.clearTimeout(+id || 0); },
    requestAnimationFrame(cb) { return host.setTimeout(() => cb(now()), 16, [], false); },
    cancelAnimationFrame(id) { host.clearTimeout(+id || 0); },
    setImmediate(fn, ...a) { return host.setTimeout(fn, 0, a, false); },
    clearImmediate(id) { host.clearTimeout(+id || 0); },
    queueMicrotask(fn) { Promise.resolve().then(fn); },
  };

  // ---------------- 编码 / URL ----------------
  class DOMException extends Error {
    constructor(message, name) { super(message === undefined ? "" : String(message)); define(this, "name", { value: name === undefined ? "Error" : String(name), configurable: true, writable: true }); }
  }
  native(DOMException, "DOMException");
  function btoa(s) {
    const r = host.btoa(String(s));
    if (r === null) throw new DOMException("Failed to execute 'btoa' on 'Window': The string to be encoded contains characters outside of the Latin1 range.", "InvalidCharacterError");
    return r;
  }
  function atob(s) {
    const r = host.atob(String(s));
    if (r === null) throw new DOMException("Failed to execute 'atob' on 'Window': The string to be decoded is not correctly encoded.", "InvalidCharacterError");
    return r;
  }
  class TextEncoder {
    get encoding() { return "utf-8"; }
    encode(s) { return new Uint8Array(host.utf8(s === undefined ? "" : String(s))); }
  }
  native(TextEncoder, "TextEncoder");
  class TextDecoder {
    get encoding() { return "utf-8"; }
    decode(b) { return host.utf8decode(b ? new Uint8Array(b.buffer || b).buffer : new ArrayBuffer(0)); }
  }
  native(TextDecoder, "TextDecoder");
  class URLSearchParams {
    constructor(init) {
      this._l = [];
      let s = init === undefined || init === null ? "" : String(init);
      if (s[0] === "?") s = s.slice(1);
      for (const part of s.split("&")) {
        if (!part) continue;
        const i = part.indexOf("=");
        const dec = (x) => { try { return decodeURIComponent(x.replace(/\+/g, " ")); } catch (e) { return x; } };
        this._l.push(i < 0 ? [dec(part), ""] : [dec(part.slice(0, i)), dec(part.slice(i + 1))]);
      }
    }
    get(k) { const e = this._l.find((x) => x[0] === k); return e ? e[1] : null; }
    has(k) { return this._l.some((x) => x[0] === k); }
    keys() { return this._l.map((x) => x[0])[Symbol.iterator](); }
    values() { return this._l.map((x) => x[1])[Symbol.iterator](); }
    entries() { return this._l.map((x) => [x[0], x[1]])[Symbol.iterator](); }
    [Symbol.iterator]() { return this.entries(); }
    toString() { return this._l.map((x) => encodeURIComponent(x[0]) + "=" + encodeURIComponent(x[1])).join("&"); }
  }
  native(URLSearchParams, "URLSearchParams");
  class URL {
    constructor(u, base) {
      const p = JSON.parse(host.parseURL(String(u), base === undefined ? "" : String(base)));
      for (const k of Object.keys(p)) define(this, "_" + k, { value: p[k], writable: true });
    }
    get href() { return this._href; }
    get origin() { return this._origin; }
    get protocol() { return this._protocol; }
    get host() { return this._host; }
    get hostname() { return this._hostname; }
    get port() { return this._port; }
    get pathname() { return this._pathname; }
    get search() { return this._search; }
    get hash() { return this._hash; }
    get searchParams() { return new URLSearchParams(this._search); }
    toString() { return this._href; }
    toJSON() { return this._href; }
  }
  native(URL, "URL");

  // ---------------- crypto ----------------
  const Crypto = iface("Crypto", {
    getRandomValues: method(function (a) {
      const b = new Uint8Array(host.randomBytes(a.byteLength));
      new Uint8Array(a.buffer, a.byteOffset, a.byteLength).set(b);
      return a;
    }),
    randomUUID: method(function () {
      const b = new Uint8Array(host.randomBytes(16));
      b[6] = (b[6] & 15) | 64; b[8] = (b[8] & 63) | 128;
      const h = Array.from(b, (x) => (x + 256).toString(16).slice(1)).join("");
      return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
    }),
    subtle: opaque("SubtleCrypto"),
  });
  const crypto = instance(Crypto);

  // ---------------- screen ----------------
  const S = P.screen;
  const ScreenOrientation = iface("ScreenOrientation", { type: S.orientation || "landscape-primary", angle: 0, onchange: null });
  const Screen = iface("Screen", {
    availWidth: S.availWidth, availHeight: S.availHeight, width: S.width, height: S.height,
    colorDepth: S.colorDepth, pixelDepth: S.pixelDepth, availLeft: S.availLeft, availTop: S.availTop,
    orientation: instance(ScreenOrientation), onchange: null, isExtended: !!S.isExtended,
  });
  const screen = instance(Screen);

  // ---------------- performance ----------------
  const M = P.memory;
  const MemoryInfo = iface("MemoryInfo", {
    jsHeapSizeLimit: M.jsHeapSizeLimit,
    totalJSHeapSize: getter(() => M.totalJSHeapSize + Math.floor(now() * 37) % 4096 * 16),
    usedJSHeapSize: getter(() => M.usedJSHeapSize + Math.floor(now() * 53) % 8192 * 16),
  });
  const memory = instance(MemoryInfo);
  const Performance = iface("Performance", {
    timeOrigin: timeOrigin,
    now: method(() => now()),
    memory: memory,
    timing: opaque("PerformanceTiming"),
    navigation: opaque("PerformanceNavigation"),
    onresourcetimingbufferfull: null,
    eventCounts: opaque("EventCounts"),
    getEntries: method(() => []),
    getEntriesByType: method(() => []),
    getEntriesByName: method(() => []),
    mark: method(() => undefined),
    measure: method(() => undefined),
    clearMarks: method(() => undefined),
    clearMeasures: method(() => undefined),
    toJSON: method(() => ({ timeOrigin })),
  });
  const performance = instance(Performance);

  // ---------------- navigator ----------------
  const N = P.navigator;
  const navMembers = {};
  for (const item of N.proto) {
    const [k, kind, v] = item;
    switch (kind) {
      case "fn": navMembers[k] = method(function () { return undefined; }); break;
      case "obj": navMembers[k] = navObject(k, v); break;
      default: navMembers[k] = v; // 字符串 / 数字 / 布尔 / null
    }
  }
  navMembers.languages = Object.freeze(N.languages.slice());
  function navObject(k, tag) {
    const o = opaque(tag);
    switch (tag) {
      case "PluginArray": case "MimeTypeArray": {
        const names = tag === "PluginArray" ? N.plugins : N.mimeTypes;
        names.forEach((n, i) => { const it = opaque(tag === "PluginArray" ? "Plugin" : "MimeType"); define(it, "name", { value: n }); define(o, i, { value: it, enumerable: true }); });
        define(o, "length", { value: names.length });
        define(o, "item", { value: fnLike("item", (i) => o[i] || null) });
        define(o, "namedItem", { value: fnLike("namedItem", () => null) });
        return o;
      }
      case "NavigatorUAData": {
        const ua = N.uaData;
        define(o, "brands", { get: () => ua.brands.map((b) => Object.assign({}, b)), enumerable: true });
        define(o, "mobile", { get: () => false, enumerable: true });
        define(o, "platform", { get: () => ua.platform, enumerable: true });
        // 与 Chrome 一样：总带 brands / mobile / platform，其余只给请求了的
        define(o, "getHighEntropyValues", { value: fnLike("getHighEntropyValues", (hints) => {
          const out = { brands: ua.brands.map((b) => Object.assign({}, b)), mobile: false, platform: ua.platform };
          const he = ua.highEntropy || {};
          for (const h of Array.from(hints || [])) if (Object.prototype.hasOwnProperty.call(he, h)) out[h] = JSON.parse(JSON.stringify(he[h]));
          return Promise.resolve(out);
        }) });
        define(o, "toJSON", { value: fnLike("toJSON", () => ({ brands: ua.brands, mobile: false, platform: ua.platform })) });
        return o;
      }
      case "NetworkInformation":
        for (const [a, b] of Object.entries(N.connection || {})) define(o, a, { get: () => b, enumerable: true });
        return o;
      default:
        return o;
    }
  }
  const Navigator = iface("Navigator", navMembers);
  const navigator = instance(Navigator);

  // ---------------- location / history ----------------
  const loc = JSON.parse(host.parseURL(P.page.href, ""));
  const Location = iface("Location");
  const location = instance(Location);
  for (const k of ["ancestorOrigins", "href", "origin", "protocol", "host", "hostname", "port", "pathname", "search", "hash"]) {
    define(location, k, { value: k === "ancestorOrigins" ? opaque("DOMStringList") : loc[k], enumerable: true });
  }
  for (const k of ["assign", "reload", "replace"]) define(location, k, { value: fnLike(k, () => undefined), enumerable: true });
  define(location, "toString", { value: fnLike("toString", () => loc.href), enumerable: true });

  const History = iface("History", {
    length: P.page.historyLength || 1, scrollRestoration: "auto", state: null,
    back: method(() => undefined), forward: method(() => undefined), go: method(() => undefined),
    pushState: method(() => undefined), replaceState: method(() => undefined),
  });
  const history = instance(History);
  // Next.js 的路由会把 pushState / replaceState 打补丁成实例自有属性。
  for (const k of P.page.historyOwnKeys || []) define(history, k, { value: function () {}, writable: true, enumerable: true, configurable: true });

  // ---------------- 存储 ----------------
  const Storage = iface("Storage", {
    length: getter(function () { return Object.keys(this).length; }),
    key: method(function (i) { const ks = Object.keys(this); return i < ks.length ? ks[i] : null; }),
    getItem: method(function (k) { return Object.prototype.hasOwnProperty.call(this, k) ? this[k] : null; }),
    setItem: method(function (k, v) { define(this, String(k), { value: String(v), writable: true, enumerable: true, configurable: true }); }),
    removeItem: method(function (k) { delete this[String(k)]; }),
    clear: method(function () { for (const k of Object.keys(this)) delete this[k]; }),
  });
  function storage(items) {
    const s = instance(Storage);
    for (const [k, v] of items || []) define(s, k, { value: String(v), writable: true, enumerable: true, configurable: true });
    return s;
  }
  const localStorage = storage(P.page.localStorage);
  const sessionStorage = storage(P.page.sessionStorage);

  // ---------------- 事件 ----------------
  const listeners = new Map(); // target -> type -> [fn]
  function on(target, type, fn) {
    if (typeof fn !== "function") return;
    if (!listeners.has(target)) listeners.set(target, {});
    const m = listeners.get(target);
    (m[type] = m[type] || []).push(fn);
  }
  function off(target, type, fn) {
    const m = listeners.get(target);
    if (m && m[type]) m[type] = m[type].filter((f) => f !== fn);
  }
  function emit(target, type, ev) {
    const m = listeners.get(target);
    for (const fn of (m && m[type]) || []) {
      try { fn.call(target, ev); } catch (e) { host.log("listener error: " + e); }
    }
  }
  const EventTarget = iface("EventTarget", {
    addEventListener: method(function (t, fn) { on(this, String(t), fn); }),
    removeEventListener: method(function (t, fn) { off(this, String(t), fn); }),
    dispatchEvent: method(function (ev) { emit(this, ev && ev.type, ev); return true; }),
  });
  class Event {
    constructor(type, init) { define(this, "type", { value: String(type), enumerable: true }); Object.assign(this, init || {}); }
  }
  native(Event, "Event");
  class CustomEvent extends Event {
    constructor(type, init) { super(type, init); this.detail = init && init.detail !== undefined ? init.detail : null; }
  }
  native(CustomEvent, "CustomEvent");

  // ---------------- DOM ----------------
  const Node = iface("Node", {}, EventTarget);
  const Element = iface("Element", {}, Node);
  const HTMLElement = iface("HTMLElement", {}, Element);
  const elemCtors = {};
  function elemCtor(tag) {
    const name = { div: "HTMLDivElement", span: "HTMLSpanElement", iframe: "HTMLIFrameElement", script: "HTMLScriptElement", canvas: "HTMLCanvasElement", a: "HTMLAnchorElement", p: "HTMLParagraphElement", body: "HTMLBodyElement", head: "HTMLHeadElement", html: "HTMLHtmlElement" }[tag] || "HTMLUnknownElement";
    if (!elemCtors[name]) elemCtors[name] = iface(name, {}, HTMLElement);
    return elemCtors[name];
  }
  function makeElement(tag) {
    tag = String(tag).toLowerCase();
    const el = instance(elemCtor(tag));
    const attrs = {};
    let text = "", parent = null;
    const children = [];
    define(el, "tagName", { value: tag.toUpperCase() });
    define(el, "nodeName", { value: tag.toUpperCase() });
    define(el, "nodeType", { value: 1 });
    define(el, "style", { value: {}, enumerable: true });
    define(el, "dataset", { value: {} });
    define(el, "children", { get: () => children.slice() });
    define(el, "childNodes", { get: () => children.slice() });
    define(el, "parentNode", { get: () => parent });
    define(el, "parentElement", { get: () => parent });
    define(el, "isConnected", { get: () => !!parent });
    for (const k of ["innerText", "textContent", "innerHTML", "outerText"]) {
      define(el, k, { get: () => text, set: (v) => { text = String(v); }, enumerable: true });
    }
    define(el, "__setParent", { value: (p) => { parent = p; } });
    define(el, "setAttribute", { value: fnLike("setAttribute", (k, v) => { attrs[String(k).toLowerCase()] = String(v); }) });
    define(el, "getAttribute", { value: fnLike("getAttribute", (k) => { const a = attrs[String(k).toLowerCase()]; return a === undefined ? null : a; }) });
    define(el, "hasAttribute", { value: fnLike("hasAttribute", (k) => attrs[String(k).toLowerCase()] !== undefined) });
    define(el, "removeAttribute", { value: fnLike("removeAttribute", (k) => { delete attrs[String(k).toLowerCase()]; }) });
    define(el, "appendChild", { value: fnLike("appendChild", (c) => { children.push(c); if (c && c.__setParent) c.__setParent(el); if (c && c.__mounted) c.__mounted(); return c; }) });
    define(el, "removeChild", { value: fnLike("removeChild", (c) => { const i = children.indexOf(c); if (i >= 0) children.splice(i, 1); if (c && c.__setParent) c.__setParent(null); return c; }) });
    define(el, "remove", { value: fnLike("remove", () => { if (parent) parent.removeChild(el); }) });
    define(el, "getBoundingClientRect", { value: fnLike("getBoundingClientRect", () => measure(el, text)) });
    define(el, "getClientRects", { value: fnLike("getClientRects", () => [measure(el, text)]) });
    define(el, "offsetWidth", { get: () => Math.round(measure(el, text).width) });
    define(el, "offsetHeight", { get: () => Math.round(measure(el, text).height) });
    define(el, "clientWidth", { get: () => Math.round(measure(el, text).width) });
    define(el, "clientHeight", { get: () => Math.round(measure(el, text).height) });
    define(el, "classList", { value: { add() {}, remove() {}, contains: () => false, toggle: () => false } });
    define(el, "querySelector", { value: fnLike("querySelector", () => null) });
    define(el, "querySelectorAll", { value: fnLike("querySelectorAll", () => []) });
    if (tag === "iframe") makeIframe(el);
    if (tag === "canvas") {
      define(el, "getContext", { value: fnLike("getContext", () => null) });
      define(el, "toDataURL", { value: fnLike("toDataURL", () => "data:,") });
    }
    return el;
  }

  // 字体测量模型（按真实 Chrome 标定）：Prism 页面的 Helvetica/Arial 都落到 Arial，
  // 字宽按 Arial 字形单位累加、组合附加符号不占宽，行高随页面 CSS（line-height: 1.5），
  // 结果按 LayoutUnit（1/64 px）四舍五入。position: fixed 的元素停在视口底部（y = innerHeight）。
  const W = P.fontMetrics;
  const isCombining = (c) => (c >= 0x300 && c <= 0x36f) || (c >= 0x1ab0 && c <= 0x1aff) || (c >= 0x1dc0 && c <= 0x1dff) || (c >= 0x20d0 && c <= 0x20ff) || (c >= 0xfe20 && c <= 0xfe2f);
  function measure(el, text) {
    const fs = parseFloat(el.style.fontSize) || 16;
    // 字形整形会把"字母 + 组合符"合成预组合字形（U + U+031B → Ư，后者更宽），先做 NFC。
    const t = String(text).replace(/\s+/g, " ").trim().normalize("NFC");
    let units = 0;
    for (const ch of t) {
      const c = ch.codePointAt(0);
      if (isCombining(c)) continue;
      let w = W.widths[ch];
      if (w === undefined) w = W.widths[ch.normalize("NFD")[0]]; // 带附加符号的字母与基字母等宽
      units += w !== undefined ? w : c < 0x80 ? W.ascii : c < 0x250 ? W.latin : W.wide;
    }
    const q = (v) => Math.round(v * 64) / 64;
    const x = P.page.rectX || 0, y = P.page.rectY || 0;
    if (!t) return rect(x, y, 0, 0);
    return rect(x, y, q(units * fs / W.unitsPerEm), q(fs * W.lineHeight));
  }
  const DOMRect = iface("DOMRect", {
    x: getter(function () { return this._x; }), y: getter(function () { return this._y; }),
    width: getter(function () { return this._w; }), height: getter(function () { return this._h; }),
    top: getter(function () { return this._y; }), right: getter(function () { return this._x + this._w; }),
    bottom: getter(function () { return this._y + this._h; }), left: getter(function () { return this._x; }),
    toJSON: method(function () { return { x: this._x, y: this._y, width: this._w, height: this._h, top: this._y, right: this._x + this._w, bottom: this._y + this._h, left: this._x }; }),
  });
  function rect(x, y, w, h) {
    const r = instance(DOMRect);
    define(r, "_x", { value: x }); define(r, "_y", { value: y }); define(r, "_w", { value: w }); define(r, "_h", { value: h });
    return r;
  }

  // 假 iframe：SDK 把 sentinel/req 交给 sentinel.openai.com 的 frame.html 去发，
  // 这里由 Go 代劳（Chrome TLS 指纹 + frame 自己的缓存语义），结果再以 message 事件回来。
  let frameWindow = null, sdkFrame = null;
  function makeIframe(el) {
    const win = Object.create(null);
    define(win, "postMessage", { value: fnLike("postMessage", (msg) => host.frame(JSON.stringify(msg))) });
    define(el, "contentWindow", { get: () => (el.isConnected ? win : null) });
    define(el, "__mounted", { value: () => {
      frameWindow = win; sdkFrame = el;
      // 页面里有了 iframe：window[0] 指向它，window.length 变成 1
      define(G, "0", { value: win, enumerable: true, configurable: true });
      G.length = 1;
      host.setTimeout(() => emit(el, "load", new Event("load")), 15 + Math.floor(Math.random() * 30), [], false);
    } });
  }

  const Document = iface("Document", {}, Node);
  const document = instance(Document);
  const html = makeElement("html");
  const head = makeElement("head");
  const body = makeElement("body");
  html.setAttribute("lang", P.page.lang || "zh-CN");
  html.appendChild(head); html.appendChild(body);
  let currentScript = null;
  const scriptEls = P.page.scripts.map((src) => { const s = makeElement("script"); define(s, "src", { value: src, enumerable: true }); return s; });
  define(document, "location", { get: () => location, set: () => {}, enumerable: true });
  // React 挂在 document 上的键带随机后缀（每次打开页面都不同）
  const s1 = host.reactSuffix(), s2 = host.reactSuffix();
  for (const k of P.page.documentKeys) {
    if (k === "location") continue;
    define(document, k.replace("{s1}", s1).replace("{s2}", s2), { value: {}, writable: true, enumerable: true, configurable: true });
  }
  addMembers(Document.prototype, {
    URL: P.page.href, documentURI: P.page.href, domain: loc.hostname, referrer: "",
    title: P.page.title, characterSet: "UTF-8", charset: "UTF-8", contentType: "text/html",
    compatMode: "CSS1Compat", readyState: "complete", visibilityState: "visible", hidden: false,
    documentElement: html, head: head, body: body,
    scripts: getter(() => scriptEls.slice()),
    currentScript: getter(() => currentScript),
    cookie: getter(() => P.page.cookie || ""),
    defaultView: getter(() => G),
    createElement: method((t) => makeElement(t)),
    createTextNode: method((t) => ({ nodeType: 3, textContent: String(t) })),
    getElementById: method(() => null),
    getElementsByTagName: method((t) => String(t).toLowerCase() === "script" ? scriptEls.slice() : []),
    querySelector: method(() => null),
    querySelectorAll: method(() => []),
    hasFocus: method(() => true),
  });
  // 页面里本来就挂着的 sentinel 脚本
  const loaderScript = scriptEls.find((s) => /\/backend-api\/sentinel\/sdk\.js$/.test(s.src));

  // ---------------- window ----------------
  const chrome = { app: { isInstalled: false, InstallState: { DISABLED: "disabled", INSTALLED: "installed", NOT_INSTALLED: "not_installed" }, RunningState: { CANNOT_RUN: "cannot_run", READY_TO_RUN: "ready_to_run", RUNNING: "running" } } };
  chrome.csi = fnLike("csi", () => ({ startE: Math.round(timeOrigin), onloadT: Math.round(timeOrigin + 900), pageT: now(), tran: 15 }));
  chrome.loadTimes = fnLike("loadTimes", () => ({ requestTime: timeOrigin / 1000, startLoadTime: timeOrigin / 1000, navigationType: "Other", wasFetchedViaSpdy: true, npnNegotiatedProtocol: "h2", connectionInfo: "h2" }));

  const winValues = {
    window: G, self: G, document, location, history, navigator, screen, performance, crypto,
    localStorage, sessionStorage, chrome, clientInformation: navigator,
    top: G, parent: G, frames: G, opener: null, frameElement: null, closed: false, length: 0,
    name: "", status: "", origin: loc.origin, isSecureContext: true, crossOriginIsolated: false,
    originAgentCluster: false, credentialless: false, event: undefined,
    atob: fnLike("atob", atob), btoa: fnLike("btoa", btoa),
    fetch: fnLike("fetch", () => Promise.reject(new TypeError("Failed to fetch"))),
    postMessage: fnLike("postMessage", () => undefined),
    matchMedia: fnLike("matchMedia", (q) => ({ matches: false, media: String(q), onchange: null, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} })),
    getComputedStyle: fnLike("getComputedStyle", (el) => Object.assign({ getPropertyValue: () => "" }, el && el.style)),
    structuredClone: fnLike("structuredClone", (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)))),
    getSelection: fnLike("getSelection", () => null),
    addEventListener: fnLike("addEventListener", (t, fn) => on(G, String(t), fn)),
    removeEventListener: fnLike("removeEventListener", (t, fn) => off(G, String(t), fn)),
    dispatchEvent: fnLike("dispatchEvent", (ev) => { emit(G, ev && ev.type, ev); return true; }),
  };
  for (const k of Object.keys(timerFns)) winValues[k] = fnLike(k, timerFns[k]);
  for (const [k, v] of Object.entries(P.window.values || {})) winValues[k] = v;

  // Chrome 页面里 Object.keys(window) 的顺序与内容（Profile 采集），未单独实现的按类型给个像样的值。
  const types = P.window.types || {};
  for (const k of P.window.keys) {
    let v;
    if (Object.prototype.hasOwnProperty.call(winValues, k)) v = winValues[k];
    else if (k === "0") v = undefined; // 由 iframe 挂载后补上
    else if (/^on/.test(k)) v = null;
    else switch (types[k]) {
      case "function": v = fnLike(k, () => undefined); break;
      case "number": v = 0; break;
      case "boolean": v = false; break;
      case "string": v = ""; break;
      case "undefined": v = undefined; break;
      case "null": v = null; break;
      default: v = opaque(k.charAt(0).toUpperCase() + k.slice(1));
    }
    if (k === "0") continue;
    define(G, k, { value: v, writable: true, enumerable: true, configurable: true });
  }
  // 不在 Object.keys(window) 里、但页面能直接用的接口（Chrome 上它们不可枚举）
  const hidden = { TextEncoder, TextDecoder, URL, URLSearchParams, Event, CustomEvent, DOMException, EventTarget, Navigator, Screen, Performance, Document, Storage, Location, History, Crypto, Node, Element, HTMLElement, DOMRect, MemoryInfo };
  for (const k of Object.keys(hidden)) define(G, k, { value: hidden[k], writable: true, configurable: true });
  for (const k of Object.keys(winValues)) {
    if (!Object.prototype.hasOwnProperty.call(G, k)) define(G, k, { value: winValues[k], writable: true, configurable: true });
  }
  if (typeof G.Intl === "undefined") {
    const DateTimeFormat = native(function DateTimeFormat() {
      return { resolvedOptions: () => ({ locale: P.navigator.languages[0], calendar: "gregory", numberingSystem: "latn", timeZone: P.timezone.iana, year: "numeric", month: "numeric", day: "numeric" }), format: (d) => new Date(d === undefined ? Date.now() : d).toLocaleDateString() };
    }, "DateTimeFormat");
    define(G, "Intl", { value: { DateTimeFormat }, writable: true, configurable: true });
  }

  // SDK 的 loader 预置的全局（真实页面里先于 sdk.js 执行）。
  G.__sentinel_token_pending = [];
  G.__sentinel_init_pending = [];
  G.SentinelSDK = {};
  G.__sentinel_script_loads = { attempts: [] };

  return {
    // sdk.js 执行期间 document.currentScript 指向它自己的 <script>
    beforeSDK(src) {
      const s = makeElement("script");
      define(s, "src", { value: src, enumerable: true });
      currentScript = s;
      if (loaderScript) G.__sentinel_script_loads.attempts.push({ script: s, bootstrap: loaderScript, state: "pending" });
    },
    afterSDK() {
      currentScript = null;
      if (G.__sentinel_script_loads.attempts.length) G.__sentinel_script_loads.attempts[0].state = "loaded";
    },
    // frame 回包：以 message 事件交给 SDK
    deliver(json) {
      if (!frameWindow) return;
      emit(G, "message", { type: "message", data: JSON.parse(json), origin: P.sentinelOrigin, source: frameWindow });
    },
    frameReady() { return !!(sdkFrame && sdkFrame.isConnected); },
    token(flow) { return G.SentinelSDK.token(flow); },
    accLog: null,
  };
};
