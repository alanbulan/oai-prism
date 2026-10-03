const http = require("http");
const fs = require("fs");

const imgPath = "C:/Users/13080/.gemini/antigravity/brain/ed4ebc7a-ece1-4b85-ab3a-5abf84858871/.user_uploaded/media_1791005319419.png";
const imgBytes = fs.readFileSync(imgPath);
const b64 = imgBytes.toString("base64");
const dataUri = `data:image/png;base64,${b64}`;

const payload = JSON.stringify({
  model: "gpt-5.6-sol",
  tools: [
    {
      type: "function",
      function: {
        name: "exec_command",
        description: "Execute a shell command in the local environment",
        parameters: {
          type: "object",
          properties: {
            command: { type: "string" }
          },
          required: ["command"]
        }
      }
    }
  ],
  input: [
    {
      role: "user",
      content: [
        { type: "input_text", text: "请仔细看我上传的图片，里面有什么文字或警告？" },
        { type: "input_image", image_url: dataUri }
      ]
    }
  ]
});

console.log("发起带 tools 的多模态请求...");
const req = http.request("http://127.0.0.1:8787/v1/responses", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(payload),
    "Authorization": "Bearer test"
  }
}, (res) => {
  console.log("Status:", res.statusCode);
  let body = "";
  res.on("data", c => body += c);
  res.on("end", () => {
    try {
      console.log(JSON.stringify(JSON.parse(body), null, 2));
    } catch {
      console.log(body);
    }
  });
});

req.write(payload);
req.end();
