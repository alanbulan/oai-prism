package facade

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Codex 客户端除 exec_command / apply_patch 之外能执行的工具：MCP 工具与 tool_search。
//
// Codex 0.160 按模型元数据有三种形态（2026-10-05 抓包，测试用 MCP 服务器 probe-kit）：
//
//   - code mode（gpt-6.x：tool_mode = code_mode_only）：工具全在 exec 的 JS 里，MCP 工具是
//     tools.mcp__probe_kit__lookup_codeword({...})，而且延迟加载 —— exec 的说明里不列，只写
//     "Some deferred nested tools may be omitted …"，要在 JS 全局 ALL_TOOLS 里找；
//   - direct mode：MCP 工具以 {"type":"namespace","name":"mcp__probe_kit","tools":[function…]}
//     列在 tools 里，调用是 function_call{namespace:"mcp__probe_kit", name:"lookup_codeword"}；
//   - direct + tool_search（gpt-5.5 这类 supports_search_tool 的模型）：MCP 工具延迟加载，tools 里
//     只有 {"type":"tool_search","execution":"client"}，说明里列出来源（服务器名）。模型发
//     tool_search_call，Codex 回 tool_search_output（带工具定义），之后照 direct 调用。
//
// 服务器名里的连字符一律换成下划线（probe-kit → mcp__probe_kit）。
//
// 桥让上游三种形态下都用同一种写法：在 codex-exec 块里 `await tools.<名字>(参数)`。code mode
// 原样交给客户端执行；function 形态由网关求值块（execjs.go），把调用换成 function_call /
// tool_search_call。早先桥只讲 exec_command，上游根本不知道有 MCP 工具（实测回答"我没有这个工具"）。

// codexMCPTool 是一个已知的 MCP 工具。
type codexMCPTool struct {
	Ident       string          // JS 里的名字：mcp__probe_kit__lookup_codeword
	Namespace   string          // function_call 的 namespace（direct）；扁平名字与 code mode 为空
	Name        string          // function_call 的 name
	Description string          //
	Params      json.RawMessage // 参数的 JSON Schema（direct / tool_search_output）
	Decl        string          // code mode exec 说明里的 TS 声明
}

// codexClientTools 是这次请求里客户端声明（或之前搜出来）的额外工具。
type codexClientTools struct {
	mcp      []codexMCPTool
	byIdent  map[string]int
	deferred bool     // code mode：有工具没列出来（在 ALL_TOOLS 里找）
	search   bool     // direct：有 tool_search
	sources  []string // tool_search 说明里列出的来源
	funcs    []string // 其余可调用的 function 工具（view_image、read_mcp_resource …）
	// function 表示客户端是 function 形态（exec_command 是 function 工具）：块由网关求值，
	// 块里的调用换成 function_call（见 execjs.go 的 functionCalls）。
	function bool
}

// codexToolSpec 是 Responses 工具定义里用得到的字段（function / custom / namespace / tool_search）。
type codexToolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Tools       []codexToolSpec `json:"tools"`
}

// codexDeferredMark 是 code mode exec 说明里"还有工具没列出来"的那句话。
const codexDeferredMark = "Some deferred nested tools may be omitted"

// parseCodexClientTools 从 tools、input 里的 additional_tools 与历史里的 tool_search_output 收集工具。
func parseCodexClientTools(raw map[string]json.RawMessage) *codexClientTools {
	c := &codexClientTools{byIdent: map[string]int{}}
	var specs []codexToolSpec
	_ = json.Unmarshal(raw["tools"], &specs)
	var items []struct {
		Type  string          `json:"type"`
		Tools []codexToolSpec `json:"tools"`
	}
	_ = json.Unmarshal(raw["input"], &items)
	for _, it := range items {
		switch it.Type {
		case "additional_tools":
			specs = append(specs, it.Tools...)
		case "tool_search_output":
			for _, t := range it.Tools {
				c.addSpec(t, "")
			}
		}
	}
	for _, t := range specs {
		c.addSpec(t, "")
	}
	return c
}

