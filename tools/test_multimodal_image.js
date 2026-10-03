const fs = require('fs');

async function testMultimodal() {
  console.log("=== 测试 OAIprism 多模态能力 (图片识别) ===");

  // 使用一个红色的 5x5 PNG 图片的 base64
  // 红色方块: 5x5 red dot
  const redDotB64 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAUAAAAFCAYAAACNbyblAAAAHElEQVQI12P4//8/w38GIAXDIBKE0DHxgljNBAAO9TXL0Y4OHwAAAABJRU5ErkJggg==";

  // 同时也准备一张用户本地的真实图片（如果存在）
  const userImgPath = "C:\\Users\\13080\\.gemini\\antigravity\\brain\\ed4ebc7a-ece1-4b85-ab3a-5abf84858871\\.user_uploaded\\media_1791005319419.png";
  let b64ToSend = redDotB64;
  let testDescription = "5x5 纯红色像素图";

  if (fs.existsSync(userImgPath)) {
    const buf = fs.readFileSync(userImgPath);
    b64ToSend = `data:image/png;base64,${buf.toString('base64')}`;
    testDescription = "真实的终端报错截图 (media_1791005319419.png)";
  }

  console.log(`准备发送图片: ${testDescription}, base64 长度: ${b64ToSend.length}`);

  const payload = {
    model: "gpt-6.1-sol",
    stream: false,
    input: [
      {
        role: "user",
        content: [
          {
            type: "input_text",
            text: "请仔细查看这张图片。请详细描述图片中的主要内容、文字或颜色是什么？"
          },
          {
            type: "input_image",
            image_url: b64ToSend
          }
        ]
      }
    ]
  };

  console.log("向 http://127.0.0.1:8787/v1/responses 发起请求...");
  const t0 = Date.now();
  const res = await fetch("http://127.0.0.1:8787/v1/responses", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "user-agent": "codex-cli/0.160.0"
    },
    body: JSON.stringify(payload)
  });

  const cost = ((Date.now() - t0) / 1000).toFixed(2);
  console.log(`HTTP 状态码: ${res.status}, 耗时: ${cost}s`);

  const data = await res.json();
  console.log("响应数据:", JSON.stringify(data, null, 2));
}

testMultimodal().catch(err => {
  console.error("测试出错:", err);
  process.exit(1);
});
