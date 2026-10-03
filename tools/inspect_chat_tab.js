const PW = "F:/Dev/Repository/npm/global/node_modules/@playwright/mcp/node_modules/playwright-core";
const { chromium } = require(PW);
const fs = require("fs");

const accts = JSON.parse(fs.readFileSync("F:/Code/Active/OAIprism/secrets/accounts.json", "utf-8"));
const cookieStr = (accts.accounts || []).map((a) => a.cookies || "").join("; ");

function parseCookies(str) {
  const out = [];
  for (const pair of str.split("; ")) {
    const eq = pair.indexOf("=");
    if (eq < 0) continue;
    out.push({
      name: pair.slice(0, eq),
      value: pair.slice(eq + 1),
      domain: "prism.openai.com",
      path: "/",
    });
  }
  return out;
}

(async () => {
  console.log("[chat-tab] 启动浏览器 ...");
  const browser = await chromium.launch({
    headless: true,
    channel: "chrome",
    args: ["--window-size=1600,1000"],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  await ctx.addCookies(parseCookies(cookieStr));

  const allReqs = [];
  ctx.on("request", (req) => {
    const u = req.url();
    if (u.includes("prism.openai.com/api")) {
      const item = { method: req.method(), url: u.replace("https://prism.openai.com", "") };
      if (req.method() === "POST") {
        try {
          const pd = req.postData();
          if (pd) item.data = pd;
        } catch {}
      }
      allReqs.push(item);
      console.log(`[REQ] ${item.method} ${item.url}`);
    }
  });

  ctx.on("response", async (res) => {
    const u = res.url();
    if (u.includes("prism.openai.com/api") && (u.includes("conversation") || u.includes("session") || u.includes("chat") || u.includes("project"))) {
      try {
        const body = await res.text();
        console.log(`[RES ${res.status()}] ${u.replace("https://prism.openai.com", "")} -> ${body.slice(0, 300)}`);
      } catch {}
    }
  });

  const page = await ctx.newPage();
  const projUuid = "e9917e4f-05a4-4c3e-9925-caaee3efc40d";
  console.log("[chat-tab] 打开项目 ...");
  await page.goto(`https://prism.openai.com/?u=${projUuid}&pg=1`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(8000);

  // 定位左侧栏顶部【文件】旁边的【聊天】按钮
  console.log("[chat-tab] 点击用户红框标出的【聊天】Tab ...");
  // 查找文本包含"聊天"且不是"新聊天"的元素
  const chatTab = page.locator('div:has-text("文件") button:has-text("聊天"), aside button:has-text("聊天"), nav button:has-text("聊天"), button:text-is("聊天")').first();
  await chatTab.click();
  await page.waitForTimeout(3000);

  await page.screenshot({ path: "tools/explore_chat_tab_opened.png" });
  console.log("[chat-tab] 已截图 tools/explore_chat_tab_opened.png");

  // 查看切换到【聊天】Tab 后的 DOM 元素
  const items = await page.evaluate(() => {
    const res = [];
    document.querySelectorAll("aside button, aside a, aside div[role='button'], [class*='sidebar'] button, [class*='sidebar'] div").forEach(el => {
      const t = (el.innerText || "").trim();
      if (t && t.length < 100) res.push({ tag: el.tagName, text: t, aria: el.getAttribute("aria-label"), cls: (el.className||"").slice(0, 40) });
    });
    return res;
  });
  console.log("[chat-tab] 侧边栏元素:", JSON.stringify(items.slice(0, 20), null, 2));

  // 查找“新聊天”按钮并点击
  const newChatBtn = page.locator('button:has-text("新聊天"), [aria-label*="新聊天"], div:has-text("新聊天")').first();
  if (await newChatBtn.count() > 0 && await newChatBtn.isVisible()) {
    console.log("[chat-tab] 找到【新聊天】按钮，点击创建新会话 ...");
    await newChatBtn.click();
    await page.waitForTimeout(4000);
    await page.screenshot({ path: "tools/explore_new_chat_clicked.png" });
    console.log("[chat-tab] 已截图 tools/explore_new_chat_clicked.png");
  } else {
    console.log("[chat-tab] 未直接找到可见的【新聊天】按钮，查找其他新增按钮");
  }

  // 再次提取侧边栏，看是否出现了会话列表条目
  const conversations = await page.evaluate(() => {
    return Array.from(document.querySelectorAll("aside [role='listitem'], aside li, aside a, aside [class*='item']")).map(e => ({
      text: (e.innerText || "").trim().slice(0, 80),
      href: e.getAttribute("href"),
      aria: e.getAttribute("aria-label"),
    })).filter(x => x.text);
  });
  console.log("[chat-tab] 会话列表条目:", JSON.stringify(conversations, null, 2));

  fs.writeFileSync("tools/explore_chat_tab_all.json", JSON.stringify({ allReqs, items, conversations }, null, 2));

  await browser.close();
  console.log("[chat-tab] 完成。");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
