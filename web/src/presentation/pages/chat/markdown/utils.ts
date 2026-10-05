import { defaultSchema } from 'rehype-sanitize';
import type { Options as SanitizeSchema } from 'rehype-sanitize';

/**
 * Markdown 渲染的纯函数工具：清洗白名单、公式定界符归一、HTML 预览文档构造。
 */

// ---------------------------------------------------------------- 清洗白名单
//
// 模型输出里的裸 HTML（<details>、<kbd>、<br> 等）需要渲染，但不能原样进 DOM：
// 一段 <style> 就能改写整个控制台的样式，on* 属性与 javascript: 链接更不能放行。
// 以 GitHub 的白名单为基础，额外允许代码块语言与公式的 className。
const codeAttrs = (defaultSchema.attributes?.code || []).filter(
  (a) => !(Array.isArray(a) && a[0] === 'className'),
);

export const sanitizeSchema: SanitizeSchema = {
  ...defaultSchema,
  // 连同内容一起丢弃（默认只去标签、保留文本：样式表会以一段 CSS 文字出现在回复里）
  strip: [...(defaultSchema.strip || []), 'style', 'noscript'],
  attributes: {
    ...defaultSchema.attributes,
    code: [...codeAttrs, ['className', /^language-./, 'math-inline', 'math-display']],
  },
};

// ---------------------------------------------------------------- 公式定界符
//
// 模型常用 \( … \) 与 \[ … \] 写公式，remark-math 只认 $ … $ 与 $$ … $$。
// 代码块（含流式中尚未闭合的）与行内代码里的内容保持原样。
const CODE_SPLIT = /(```[\s\S]*?(?:```|$)|~~~[\s\S]*?(?:~~~|$)|`[^`\n]*`)/g;

export function normalizeMath(md: string): string {
  if (!md.includes('\\(') && !md.includes('\\[')) return md;
  return md
    .split(CODE_SPLIT)
    .map((seg, i) =>
      i % 2 === 1
        ? seg
        : seg
            .replace(/\\\[([\s\S]+?)\\\]/g, (_, m: string) => `\n$$\n${m.trim()}\n$$\n`)
            .replace(/\\\(([\s\S]+?)\\\)/g, (_, m: string) => `$${m.trim()}$`),
    )
    .join('');
}

// ---------------------------------------------------------------- 裸文档补围栏
//
// 用户说"直接输出 HTML"时，模型会把整份 <!DOCTYPE html> 文档（或一段 <svg>）当作回复本身，
// 不加 ``` 围栏。按 Markdown 解析它会被拆碎：<style>/<script> 被清洗掉，缩进四格的行变成
// 一个个 text 代码块，剩下的标题、段落散落在回复里（2026-10-05 摸糖果可视化）。
// 这里给围栏外、从行首开始的完整文档补上 ```html / ```svg 围栏，走正常的预览流程；
// 还没收到结束标签（生成中或输出被截断）时包到结尾。
const RAW_DOC_HINT = /<!doctype\s+html|<html[\s>]|<svg[\s>]/i;
const HTML_DOC_START = /^ {0,3}(?:<!doctype\s+html|<html[\s>])/i;
const SVG_START = /^ {0,3}(?:<\?xml[^>]*>\s*)?<svg[\s>]/i;
const FENCE_LINE = /^\s*(`{3,}|~{3,})(.*)$/;

/** 文档结束位置（结束标签之后）；没有结束标签时到全文末尾 */
function rawDocEnd(md: string, from: number, kind: PreviewKind): number {
  if (kind === 'html') {
    const m = /<\/html\s*>/i.exec(md.slice(from));
    return m ? from + m.index + m[0].length : md.length;
  }
  // SVG 可以嵌套：按开闭标签计数，找到与开头配对的 </svg>
  const tags = /<svg[\s>]|<\/svg\s*>/gi;
  tags.lastIndex = from;
  let depth = 0;
  for (let m = tags.exec(md); m; m = tags.exec(md)) {
    depth += m[0][1] === '/' ? -1 : 1;
    if (depth === 0) return m.index + m[0].length;
  }
  return md.length;
}

export function fenceRawDocuments(md: string): string {
  if (!RAW_DOC_HINT.test(md)) return md;
  let out = '';
  let pos = 0;
  let fence = ''; // 所在围栏的开头标记；空 = 不在围栏里
  while (pos < md.length) {
    const nl = md.indexOf('\n', pos);
    const lineEnd = nl < 0 ? md.length : nl + 1;
    const line = md.slice(pos, nl < 0 ? md.length : nl).replace(/\r$/, '');
    const f = FENCE_LINE.exec(line);
    if (fence) {
      if (f && f[1][0] === fence[0] && f[1].length >= fence.length && !f[2].trim()) fence = '';
    } else if (f && !(f[1][0] === '`' && f[2].includes('`'))) {
      fence = f[1];
    } else {
      const kind: PreviewKind | null = HTML_DOC_START.test(line) ? 'html' : SVG_START.test(line) ? 'svg' : null;
      if (kind) {
        const end = rawDocEnd(md, pos, kind);
        const doc = md.slice(pos, end).replace(/\s+$/, '');
        // 围栏比文档里最长的一串反引号还长一截（JS 模板字符串、内嵌的 Markdown 示例）
        const longest = Math.max(0, ...(doc.match(/`+/g) || []).map((s) => s.length));
        const ticks = '`'.repeat(Math.max(3, longest + 1));
        out += `${ticks}${kind}\n${doc}\n${ticks}\n`;
        // 结束标签后同一行还有文字时，从那里另起一行继续
        pos = md[end] === '\n' ? end + 1 : end;
        continue;
      }
    }
    out += md.slice(pos, lineEnd);
    pos = lineEnd;
  }
  return out;
}

