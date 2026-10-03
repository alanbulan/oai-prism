const http = require("http");
const fs = require("fs");
const path = require("path");

const imgPath = "C:/Users/13080/.gemini/antigravity/brain/ed4ebc7a-ece1-4b85-ab3a-5abf84858871/.user_uploaded/media_1791005319419.png";
const imgBytes = fs.readFileSync(imgPath);
const b64 = imgBytes.toString("base64");
const dataUri = `data:image/png;base64,${b64}`;

const payload = JSON.stringify({
  model: "gpt-5.6-sol",
  input: [
    {
      role: "user",
      content: [
        { type: "input_text", text: "请仔细看我上传的图片，提取图片里的核心文字" },
        { type: "input_image", image_url: dataUri }
      ]
    }
  ]
});

console.log("[test_multimodal] 发起请求，图片大小:", imgBytes.length, "字节...");
const req = http.request("http://127.0.0.1:8787/v1/responses", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(payload),
    "Authorization": "Bearer test"
  }
}, (res) => {
  console.log("[test_multimodal] HTTP 状态码:", res.statusCode);
  let body = "";
  res.on("data", chunk => {
    body += chunk;
  });
  res.on("end", () => {
    console.log("[test_multimodal] 响应完成，响应体:");
    try {
      const parsed = JSON.parse(body);
      console.log(JSON.stringify(parsed, null, 2));
    } catch {
      console.log(body);
    }
  });
});

req.on("error", (err) => {
  console.error("[test_multimodal] 请求异常:", err);
});

req.write(payload);
req.end();
