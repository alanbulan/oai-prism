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

(async () => {
  console.log("=== 1. 获取可用 Project 与沙箱 ===");
  const pRes = await getJSON("/api/file-management/projects?section=your_projects");
  const proj = pRes.body.projects[0];
  const projId = proj.id || proj.uuid;
  console.log("Project:", projId, proj.name);

  // 申请沙箱
  console.log("申请沙箱 ...");
  const sbRes = await postJSON("/api/backend/1/new", {});
  const sbUrl = sbRes.body.url;
  const sbTok = sbRes.body.token;
  console.log("沙箱 URL:", sbUrl);

  const convId = "cdx1_" + require("crypto").randomUUID();
  console.log("会话 ID:", convId);

  // ---------- 轮次 1 ----------
  console.log("\n=== 2. 发起第一轮请求 ===");
  const start1 = await postJSON("/api/llm/response_with_tools_start", {
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
    input: [
      {
        role: "system",
        content: [{ type: "input_text", text: "你是一个助手。请遵守指示。" }]
      },
      {
        role: "user",
        content: [{ type: "input_text", text: "请记住：我的名字叫路南，我的秘密暗号是【极光飞羽】。收到请回复：记住。" }]
      }
    ]
  });
  console.log("Start 1 状态:", start1.status, start1.body.status, "reqId:", start1.body.request_id);
  const reqId1 = start1.body.request_id;
  let turnState1 = start1.body.turn_state;

  let completed1 = null;
  if (start1.body && start1.body.status === "completed") {
    completed1 = start1.body;
  } else {
    for (let i = 0; i < 40; i++) {
      await sleep(1500);
      const st = await postJSON("/api/llm/response_with_tools_status", {
        request_id: reqId1,
        turn_state: turnState1,
      });
      if (st.body && st.body.turn_state) turnState1 = st.body.turn_state;
      if (st.body && st.body.status === "completed") {
        completed1 = st.body;
        break;
      }
    }
  }

  if (!completed1) {
    console.error("第一轮超时！");
    process.exit(1);
  }

  console.log("Completed 1 原始结构键:", Object.keys(completed1));
  const payload = completed1.response?.payload || completed1;
  const respId1 = payload.id || completed1.response_id || completed1.initial?.response_id;
  const snap1 = payload.codexListenSnapshot || completed1.initial?.codex_listen_snapshot || completed1.codex_listen_snapshot;
  let text1 = "";
  if (payload.output && payload.output[0]?.content && payload.output[0].content[0]) {
    text1 = payload.output[0].content[0].text;
  } else if (completed1.initial?.text) {
    text1 = completed1.initial.text;
  } else if (payload.text) {
    text1 = payload.text;
  }
  console.log("第一轮完成！");
  console.log("RespId 1:", respId1);
  console.log("Output 1:", text1);
  console.log("Snapshot 1:", typeof snap1 === "string" ? snap1.slice(0, 100) : JSON.stringify(snap1)?.slice(0, 100));

  // ---------- 轮次 2 (增量 input 测试) ----------
  console.log("\n=== 3. 发起第二轮请求 (使用 previousResponseId + 增量 input) ===");
  const start2 = await postJSON("/api/llm/response_with_tools_start", {
    conversationId: convId,
    previousResponseId: respId1,
    model: "gpt-6.1-sol",
    metadata: {
      projectId: projId,
      model: "gpt-6.1-sol",
      reasoning_effort: "low",
      frontend_origin: "https://prism.openai.com",
      sandbox_url: sbUrl,
      sandbox_token: sbTok,
      codex_listen_snapshot: typeof snap1 === "string" ? snap1 : JSON.stringify(snap1),
    },
    input: [
      {
        role: "user",
        content: [{ type: "input_text", text: "请问我的名字和秘密暗号是什么？直接回答。" }]
      }
    ]
  });
  console.log("Start 2 状态:", start2.status, start2.body.status, "reqId:", start2.body.request_id);
  const reqId2 = start2.body.request_id;
  let turnState2 = start2.body.turn_state;

  let completed2 = null;
  if (start2.body && start2.body.status === "completed") {
    completed2 = start2.body;
  } else {
    for (let i = 0; i < 40; i++) {
      await sleep(1500);
      const st = await postJSON("/api/llm/response_with_tools_status", {
        request_id: reqId2,
        turn_state: turnState2,
      });
      if (st.body && st.body.turn_state) turnState2 = st.body.turn_state;
      if (st.body && st.body.status === "completed") {
        completed2 = st.body;
        break;
      }
    }
  }

  if (!completed2) {
    console.error("第二轮超时！");
    process.exit(1);
  }

  const p2 = completed2.response?.payload || completed2;
  let text2 = "";
  if (p2.output && p2.output[0]?.content && p2.output[0].content[0]) {
    text2 = p2.output[0].content[0].text;
  } else if (completed2.initial?.text) {
    text2 = completed2.initial.text;
  } else if (p2.text) {
    text2 = p2.text;
  }
  console.log("第二轮完成！");
  console.log("Output 2:", text2);

  if (text2.includes("路南") && text2.includes("极光飞羽")) {
    console.log("\n🎉🎉🎉 完美验证！原生续接链（previousResponseId + 增量 input）100% 成功接续记忆！");
  } else {
    console.log("\n❌ 未完全匹配:", text2);
  }
})().catch(e => {
  console.error("执行异常:", e);
});