// ---------------------------------------------------------------- hast 文本
interface HastLike {
  type?: string;
  value?: string;
  tagName?: string;
  properties?: Record<string, unknown>;
  children?: HastLike[];
}

/** 取节点的纯文本（高亮后的代码块也能还原出源码） */
export function nodeText(node: HastLike | undefined): string {
  if (!node) return '';
  if (node.type === 'text') return node.value || '';
  return (node.children || []).map(nodeText).join('');
}

/** 从 <pre> 节点取出 <code> 子节点、语言与源码 */
export function codeOf(pre: HastLike | undefined): { lang: string; code: string } {
  const codeEl = pre?.children?.find((c) => c.tagName === 'code');
  const cls = codeEl?.properties?.className;
  const list = Array.isArray(cls) ? cls.map(String) : typeof cls === 'string' ? cls.split(/\s+/) : [];
  const lang = (list.find((c) => c.startsWith('language-'))?.slice(9) || '').toLowerCase();
  return { lang, code: nodeText(codeEl).replace(/\n$/, '') };
}

// ---------------------------------------------------------------- HTML 预览
export type PreviewKind = 'html' | 'svg';

/** 判断代码块能否渲染预览：显式 html/svg，或未标语言但内容明显是完整文档 / SVG */
export function previewKind(lang: string, code: string): PreviewKind | null {
  if (lang === 'html' || lang === 'htm' || lang === 'xhtml') return 'html';
  if (lang === 'svg') return 'svg';
  if ((lang === '' || lang === 'xml') && /^\s*(<\?xml[^>]*>\s*)?<svg[\s>]/i.test(code)) return 'svg';
  if (lang === '' && /^\s*(<!doctype html|<html[\s>])/i.test(code)) return 'html';
  return null;
}

/**
 * 页内探针（在沙箱 iframe 里运行，以字符串形式注入）。
 *
 * 上报：
 *   width/height —— 文档的完整尺寸（父页面据此设定 iframe 视口，保证内容完整布局）；
 *   crop —— "真正的内容"所在矩形。生成页面常把内容放在一个"舞台"上：整页底色、
 *            大段留白、居中对齐、外加一圈模糊投影。从 body 往下，只要某层只有唯一一个
 *            更小的可见子元素，这层就是舞台，继续向内找，直到内容本体；
 *   transparent —— 内容自带底色（或本身就是 svg/canvas/img）时把舞台底色设为透明，
 *            圆角处不再露出舞台颜色。内容依赖舞台底色时（深色底上的白字）保留，避免看不清。
 *            同时去掉内容的外阴影（内阴影保留）：外阴影画在内容盒外，裁切后只剩
 *            四角弧线外的一块深色和左右边缘的一像素细线；
 *   radius —— 内容四角的圆角半径，父页面按同样的弧度裁剪显示区域，与内容圆角严丝合缝。
 */
