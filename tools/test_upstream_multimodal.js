const http = require("http");
const fs = require("fs");
const crypto = require("crypto");

async function main() {
  const imgPath = "C:/Users/13080/.gemini/antigravity/brain/ed4ebc7a-ece1-4b85-ab3a-5abf84858871/.user_uploaded/media_1791005319419.png";
  const imgBytes = fs.readFileSync(imgPath);
  const projectId = "45e79a6d-eef0-46e9-b757-7a95a7f536a5";
  const fileId = crypto.randomUUID();
  const filename = "media_" + Date.now() + ".png";

  console.log("1. 上传文件到项目...");
  const uploadRes = await new Promise((resolve, reject) => {
    const req = http.request("http://127.0.0.1:8790/api/project-files/upload", {
      method: "POST",
      headers: {
        "content-type": "image/png",
        "x-prism-file-name": filename,
        "x-prism-file-id": fileId,
        "x-prism-file-size": imgBytes.length.toString(),
        "x-prism-project-id": projectId,
        "x-prism-require-project-edit-access": "true",
        "Content-Length": imgBytes.length
      }
    }, (res) => {
      let b = "";
      res.on("data", c => b += c);
      res.on("end", () => resolve({ status: res.statusCode, body: b }));
    });
    req.on("error", reject);
    req.write(imgBytes);
    req.end();
  });

  console.log("Upload Status:", uploadRes.status, "Body:", uploadRes.body);
  if (uploadRes.status !== 200) return;

  const convId = "cdx1_" + crypto.randomUUID();
  console.log("2. 发起 response_with_tools_start ...");
  const startPayload = {
    input: [
      {
        type: "message",
        role: "user",
        content: [
          { type: "input_text", text: "请仔细看我上传的图片，里面有什么文字或警告？" },
          { type: "input_file", filename: filename, project_path: "/prism-uploads/" + filename }
        ]
      }
    ],
    metadata: {
      projectId: projectId,
      model: "gpt-5.6-sol",
      reasoning_effort: "medium"
    },
    conversationId: convId
  };

  const startRes = await new Promise((resolve, reject) => {
    const data = JSON.stringify(startPayload);
    const req = http.request("http://127.0.0.1:8790/api/llm/response_with_tools_start", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "Content-Length": Buffer.byteLength(data)
      }
    }, (res) => {
      let b = "";
      res.on("data", c => b += c);
      res.on("end", () => resolve({ status: res.statusCode, body: b }));
    });
    req.on("error", reject);
    req.write(data);
    req.end();
  });

  console.log("Start Status:", startRes.status, "Body:", startRes.body.slice(0, 300));
  const startJson = JSON.parse(startRes.body);
  const requestId = startJson.request_id;
  let turnState = startJson.turn_state;

  console.log("3. 轮询 response_with_tools_status ...");
  for (let i = 0; i < 30; i++) {
    await new Promise(r => setTimeout(r, 2000));
    const statusPayload = {
      request_id: requestId,
      turn_state: turnState
    };
    const sRes = await new Promise((resolve, reject) => {
      const data = JSON.stringify(statusPayload);
      const req = http.request("http://127.0.0.1:8790/api/llm/response_with_tools_status", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "Content-Length": Buffer.byteLength(data)
        }
      }, (res) => {
        let b = "";
        res.on("data", c => b += c);
        res.on("end", () => resolve({ status: res.statusCode, body: b }));
      });
      req.on("error", reject);
      req.write(data);
      req.end();
    });

    const sJson = JSON.parse(sRes.body);
    console.log(`Poll [${i}] status=${sJson.status}`);
    if (sJson.turn_state) turnState = sJson.turn_state;
    if (sJson.status === "completed") {
      console.log("=== COMPLETED ===");
      console.log(JSON.stringify(sJson.response ? sJson.response.payload : sJson, null, 2));
      break;
    }
  }
}

main().catch(console.error);