func (c *codexClientTools) addSpec(t codexToolSpec, ns string) {
	switch t.Type {
	case "namespace":
		for _, child := range t.Tools {
			if strings.HasPrefix(t.Name, "mcp__") {
				c.addSpec(child, t.Name)
			} else if child.Type == "custom" && child.Name == "exec" {
				c.addSpec(child, "") // code mode：exec 在 functions 命名空间里
			}
		}
	case "function":
		switch {
		case ns != "":
			c.addMCP(codexMCPTool{Ident: joinToolIdent(ns, t.Name), Namespace: ns, Name: t.Name,
				Description: t.Description, Params: t.Parameters})
		case strings.HasPrefix(t.Name, "mcp__"):
			c.addMCP(codexMCPTool{Ident: t.Name, Name: t.Name, Description: t.Description, Params: t.Parameters})
		case codexReadTools[t.Name]:
			c.funcs = appendUnique(c.funcs, t.Name)
		}
	case "custom":
		if t.Name == "exec" {
			c.deferred = c.deferred || strings.Contains(t.Description, codexDeferredMark)
			for _, d := range execMCPDeclarations(t.Description) {
				c.addMCP(d)
			}
		}
	case "tool_search":
		c.search = true
		c.sources = toolSearchSources(t.Description)
	}
}

func (c *codexClientTools) addMCP(t codexMCPTool) {
	if t.Ident == "" {
		return
	}
	if i, ok := c.byIdent[t.Ident]; ok {
		c.mcp[i] = t // 后到的（搜索结果）更完整
		return
	}
	c.byIdent[t.Ident] = len(c.mcp)
	c.mcp = append(c.mcp, t)
}

// codexReadTools 是提示词里顺带提到的其他 function 工具：看图与 MCP 资源（读类）。计划模式、目标、
// 子代理之类的工具不提，免得上游在不该用的时候去调。
var codexReadTools = map[string]bool{"view_image": true, "list_mcp_resources": true, "list_mcp_resource_templates": true, "read_mcp_resource": true}

// isExecToolName 是桥自己会用到的执行类工具（在提示词的 exec 段落里讲，不进额外工具清单）。
func isExecToolName(name string) bool {
	switch name {
	case "exec_command", "exec", "shell", "write_stdin", "apply_patch", "wait":
		return true
	}
	return false
}

