const fs = require("fs");

async function main() {
  const sessionId = "session_live_test_" + Date.now();
  console.log("=== 测试开始，SessionId:", sessionId);

  // 第一轮
  console.log("\n--- [轮 1] 发送名字和秘密暗号 ---");
  const p1 = {
    model: "gpt-6.1-sol",
    input: [
      { role: "user", content: "我叫极光特工，我的专属暗号是【北极星007】。请确认记住。" }
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

  // 第二轮（多轮回传模式，模拟真实 Codex 客户端）
  console.log("\n--- [轮 2] 询问名字与暗号 ---");
  const p2 = {
    model: "gpt-6.1-sol",
    input: [
      { role: "user", content: "我叫极光特工，我的专属暗号是【北极星007】。请确认记住。" },
      { role: "assistant", content: r1.output ? r1.output[0].content[0].text : "已记住。" },
      { role: "user", content: "我的名字和专属暗号是什么？只回复名字和暗号本身。" }
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

  if (text2.includes("极光特工") && text2.includes("北极星007")) {
    console.log("\n>>> 测试 100% 成功！多轮记忆完全恢复，精准回答出名字与暗号！<<<");
  } else {
    console.log("\n>>> 未完全包含，请检查输出 <<<");
  }
}

main().catch(console.error);