const PROBE_SOURCE = `function(id,clearStage){
  var SKIP={SCRIPT:1,STYLE:1,LINK:1,META:1,TEMPLATE:1,NOSCRIPT:1,TITLE:1};
  var MEDIA={svg:1,SVG:1,CANVAS:1,IMG:1,VIDEO:1,PICTURE:1};
  var last="", cleared=false;
  function kids(el){
    var out=[];
    for(var i=0;i<el.children.length;i++){
      var c=el.children[i];
      if(SKIP[c.tagName])continue;
      var cs=getComputedStyle(c);
      if(cs.display==="none"||cs.visibility==="hidden"||cs.position==="fixed"&&cs.opacity==="0")continue;
      var r=c.getBoundingClientRect();
      if(r.width<1||r.height<1)continue;
      out.push(c);
    }
    return out;
  }
  function hasText(el){
    for(var i=0;i<el.childNodes.length;i++){var n=el.childNodes[i];if(n.nodeType===3&&n.textContent.trim())return true}
    return false;
  }
  function opaque(el){
    var cs=getComputedStyle(el);
    if(cs.backgroundImage&&cs.backgroundImage!=="none")return true;
    var m=(cs.backgroundColor||"").match(/[0-9.]+/g);
    return !!m&&(m.length<4||+m[3]>0);
  }
  function selfPaints(el){
    if(MEDIA[el.tagName]||opaque(el))return true;
    var r=el.getBoundingClientRect(),a=r.width*r.height,k=kids(el);
    for(var i=0;i<k.length;i++){
      if(!MEDIA[k[i].tagName])continue;
      var q=k[i].getBoundingClientRect();
      if(q.width*q.height>=0.9*a)return true;
    }
    return false;
  }
  // 只保留内阴影：外阴影画在内容盒外，裁切后只剩四角弧线外的一块和边缘一像素
  function innerShadows(v){
    if(!v||v==="none")return "none";
    var parts=[],depth=0,start=0;
    for(var i=0;i<v.length;i++){var c=v.charAt(i);if(c==="(")depth++;else if(c===")")depth--;else if(c===","&&!depth){parts.push(v.slice(start,i));start=i+1}}
    parts.push(v.slice(start));
    var keep=[];
    for(var j=0;j<parts.length;j++)if(parts[j].indexOf("inset")>=0)keep.push(parts[j].trim());
    return keep.length?keep.join(","):"none";
  }
  function radii(el,w,h){
    var cs=getComputedStyle(el),m=Math.min(w,h),out=[];
    var ps=["borderTopLeftRadius","borderTopRightRadius","borderBottomRightRadius","borderBottomLeftRadius"];
    for(var i=0;i<4;i++){
      var v=cs[ps[i]]||"0",n=parseFloat(v)||0;
      if(v.indexOf("%")>=0)n=n*m/100;
      out.push(Math.min(n,m/2));
    }
    return out;
  }
  function measure(){
    var d=document.documentElement,b=document.body;
    var w=Math.ceil(Math.max(d.scrollWidth,b?b.scrollWidth:0));
    var h=d.getBoundingClientRect().height;
    if(b){var bs=getComputedStyle(b);h=Math.max(h,b.scrollHeight+parseFloat(bs.marginTop)+parseFloat(bs.marginBottom))}
    h=Math.ceil(h);
    var crop=null,transparent=false,radius=null;
    if(b&&!hasText(b)){
      var node=b,stages=[d,b];
      for(var guard=0;guard<12;guard++){
        if(hasText(node))break;
        var k=kids(node);
        if(k.length!==1)break;
        var r=node.getBoundingClientRect(),q=k[0].getBoundingClientRect();
        if(q.width<r.width-1||q.height<r.height-1)stages.push(node);
        node=k[0];
      }
      // 只在内容"自成画面"时裁切：本身是 svg/canvas/img，或自带底色（卡片、场景）。
      // 内容只是文字或依赖页面底色时，页面底色就是设计的一部分，整页显示。
      // 多块内容平铺在无底色页面上时，只裁掉四周留白。
      var rect,paints=node!==b&&selfPaints(node);
      if(node===b&&!opaque(d)&&!opaque(b)){
        var ks=kids(b);
        if(ks.length){
          var x1=1e9,y1=1e9,x2=-1e9,y2=-1e9;
          for(var j=0;j<ks.length;j++){var s=ks[j].getBoundingClientRect();x1=Math.min(x1,s.left);y1=Math.min(y1,s.top);x2=Math.max(x2,s.right);y2=Math.max(y2,s.bottom)}
          rect={left:x1,top:y1,width:x2-x1,height:y2-y1};
        }
      }else if(paints){
        var nr=node.getBoundingClientRect();
        rect={left:nr.left,top:nr.top,width:Math.max(nr.width,node.scrollWidth||0),height:Math.max(nr.height,node.scrollHeight||0)};
      }
      if(rect&&rect.width>=24&&rect.height>=24){
        crop={x:Math.max(0,Math.floor(rect.left+scrollX)),y:Math.max(0,Math.floor(rect.top+scrollY)),w:Math.ceil(rect.width),h:Math.ceil(rect.height)};
        if(clearStage&&paints){
          transparent=true;
          radius=radii(node,rect.width,rect.height);
          if(!cleared){
            cleared=true;
            for(var t=0;t<stages.length;t++)stages[t].style.setProperty("background","transparent","important");
            node.style.setProperty("box-shadow",innerShadows(getComputedStyle(node).boxShadow),"important");
          }
        }
      }
    }
    var msg={oaiprismPreview:id,width:w,height:h,crop:crop,transparent:transparent,radius:radius};
    var key=JSON.stringify(msg);
    if(h&&key!==last){last=key;parent.postMessage(msg,"*")}
  }
  addEventListener("load",function(){measure();setTimeout(measure,300);setTimeout(measure,1200)});
  if(window.ResizeObserver){var ro=new ResizeObserver(measure);ro.observe(document.documentElement);if(document.body)ro.observe(document.body)}
}`;