// joinToolIdent 按 Codex 的规则把 namespace 与工具名拼成 JS 名字（去掉相接处多余的下划线）。
func joinToolIdent(ns, name string) string {
	return strings.TrimRight(ns, "_") + "__" + strings.TrimLeft(name, "_")
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// reExecSection 切出 code mode exec 说明里的 "### `名字`" 小节。
var reExecSection = regexp.MustCompile("(?m)^### `([A-Za-z0-9_$]+)`[ \t]*$")

// execMCPDeclarations 取出 exec 说明里列出的 MCP 工具（没延迟加载时才会列）。
func execMCPDeclarations(desc string) []codexMCPTool {
	locs := reExecSection.FindAllStringSubmatchIndex(desc, -1)
	var out []codexMCPTool
	for i, l := range locs {
		name := desc[l[2]:l[3]]
		if !strings.HasPrefix(name, "mcp__") {
			continue
		}
		end := len(desc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if j := strings.Index(desc[l[1]:end], "\n## "); j >= 0 {
			end = l[1] + j
		}
		body := strings.TrimSpace(desc[l[1]:end])
		d := body
		if j := strings.Index(body, "exec tool declaration:"); j >= 0 {
			d = strings.TrimSpace(body[:j])
		}
		out = append(out, codexMCPTool{Ident: name, Name: name, Description: d, Decl: tsDeclaration(body)})
	}
	return out
}

// tsDeclaration 取出 "declare const tools: { … }" 里的那一句函数签名。
func tsDeclaration(body string) string {
	i := strings.Index(body, "declare const tools: {")
	if i < 0 {
		return ""
	}
	s := body[i+len("declare const tools: {"):]
	if j := strings.LastIndex(s, "}; };"); j >= 0 {
		s = s[:j+2]
	} else if j := strings.Index(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// toolSearchSources 取出 tool_search 说明里 "following sources:" 之后列出的来源。
func toolSearchSources(desc string) []string {
	i := strings.Index(desc, "following sources:")
	if i < 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(desc[i:], "\n")[1:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
	}
	return out
}

// hasExtras 表示有值得在提示词里讲的额外工具。
func (c *codexClientTools) hasExtras() bool {
	return c != nil && (len(c.mcp) > 0 || c.deferred || c.search)
}

// resolve 把 JS 名字换成 function_call 的 namespace 与 name。没见过的 mcp__ 名字按
// "mcp__<服务器>__<工具>" 拆（延迟加载、还没搜过的工具，Codex 照样按名字派发）。
func (c *codexClientTools) resolve(ident string) (ns, name string) {
	if c != nil {
		if i, ok := c.byIdent[ident]; ok {
			return c.mcp[i].Namespace, c.mcp[i].Name
		}
	}
	if rest, ok := strings.CutPrefix(ident, "mcp__"); ok {
		if j := strings.Index(rest, "__"); j > 0 && j+2 < len(rest) {
			return "mcp__" + rest[:j], rest[j+2:]
		}
	}
	return "", ident
}

// codexToolsBudget 是提示词里 MCP 工具清单的字节预算（工具多时逐档简化，同 Claude Code 的目录）。
const codexToolsBudget = 12 << 10

// promptSection 是桥提示词里讲额外工具的一段（没有时为空）。输出必须稳定：system 一变，
// 原生续接就要重发整段。
func (c *codexClientTools) promptSection() string {
	if !c.hasExtras() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("CLIENT MCP TOOLS: the user's MCP servers run on the client too. Call an MCP tool from the same ```codex-exec block, like exec_command:\n")
	sb.WriteString("  text(await tools.mcp__<server>__<tool>({ <arguments> }));\n")
	sb.WriteString("The result is an MCP CallToolResult ({ content: [{ type: \"text\", text }], isError? }); pass it to text() to read it. ")
	sb.WriteString("Use an MCP tool when the user asks for it or when it is clearly the right tool; never claim you called one without the block.\n")
	if c.function {
		sb.WriteString("Put MCP calls in their own block (not mixed with exec_command calls), and do not use their return value inside the block: each call's result comes back to you separately.\n")
	}
	if len(c.mcp) > 0 {
		sb.WriteString("Available MCP tools:\n")
		sb.WriteString(c.renderMCP())
	}
	switch {
	case c.search:
		sb.WriteString("More tools are loaded on demand. To find them, search first (the result lists the tools and their parameters):\n")
		sb.WriteString("  text(await tools.tool_search({ query: \"<what you need>\", limit: 8 }));\n")
		if len(c.sources) > 0 {
			sb.WriteString("Searchable sources: " + strings.Join(c.sources, "; ") + "\n")
		}
	case c.deferred:
		sb.WriteString("More MCP tools may exist that are not listed here. To list them with their parameters, run:\n")
		sb.WriteString("  text(JSON.stringify(ALL_TOOLS.filter(t => t.name.startsWith(\"mcp__\"))));\n")
	}
	if c.function && len(c.funcs) > 0 {
		fs := append([]string(nil), c.funcs...)
		sort.Strings(fs)
		sb.WriteString("Other client tools callable the same way: " + strings.Join(fs, ", ") + ".\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// renderMCP 列出已知 MCP 工具：从最详细的一档开始，放不进预算就换更简略的一档。
func (c *codexClientTools) renderMCP() string {
	out := ""
	for _, lv := range catalogLevels {
		var sb strings.Builder
		for _, t := range c.mcp {
			renderMCPTool(&sb, t, lv)
		}
		out = sb.String()
		if len(out) <= codexToolsBudget {
			break
		}
	}
	return out
}

func renderMCPTool(sb *strings.Builder, t codexMCPTool, lv catalogLevel) {
	sb.WriteString("- tools." + t.Ident)
	if d := strings.Join(strings.Fields(clipText(t.Description, lv.other)), " "); d != "" {
		sb.WriteString(" — " + d)
	}
	sb.WriteString("\n")
	var schema map[string]any
	if len(t.Params) > 0 && json.Unmarshal(t.Params, &schema) == nil {
		var ps strings.Builder
		renderSchemaProps(&ps, schema, "    ", 1, lv)
		if ps.Len() > 0 {
			sb.WriteString("  arguments:\n" + ps.String())
		} else {
			sb.WriteString("  arguments: {}\n")
		}
	} else if t.Decl != "" {
		sb.WriteString("  " + strings.Join(strings.Fields(clipText(t.Decl, max(lv.other, 200))), " ") + "\n")
	}
}

// renderToolSearchOutput 把 tool_search_output 里的工具渲染成 [CLIENT RESULT] 的正文。
func renderToolSearchOutput(raw json.RawMessage) string {
	var specs []codexToolSpec
	_ = json.Unmarshal(raw, &specs)
	c := &codexClientTools{byIdent: map[string]int{}}
	var other []string
	for _, t := range specs {
		c.addSpec(t, "")
		if t.Type == "namespace" && !strings.HasPrefix(t.Name, "mcp__") {
			for _, child := range t.Tools {
				other = append(other, t.Name+"."+child.Name)
			}
		}
	}
	var sb strings.Builder
	if len(c.mcp) > 0 {
		sb.WriteString("Tools found (call them from a codex-exec block as `text(await tools.<name>({...}))`):\n")
		sb.WriteString(c.renderMCP())
	}
	if len(other) > 0 {
		sb.WriteString("Other tools found (not callable through the bridge): " + strings.Join(other, ", ") + "\n")
	}
	if sb.Len() == 0 {
		return "(no matching tools)"
	}
	return strings.TrimRight(sb.String(), "\n")
}
