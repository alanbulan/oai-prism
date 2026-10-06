const fs = require("fs");

async function main() {
  const sessionId = "session_bridge_test_" + Date.now();
  console.log("=== 测试开始，SessionId:", sessionId);

  // 第一轮：带 tools 走桥模式
  console.log("\n--- [轮 1] 发送名字和秘密暗号 ---");
  const p1 = {
    model: process.env.OAIPRISM_MODEL, // 不设 = 网关的默认模型（跟随上游在售清单）
    tools: [
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
    ],
    input: [
      { role: "user", content: "我叫极光特工，我的专属暗号是【雪顶冰咖啡】。请直接回复收到即可，不需要执行任何命令。" }
    ]
  };

  const r1 = await (await fetch("http://127.0.0.1:8787/v1/responses", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-oaiprism-session": sessionId
    },
    body: JSON.stringify(p1)
  })).json();

  console.log("轮 1 响应:", JSON.stringify(r1, null, 2));

  // 第二轮：多轮回传，询问上一轮内容
  console.log("\n--- [轮 2] 询问名字与专属暗号 ---");
  const p2 = {
    model: process.env.OAIPRISM_MODEL, // 不设 = 网关的默认模型（跟随上游在售清单）
    tools: p1.tools,
    input: [
      { role: "user", content: "我叫极光特工，我的专属暗号是【雪顶冰咖啡】。请直接回复收到即可，不需要执行任何命令。" },
      { role: "assistant", content: r1.output ? r1.output[0].content[0].text : "收到。" },
      { role: "user", content: "请问我的名字和专属暗号是什么？只回复名字和暗号本身，不需要执行命令。" }
    ]
  };

  const r2 = await (await fetch("http://127.0.0.1:8787/v1/responses", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-oaiprism-session": sessionId
    },
    body: JSON.stringify(p2)
  })).json();

  console.log("轮 2 响应:", JSON.stringify(r2, null, 2));

  const text2 = r2.output && r2.output[0] && r2.output[0].content[0] ? r2.output[0].content[0].text : "";
  console.log("\n=== 最终模型回答 ===");
  console.log(text2);

  if (text2.includes("极光特工") && text2.includes("雪顶冰咖啡")) {
    console.log("\n>>> 测试 100% 成功！多轮记忆完全恢复，精准回答出名字与暗号！<<<");
  } else {
    console.log("\n>>> 请检查输出内容 <<<");
  }
}

main().catch(console.error);
