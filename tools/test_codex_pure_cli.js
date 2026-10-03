async function fetchSSE(url, payload, headers) {
  const res = await fetch(url, {
    method: "POST",
    headers: headers,
    body: JSON.stringify(payload)
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`HTTP ${res.status}: ${text}`);
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let fullText = "";
  let buf = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const lines = buf.split("\n");
    buf = lines.pop(); // 保留未完整的末行

    for (const line of lines) {
      if (line.startsWith("data:")) {
        const raw = line.slice(5).trim();
        console.log("  [SSE DATA]", raw);
        if (raw === "[DONE]") continue;
        try {
          const ev = JSON.parse(raw);
          if (ev.type === "response.output_text.delta" && ev.delta) {
            fullText += ev.delta;
          } else if (ev.type === "response.output_text.done" && ev.text) {
            fullText = ev.text;
          } else if (ev.text) {
            fullText = ev.text;
          }
        } catch {}
      }
    }
  }
  return fullText;
}

async function run() {
  console.log("=== 启动端到端纯真实 Codex CLI 仿真测试 (stream: true + tools 桥模式) ===");

  const tools = [
    {
      type: "function",
      function: {
        name: "exec_command",
        description: "Runs a command in the user workspace",
        parameters: {
          type: "object",
          properties: { cmd: { type: "string" } },
          required: ["cmd"]
        }
      }
    }
  ];

  const headers = {
    "content-type": "application/json",
    "user-agent": "codex-cli/0.160.0 (Windows NT 10.0; Win64; x64)"
  };

  // 轮 1
  console.log("\n--- [轮 1] 注入身份与暗号 ---");
  const p1 = {
    model: "gpt-6.1-sol",
    stream: true,
    tools: tools,
    input: [
      { role: "user", content: "请记住：我的名字叫路南，我的秘密暗号是【极光飞羽】。收到请回复记住。" }
    ]
  };

  const text1 = await fetchSSE("http://127.0.0.1:8787/v1/responses", p1, headers);
  console.log("轮 1 响应:", text1);

  // 轮 2（全量回传前序问答，完全模拟真实 Codex CLI）
  console.log("\n--- [轮 2] 追问名字与暗号（全量回传 input，不传 previousResponseId） ---");
  const p2 = {
    model: "gpt-6.1-sol",
    stream: true,
    tools: tools,
    input: [
      { role: "user", content: "请记住：我的名字叫路南，我的秘密暗号是【极光飞羽】。收到请回复记住。" },
      { role: "assistant", content: text1 || "记住。" },
      { role: "user", content: "请问我的名字和秘密暗号是什么？直接回答。" }
    ]
  };

  const text2 = await fetchSSE("http://127.0.0.1:8787/v1/responses", p2, headers);
  console.log("轮 2 响应:", text2);

  if (text2.includes("路南") && text2.includes("极光飞羽")) {
    console.log("\n🎉🎉🎉 成功！模型精准回答了名字与暗号，多轮记忆完全恢复！");
  } else {
    console.log("\n❌ 失败！模型未正确回忆出上一轮内容，输出为:", text2);
    process.exit(1);
  }
}

run().catch(e => {
  console.error("执行异常:", e);
  process.exit(1);
});
