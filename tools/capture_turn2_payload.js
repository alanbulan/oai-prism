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
  const browser = await chromium.launch({
    headless: true,
    channel: "chrome",
    args: ["--window-size=1600,1000"],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  await ctx.addCookies(parseCookies(cookieStr));

  let turn2PostData = null;
  ctx.on("request", (req) => {
    const u = req.url();
    if (u.includes("response_with_tools_start") && req.method() === "POST") {
      console.log("[REQ] response_with_tools_start 触发！");
      turn2PostData = req.postData();
    }
  });

  const page = await ctx.newPage();
  const projUuid = "e9917e4f-05a4-4c3e-9925-caaee3efc40d";
  await page.goto(`https://prism.openai.com/?u=${projUuid}&pg=1`, { waitUntil: "domcontentloaded" });
  await page.waitForTimeout(8000);

  // 1. 点击【聊天】Tab
  const chatSpan = page.locator('span:text-is("聊天")').first();
  await chatSpan.click();
  await page.waitForTimeout(2000);

  // 2. 点击刚刚生成的会话条目（如果存在）或者点击新聊天
  const existingChat = page.locator('text="这是通过新聊天列表创建的持久会话"').first();
  if (await existingChat.count() > 0 && await existingChat.isVisible()) {
    console.log("[capture] 点击已有持久会话 ...");
    await existingChat.click();
    await page.waitForTimeout(3000);
  }

  // 3. 在当前会话中输入第二轮消息
  console.log("[capture] 发送第二轮消息 ...");
  turn2PostData = null; // 重置
  const box = page.locator("textarea").first();
  await box.fill("请问我上一句话说了什么？");
  await page.keyboard.press("Enter");

  await page.waitForTimeout(15000);

  if (turn2PostData) {
    fs.writeFileSync("tools/turn2_full_post_data.json", turn2PostData);
    console.log("[capture] 第二轮完整请求体已保存至 tools/turn2_full_post_data.json！");
  } else {
    console.log("[capture] 未捕获到 turn2PostData");
  }

  await browser.close();
})().catch(err => {
  console.error(err);
  process.exit(1);
});
