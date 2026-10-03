const http = require("http");
const fs = require("fs");

const accts = JSON.parse(fs.readFileSync("F:/Code/Active/OAIprism/secrets/accounts.json", "utf-8"));
const cookies = (accts.accounts || []).map((a) => a.cookies || "").join("; ");

function postJSON(path, data) {
  return new Promise((resolve, reject) => {
    const postData = JSON.stringify(data);
    const req = http.request("http://127.0.0.1:8790" + path, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Content-Length": Buffer.byteLength(postData),
        "Cookie": cookies,
      }
    }, (res) => {
      let buf = "";
      res.on("data", c => buf += c);
      res.on("end", () => {
        try {
          resolve({ status: res.statusCode, body: JSON.parse(buf) });
        } catch {
          resolve({ status: res.statusCode, text: buf });
        }
      });
    });
    req.on("error", reject);
    req.write(postData);
    req.end();
  });
}

function getJSON(path) {
  return new Promise((resolve, reject) => {
    const req = http.request("http://127.0.0.1:8790" + path, {
      method: "GET",
      headers: { "Cookie": cookies }
    }, (res) => {
      let buf = "";
      res.on("data", c => buf += c);
      res.on("end", () => {
        try {
          resolve({ status: res.statusCode, body: JSON.parse(buf) });
        } catch {
          resolve({ status: res.statusCode, text: buf });
        }
      });
    });
    req.on("error", reject);
    req.end();
  });
}

async function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

async function runPrompt(convId, projId, sbUrl, sbTok, prevRespId, snapshot, inputItems) {
  const reqBody = {
    conversationId: convId,
    model: "gpt-6.1-sol",
    metadata: {
      projectId: projId,
      model: "gpt-6.1-sol",
      reasoning_effort: "low",
      frontend_origin: "https://prism.openai.com",
      sandbox_url: sbUrl,
      sandbox_token: sbTok,
    },
    input: inputItems,
  };
  if (prevRespId) reqBody.previousResponseId = prevRespId;
  if (snapshot) reqBody.metadata.codex_listen_snapshot = typeof snapshot === "string" ? snapshot : JSON.stringify(snapshot);

  const start = await postJSON("/api/llm/response_with_tools_start", reqBody);
  console.log("Start 返回:", start.status, start.body ? start.body.status : start.text);
  if (!start.body || !start.body.request_id) {
    throw new Error("Start failed: " + JSON.stringify(start));
  }

  let completed = null;
  if (start.body.status === "completed") {
    completed = start.body;
  } else {
    let reqId = start.body.request_id;
    let turnState = start.body.turn_state;
    for (let i = 0; i < 40; i++) {
      await sleep(1500);
      const st = await postJSON("/api/llm/response_with_tools_status", {
        request_id: reqId,
        turn_state: turnState,
      });
      if (st.body && st.body.turn_state) turnState = st.body.turn_state;
      if (st.body && st.body.status === "completed") {
        completed = st.body;
        break;
      }
    }
  }

  if (!completed) throw new Error("Status polling timed out");
  const payload = completed.response ? completed.response.payload : completed.payload;
  const respId = payload ? payload.id : null;
  const snap = payload ? (payload.codexListenSnapshot || payload.codex_listen_snapshot) : null;
  const text = payload && payload.output && payload.output[0] && payload.output[0].content && payload.output[0].content[0] ? payload.output[0].content[0].text : "";
  return { respId, snap, text };
}

(async () => {
  console.log("=== 初始化 Project 与沙箱凭据 ===");
  const pRes = await getJSON("/api/file-management/projects?section=your_projects");
  const projId = pRes.body.projects[0].id || pRes.body.projects[0].uuid;

  // 申请并获取已有沙箱
  const sbRes = await postJSON("/api/backend/1/new", {});
  const sbUrl = sbRes.body.url;
  const sbTok = sbRes.body.token;

  // 必须走完资源令牌同步
  const rtRes = await postJSON(`/api/projects/${projId}/sandbox/resources-token`, {});
  const yRes = await postJSON("/api/y", {});

  const convId = "cdx1_" + require("crypto").randomUUID();
  console.log("项目:", projId, "会话:", convId);

  console.log("\n--- Turn 1 ---");
  const t1 = await runPrompt(convId, projId, sbUrl, sbTok, null, null, [
    { role: "system", content: [{ type: "input_text", text: "你是一个助手。" }] },
    { role: "user", content: [{ type: "input_text", text: "请记住：我的名字叫路南，我的秘密暗号是【极光飞羽】。收到请回复：记住。" }] }
  ]);
  console.log("Turn 1 响应:", t1.text, "RespId:", t1.respId);

  console.log("\n--- Turn 2 (测试增量 input) ---");
  const t2 = await runPrompt(convId, projId, sbUrl, sbTok, t1.respId, t1.snap, [
    { role: "user", content: [{ type: "input_text", text: "请问我的名字和秘密暗号是什么？直接回答。" }] }
  ]);
  console.log("Turn 2 响应:", t2.text);

  if (t2.text.includes("路南") && t2.text.includes("极光飞羽")) {
    console.log("\n🎉🎉🎉 测试完全成功！原生链路完美记忆！");
  } else {
    console.log("\n❌ 测试失败，未能恢复记忆！");
  }
})().catch(e => console.error("Error:", e));
