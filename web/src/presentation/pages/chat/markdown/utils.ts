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
  function measure(){
    var d=document.documentElement,b=document.body;
    var w=Math.ceil(Math.max(d.scrollWidth,b?b.scrollWidth:0));
    var h=d.getBoundingClientRect().height;
    if(b){var bs=getComputedStyle(b);h=Math.max(h,b.scrollHeight+parseFloat(bs.marginTop)+parseFloat(bs.marginBottom))}
    h=Math.ceil(h);
    var crop=null,transparent=false;
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
          if(!cleared){
            cleared=true;
            for(var t=0;t<stages.length;t++)stages[t].style.setProperty("background","transparent","important");
          }
        }
      }
    }
    var msg={oaiprismPreview:id,width:w,height:h,crop:crop,transparent:transparent};
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
