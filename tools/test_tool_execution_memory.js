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
  let toolCall = null;
  let buf = "";

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const lines = buf.split("\n");
    buf = lines.pop();

    for (const line of lines) {
      if (line.startsWith("data:")) {
        const raw = line.slice(5).trim();
        if (raw === "[DONE]") continue;
        try {
          const ev = JSON.parse(raw);
          if (ev.type === "response.output_item.added" || ev.type === "response.output_item.done") {
            if (ev.item && (ev.item.type === "custom_tool_call" || ev.item.type === "function_call")) {
              toolCall = ev.item;
            }
          }
          if (ev.type === "response.output_text.done" && ev.text) {
            fullText = ev.text;
          } else if (ev.text) {
            fullText = ev.text;
          }
        } catch {}
      }
    }
  }
  return { fullText, toolCall };
}

async function run() {
  console.log("=== 验证工具调用多轮记忆链路 (执行命令 -> 回传结果 -> 追问操作) ===");
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

  // 轮 1: 要求执行命令
  console.log("\n--- [轮 1] 要求在本地执行命令创建 demo_prism.txt ---");
  const p1 = {
    model: process.env.OAIPRISM_MODEL, // 不设 = 网关的默认模型（跟随上游在售清单）
    stream: true,
    tools: tools,
    input: [
      { role: "user", content: "请在当前目录帮我创建一个名为 demo_prism.txt 的文件，内容写入【PrismMemoryOK】。请直接给出执行命令。" }
    ]
  };

  const res1 = await fetchSSE("http://127.0.0.1:8787/v1/responses", p1, headers);
  console.log("轮 1 文本:", res1.fullText);
  console.log("轮 1 工具调用:", JSON.stringify(res1.toolCall));

  if (!res1.toolCall) {
    throw new Error("轮 1 未能触发工具调用！");
  }

  // 轮 2: 模拟客户端执行成功并回传结果，追问刚创建的文件名和内容
  console.log("\n--- [轮 2] 回传执行成功结果并追问操作详情 ---");
  const input2 = [
    { role: "user", content: "请在当前目录帮我创建一个名为 demo_prism.txt 的文件，内容写入【PrismMemoryOK】。请直接给出执行命令。" },
    res1.toolCall,
    {
      type: "custom_tool_call_output",
      call_id: res1.toolCall.call_id || res1.toolCall.id,
      name: "exec_command",
      output: "exited successfully with no output (code 0)"
    },
    { role: "user", content: "你刚才为我创建的文件叫什么名字？里面写入了什么内容？直接回答。" }
  ];

  const p2 = {
    model: process.env.OAIPRISM_MODEL, // 不设 = 网关的默认模型（跟随上游在售清单）
    stream: true,
    tools: tools,
    input: input2
  };

  const res2 = await fetchSSE("http://127.0.0.1:8787/v1/responses", p2, headers);
  console.log("轮 2 文本回答:", res2.fullText);

  if (res2.fullText.includes("demo_prism.txt") && res2.fullText.includes("PrismMemoryOK")) {
    console.log("\n🎉🎉🎉 工具执行多轮记忆端到端验证完全成功！！！");
  } else {
    console.log("\n❌ 失败！输出未包含文件名或内容:", res2.fullText);
    process.exit(1);
  }
}

run().catch(e => {
  console.error("执行异常:", e);
  process.exit(1);
});
