const { chromium } = require("playwright");
const fs = require("fs");

(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ storageState: "secrets/playwright_storage_state.json" });
  const page = await context.newPage();
  await page.goto("https://prism.openai.com/p/45e79a6d-eef0-46e9-b757-7a95a7f536a5", { waitUntil: "networkidle" });
  
  const chatTab = page.locator('button:has-text("聊天"), [role="tab"]:has-text("聊天")').first();
  if (await chatTab.count() > 0) await chatTab.click();
  await page.waitForTimeout(3000);

  const plusBtn = page.locator('button[aria-label="上传文件和照片"], button:has-text("+")').first();
  await plusBtn.click();
  await page.waitForTimeout(1000);

  const elements = await page.evaluate(() => {
    // 找出所有可见的浮层元素
    return Array.from(document.querySelectorAll("*")).filter(el => {
      const text = el.innerText || "";
      return text.includes("上传文件和照片") || text.includes("上传并引用");
    }).map(el => ({
      tag: el.tagName,
      role: el.getAttribute("role"),
      className: el.className,
      text: el.innerText.trim(),
      html: el.outerHTML.slice(0, 300)
    }));
  });

  console.log("Found popup elements:", JSON.stringify(elements, null, 2));
  await browser.close();
})();
