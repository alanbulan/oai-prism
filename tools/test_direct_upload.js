const http = require("http");
const fs = require("fs");

const imgPath = "C:/Users/13080/.gemini/antigravity/brain/ed4ebc7a-ece1-4b85-ab3a-5abf84858871/.user_uploaded/media_1791005319419.png";
const imgBytes = fs.readFileSync(imgPath);

const projectId = "45e79a6d-eef0-46e9-b757-7a95a7f536a5";
const filename = "test_upload_1.png";

const req = http.request("http://127.0.0.1:8790/api/project-files/upload", {
  method: "POST",
  headers: {
    "content-type": "image/png",
    "x-prism-file-name": filename,
    "x-prism-file-id": "11111111-2222-3333-4444-555555555555",
    "x-prism-file-size": imgBytes.length.toString(),
    "x-prism-project-id": projectId,
    "x-prism-require-project-edit-access": "true",
    "Content-Length": imgBytes.length
  }
}, (res) => {
  console.log("Direct Upload HTTP Status:", res.statusCode);
  console.log("Direct Upload Headers:", res.headers);
  let body = "";
  res.on("data", chunk => body += chunk);
  res.on("end", () => {
    console.log("Direct Upload Body:", body);
  });
});

req.on("error", err => console.error("Error:", err));
req.write(imgBytes);
req.end();
