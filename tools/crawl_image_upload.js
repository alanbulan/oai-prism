const PW = "F:/Dev/Repository/npm/global/node_modules/@playwright/mcp/node_modules/playwright-core";
const { chromium } = require(PW);
const fs = require("fs");
const path = require("path");

const accts = JSON.parse(fs.readFileSync("F:/Code/Active/OAIprism/secrets/accounts.json", "utf-8"));
let AT = "";
try {
  const cookies = (accts.accounts || []).map((a) => a.cookies || "").join("; ");
  const m = cookies.match(/prism_oai_access_token=([^;\s]+)/);
  if (m) AT = m[1];
} catch {}

(async () => {
  console.log("[crawl_image] 启动浏览器 ...");
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
  ctx.on("request", (req) => {
    const u = req.url();
    if (u.includes("prism.openai.com/api") || u.includes("prism.openai.com/s/sandboxes")) {
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
      console.log("[REQ]", req.method(), u.replace("https://prism.openai.com", "").slice(0, 100));
    }
  });

  ctx.on("response", async (res) => {
    const u = res.url();
    if (u.includes("prism.openai.com/api") || u.includes("prism.openai.com/s/sandboxes")) {
      let body = "";
      try {
        body = await res.text();
      } catch {}
      apiLogs.push({
        time: new Date().toISOString(),
        type: "RES",
        status: res.status(),
        url: u,
        body: body.slice(0, 5000),
      });
      console.log("[RES]", res.status(), u.replace("https://prism.openai.com", "").slice(0, 100), "len:", body.length);
    }
  });

  const page = await ctx.newPage();
  console.log("[crawl_image] 打开项目工作区 ...");
  const projId = "45e79a6d-eef0-46e9-b757-7a95a7f536a5";
  await page.goto(`https://prism.openai.com/?u=${projId}&pg=1`, { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForTimeout(8000);

  // 点击左侧栏【聊天】Tab
  console.log("[crawl_image] 查找并点击【聊天】Tab ...");
  const chatTab = page.locator('span:text-is("聊天"), button:has-text("聊天")').first();
  if (await chatTab.count() > 0) {
    await chatTab.click();
    await page.waitForTimeout(3000);
  }

  // 点击【新聊天】
  const newChatBtn = page.locator('aside button:has-text("新聊天"), aside [role="button"]:has-text("新聊天"), aside button:has-text("New chat")').first();
  if (await newChatBtn.count() > 0 && await newChatBtn.isVisible()) {
    console.log("[crawl_image] 点击【新聊天】按钮 ...");
    await newChatBtn.click();
    await page.waitForTimeout(4000);
  }

  // 探查页面上的所有 input[type="file"]
  const fileInputs = await page.locator('input[type="file"]').all();
  console.log("[crawl_image] 发现 input[type=file] 数量:", fileInputs.length);

  // 探查输入框周围的按钮
  const buttonsAround = await page.evaluate(() => {
    return Array.from(document.querySelectorAll("button, [role='button'], svg")).map(b => ({
      tag: b.tagName,
      text: (b.innerText || "").trim().slice(0, 30),
      aria: b.getAttribute("aria-label"),
      title: b.getAttribute("title"),
      cls: (b.className || "").toString().slice(0, 40),
    })).filter(x => x.aria || x.title || x.text.includes("上传") || x.text.includes("附加") || x.text.includes("Attach") || x.text.includes("Upload"));
  });
  console.log("[crawl_image] 相关按钮列表:", JSON.stringify(buttonsAround, null, 2));

  // 准备上传的图片
  const imgPath = path.resolve("C:/Users/13080/.gemini/antigravity/brain/ed4ebc7a-ece1-4b85-ab3a-5abf84858871/.user_uploaded/media_1791005319419.png");
  console.log("[crawl_image] 本地图片路径:", imgPath, "是否存在:", fs.existsSync(imgPath));

  let uploaded = false;
  try {
    const plusBtn = page.locator('button[aria-label="上传文件和照片"], button:has-text("+"), button[title="上传文件和照片"]').first();
    if (await plusBtn.count() > 0) {
      console.log("[crawl_image] 点击 + 按钮展开菜单 ...");
      await plusBtn.click();
      await page.waitForTimeout(2000);

      const popupInfo = await page.evaluate(() => {
        const inputs = Array.from(document.querySelectorAll("input")).map(i => ({
          type: i.type,
          id: i.id,
          name: i.name,
          accept: i.accept,
          className: i.className,
          style: i.getAttribute("style"),
          outerHTML: i.outerHTML.slice(0, 200)
        }));
        const menuItems = Array.from(document.querySelectorAll('*')).filter(el => (el.innerText || "").includes("上传并引用项目文件")).map(el => ({
          tag: el.tagName,
          role: el.getAttribute("role"),
          className: el.className,
          outerHTML: el.outerHTML.slice(0, 300)
        }));
        return { inputs, menuItems };
      });
      console.log("[crawl_image] 展开后的 popupInfo:", JSON.stringify(popupInfo, null, 2));

      // 尝试在页面上查找包含“上传文件和照片”的最外层可点击元素
      const targetClickable = page.locator('text=上传并引用项目文件和图像').locator('xpath=ancestor-or-self::*[(self::button or self::div or @role="menuitem") and not(self::body)]').last();
      console.log("[crawl_image] 准备点击目标元素并等待 filechooser ...");
      try {
        const [fileChooser] = await Promise.all([
          page.waitForEvent('filechooser', { timeout: 6000 }),
          targetClickable.click({ force: true })
        ]);
        console.log("[crawl_image] filechooser 事件捕获成功！上传图片...");
        await fileChooser.setFiles(imgPath);
        uploaded = true;
      } catch (err) {
        console.log("[crawl_image] 捕获 filechooser 失败:", err.message);
        // 如果失败，检查是否有刚生成的 input[type=file]
        const latestInputs = await page.locator('input[type="file"]').all();
        console.log("[crawl_image] 当前 input[type=file] 数量:", latestInputs.length);
        if (latestInputs.length > 0) {
          await latestInputs[latestInputs.length - 1].setInputFiles(imgPath);
          uploaded = true;
          console.log("[crawl_image] 兜底向最新 input[type=file] setInputFiles 成功！");
        }
      }
      if (uploaded) {
        console.log("[crawl_image] 上传完成，等待页面渲染附件（10秒）...");
        await page.waitForTimeout(10000);
      }
    } else {
      console.log("[crawl_image] 未找到 + 按钮！");
    }
  } catch (e) {
    console.log("[crawl_image] 文件上传流程异常:", e.message);
  }

  await page.screenshot({ path: "tools/web_after_attach_attempt.png" });

  // 定位聊天输入框
  const textarea = page.locator('textarea, [contenteditable="true"]').first();
  await textarea.fill("请仔细看我上传的图片，里面有什么文字或报错？");
  await page.keyboard.press("Enter");

  console.log("[crawl_image] 正在等待 AI 响应（35 秒）...");
  await page.waitForTimeout(35000);
  await page.screenshot({ path: "tools/web_image_response.png" });

  fs.writeFileSync("tools/web_image_crawl_logs.json", JSON.stringify(apiLogs, null, 2));
  console.log("[crawl_image] 抓包日志已保存至 tools/web_image_crawl_logs.json");

  await page.waitForTimeout(2000);
  await browser.close();
  console.log("[crawl_image] 浏览器已关闭。");
})().catch(err => {
  console.error("执行异常:", err);
  process.exit(1);
});
