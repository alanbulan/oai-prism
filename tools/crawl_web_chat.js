const PW = "F:/Dev/Repository/npm/global/node_modules/@playwright/mcp/node_modules/playwright-core";
const { chromium } = require(PW);
const fs = require("fs");

const accts = JSON.parse(fs.readFileSync("F:/Code/Active/OAIprism/secrets/accounts.json", "utf-8"));
let AT = "";
try {
  const cookies = (accts.accounts || []).map((a) => a.cookies || "").join("; ");
  const m = cookies.match(/prism_oai_access_token=([^;\s]+)/);
  if (m) AT = m[1];
} catch {}

(async () => {
  console.log("[crawl] 启动浏览器 ...");
  const browser = await chromium.launch({
    headless: false,
    channel: "chrome",
    args: ["--window-size=1600,1000", "--window-position=50,50"],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  if (AT) {
    await ctx.addCookies([{ name: "prism_oai_access_token", value: AT, domain: "prism.openai.com", path: "/" }]);
  }

  const apiLogs = [];
  let capturedProjects = null;

  ctx.on("request", (req) => {
    const u = req.url();
    if (u.includes("prism.openai.com/api")) {
      const entry = {
        time: new Date().toISOString(),
        type: "REQ",
        method: req.method(),
        url: u,
        headers: req.headers(),
      };
      if (req.method() === "POST" || req.method() === "PUT") {
        try {
          entry.postData = req.postData();
        } catch {}
      }
      apiLogs.push(entry);
      console.log("[REQ]", req.method(), u.replace("https://prism.openai.com", ""));
    }
  });

  ctx.on("response", async (res) => {
    const u = res.url();
    if (u.includes("prism.openai.com/api")) {
      let body = "";
      try {
        body = await res.text();
      } catch {}
      if (u.includes("projects?section=your_projects")) {
        try {
          capturedProjects = JSON.parse(body);
        } catch {}
      }
      apiLogs.push({
        time: new Date().toISOString(),
        type: "RES",
        status: res.status(),
        url: u,
        body: body.slice(0, 10000),
      });
      console.log("[RES]", res.status(), u.replace("https://prism.openai.com", ""), "len:", body.length);
    }
  });

  const page = await ctx.newPage();
  console.log("[crawl] 打开主页以获取项目列表 ...");
  await page.goto("https://prism.openai.com/", { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(6000);

  let projId = "";
  if (capturedProjects && capturedProjects.projects && capturedProjects.projects.length > 0) {
    projId = capturedProjects.projects[0].id || capturedProjects.projects[0].uuid;
    console.log("[crawl] 获取到第一个项目 ID:", projId, "名称:", capturedProjects.projects[0].name);
  }

  if (!projId) {
    // 降级使用之前已知的可用 project
    projId = "45e79a6d-eef0-46e9-b757-7a95a7f536a5";
  }

  console.log("[crawl] 直接导航进入项目工作区:", projId);
  await page.goto(`https://prism.openai.com/?u=${projId}&pg=1`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(8000);
  await page.screenshot({ path: "tools/web_project_workspace.png" });

  // 1. 查找左侧栏顶部【聊天】Tab
  console.log("[crawl] 查找并点击左侧栏【聊天】Tab ...");
  const chatTab = page.locator('span:text-is("聊天"), button:has-text("聊天")').first();
  await chatTab.click();
  await page.waitForTimeout(3000);
  await page.screenshot({ path: "tools/web_chat_tab_opened.png" });

  // 2. 检查聊天列表区域中的全部子元素
  const chatListItems = await page.evaluate(() => {
    const aside = document.querySelector("aside");
    if (!aside) return [];
    return Array.from(aside.querySelectorAll("*")).map(el => ({
      tag: el.tagName,
      text: (el.innerText || "").trim().slice(0, 80),
      aria: el.getAttribute("aria-label"),
      role: el.getAttribute("role"),
      cls: (el.className || "").toString().slice(0, 50),
    })).filter(x => x.text && x.text.length > 0);
  });
  console.log("[crawl] 聊天列表内元素概览:", JSON.stringify(chatListItems.slice(0, 30), null, 2));

  // 3. 点击【+ 新聊天】或列表中的某个会话项
  const newChatBtn = page.locator('aside button:has-text("新聊天"), aside [role="button"]:has-text("新聊天"), aside button:has-text("New chat")').first();
  if (await newChatBtn.count() > 0 && await newChatBtn.isVisible()) {
    console.log("[crawl] 点击【新聊天】按钮 ...");
    await newChatBtn.click();
    await page.waitForTimeout(4000);
    await page.screenshot({ path: "tools/web_new_chat_clicked.png" });
  } else {
    console.log("[crawl] 未见新聊天按钮，寻找会话列表条目 ...");
    const chatEntry = page.locator('aside li, aside div[role="listitem"], aside div[tabindex]').filter({ hasNotText: "聊天" }).first();
    if (await chatEntry.count() > 0) {
      console.log("[crawl] 点击会话列表项:", (await chatEntry.innerText()).slice(0, 40));
      await chatEntry.click();
      await page.waitForTimeout(4000);
      await page.screenshot({ path: "tools/web_chat_item_clicked.png" });
    }
  }

  console.log("[crawl] 点击后当前 URL:", page.url());

  // 4. 发送第一轮提问
  console.log("[crawl] 定位输入框发送第一轮提问 ...");
  const textarea = page.locator('textarea, [contenteditable="true"]').first();
  await textarea.fill("第一轮测试：我的名字是路南。收到请回复：收到。");
  await page.keyboard.press("Enter");

  console.log("[crawl] 等待第一轮回复完成（25秒）...");
  await page.waitForTimeout(25000);
  await page.screenshot({ path: "tools/web_turn1_done.png" });

  // 5. 发送第二轮提问
  console.log("[crawl] 定位输入框发送第二轮提问 ...");
  const textarea2 = page.locator('textarea, [contenteditable="true"]').first();
  await textarea2.fill("第二轮测试：我刚才说我的名字是什么？请直接回答。");
  await page.keyboard.press("Enter");

  console.log("[crawl] 等待第二轮回复完成（25秒）...");
  await page.waitForTimeout(25000);
  await page.screenshot({ path: "tools/web_turn2_done.png" });

  fs.writeFileSync("tools/web_full_crawl_logs.json", JSON.stringify(apiLogs, null, 2));
  console.log("[crawl] 全部日志已保存到 tools/web_full_crawl_logs.json");

  await page.waitForTimeout(3000);
  await browser.close();
  console.log("[crawl] 完成退出。");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
