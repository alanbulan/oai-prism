const http = require("http");
const fs = require("fs");

async function main() {
  const projectId = "45e79a6d-eef0-46e9-b757-7a95a7f536a5";
  const filename = "media_1791005319419.png";
  const convId = "cdx1_aa331c70-ad47-4aa3-b15c-5e1f06b1ef4c";

  // 取上一次抓包里沙箱的 token
  const logs = JSON.parse(fs.readFileSync("tools/web_image_crawl_logs.json", "utf8"));
  const startLog = logs.find(x => x.url && x.url.includes("response_with_tools_start") && x.type === "REQ");
  const meta = JSON.parse(startLog.postData).metadata;

  const payload = {
    input: [
      {
        type: "message",
        role: "user",
        content: [
          { type: "input_text", text: "这张图片里的主要内容是什么？请详细说出其中的文字" },
          { type: "input_file", filename: filename, project_path: "/prism-uploads/" + filename }
        ]
      }
    ],
    metadata: meta,
    conversationId: convId
  };

  console.log("发起 start 请求...");
  const data = JSON.stringify(payload);
  const startRes = await new Promise((resolve, reject) => {
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

  console.log("Start Status:", startRes.status);
  const startJson = JSON.parse(startRes.body);
  const requestId = startJson.request_id;
  let turnState = startJson.turn_state;

  console.log("开始轮询 status ...");
  for (let i = 0; i < 20; i++) {
    await new Promise(r => setTimeout(r, 2000));
    const sRes = await new Promise((resolve, reject) => {
      const d = JSON.stringify({ request_id: requestId, turn_state: turnState });
      const req = http.request("http://127.0.0.1:8790/api/llm/response_with_tools_status", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          "Content-Length": Buffer.byteLength(d)
        }
      }, (res) => {
        let b = "";
        res.on("data", c => b += c);
        res.on("end", () => resolve({ status: res.statusCode, body: b }));
      });
      req.on("error", reject);
      req.write(d);
      req.end();
    });

    const sJson = JSON.parse(sRes.body);
    console.log(`Poll [${i}] status=${sJson.status}`);
    if (sJson.turn_state) turnState = sJson.turn_state;
    if (sJson.status === "completed") {
      console.log("=== COMPLETED ===");
      console.log(JSON.stringify(sJson.response ? sJson.response.payload.output : sJson, null, 2));
      break;
    }
  }
}

main().catch(console.error);