/**
 * 构造 iframe 文档：SVG 片段包一层居中页面；注入两样东西：
 *   - 隐藏滚动条的样式：预览框按内容自适应尺寸，不需要也不应出现内部滚动条；
 *   - 尺寸探针：通过 postMessage 把内容的宽高告诉父页面
 *     （沙箱没有同源权限，父页面读不到 iframe 文档）。
 *     高度取 <html> 盒高与 body 溢出高度：documentElement.scrollHeight 不会小于视口，
 *     内容比预览框矮时就缩不回去。
 */
export function buildPreviewDoc(
  code: string,
  kind: PreviewKind,
  probe?: { id: string; clearStage: boolean },
): string {
  const doc =
    kind === 'svg' && !/<html[\s>]/i.test(code)
      ? `<!doctype html><html><head><meta charset="utf-8"><style>html,body{margin:0}body{display:grid;place-items:center;background:#fff}svg{max-width:100%;height:auto}</style></head><body>${code}</body></html>`
      : code;
  // 不带探针 = 原始页面（全屏 / 新窗口）：不隐藏滚动条、不动舞台
  if (!probe) return doc;
  const style =
    '<style>html,body{scrollbar-width:none}html::-webkit-scrollbar,body::-webkit-scrollbar{display:none}</style>';
  const script = `<script>(${PROBE_SOURCE})(${JSON.stringify(probe.id)},${probe.clearStage});</script>`;
  let out = doc;
  const head = out.search(/<\/head>/i);
  out = head >= 0 ? out.slice(0, head) + style + out.slice(head) : style + out;
  const at = out.search(/<\/body>(?![\s\S]*<\/body>)/i);
  return at >= 0 ? out.slice(0, at) + script + out.slice(at) : out + script;
}

const escapeAttr = (s: string) => s.replace(/&/g, '&amp;').replace(/"/g, '&quot;');
const escapeHtml = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

/**
 * 新窗口打开用的外壳页：生成内容放进沙箱 iframe（不带 allow-same-origin）。
 *
 * 不能直接打开内容本身的 blob URL —— blob 与控制台同源，页面里的脚本
 * 就能读到 localStorage 里的 API Key、调用管理接口。
 */
export function buildSandboxWrapper(doc: string, title: string): string {
  return `<!doctype html><html><head><meta charset="utf-8"><title>${escapeHtml(title)}</title><style>html,body{margin:0;height:100%;background:#fff}iframe{border:0;width:100%;height:100%;display:block}</style></head><body><iframe sandbox="allow-scripts allow-modals" srcdoc="${escapeAttr(doc)}"></iframe></body></html>`;
}

/** 字节数紧凑显示：820 B / 32.1 KB */
export function formatBytes(n: number): string {
  return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KB`;
}
