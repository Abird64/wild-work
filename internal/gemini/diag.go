// diag.go 上游 400 的现场取证（诊断用）。
//
// # 为什么需要（2026-11-04 用户实测）
//
// Gemini 有的 400 会给出精确字段（`Base64 decoding failed for "…"`、
// `Invalid JSON payload received. Unknown name "$schema" at …`），有的**只回一句**
// `Request contains an invalid argument.` —— 后者信息量为零，从错误本身无法反推原因。
//
// 已用单发对照逐一排除的（都返回 200，说明上游对这些是宽容的）：
// 工具调用/结果不配对、未声明的工具名、空文本 part、并行调用被拆成多个 model 块、
// 多步的哨兵签名、历史轮的哨兵签名、4MB 的工具结果、maxOutputTokens 到 1e6、
// schema 的 additionalProperties / oneOf。
//
// 所以只剩一条可靠路径：**把出错那一次的请求体原样留下来**，离线比对。
// 本文件提供两件事：
//  1. summarizeRequest —— 无内容的结构摘要（始终写日志，可长期开着）；
//  2. 全量落盘（仅当 `config.body_log` 开启时，与既有「请求体摘要记录」同一开关、
//     同一 `data/` 目录，用户显式开过一次才写）。
package gemini

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// requestShape 结构摘要的解析目标（只取结构，不取内容）。
type requestShape struct {
	Request struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         *string         `json:"text"`
				FunctionCall json.RawMessage `json:"functionCall"`
				FunctionResp json.RawMessage `json:"functionResponse"`
				InlineData   json.RawMessage `json:"inlineData"`
				ThoughtSig   string          `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		GenerationConfig struct {
			MaxOutputTokens int64 `json:"maxOutputTokens"`
			ThinkingConfig  *struct {
				ThinkingBudget *int64 `json:"thinkingBudget"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	} `json:"request"`
	Model string `json:"model"`
}

// summarizeRequest 生成**不含任何内容**的结构摘要，用于在 400 时留下一行可诊断的证据。
//
// 只报容易出错且能一眼看出异常的维度：角色序列、块类型分布、签名真假计数、
// 以及几类结构异常（空 parts、块内既有文本又有 functionCall、args 不是对象、
// 工具声明数）。**不输出任何消息正文、参数值或工具名全文**（工具名只报数量与超长项）。
func summarizeRequest(body []byte) string {
	var sh requestShape
	if err := json.Unmarshal(body, &sh); err != nil {
		return fmt.Sprintf("bytes=%d 解析失败=%v", len(body), err)
	}
	r := sh.Request

	roles := make([]string, 0, len(r.Contents))
	var parts, nText, nCall, nResp, nImage, nEmptyContent int
	realSig, sentinelSig, badSig := 0, 0, 0
	var bothTextAndCall, nonObjectArgs int
	for _, c := range r.Contents {
		roles = append(roles, shortRole(c.Role))
		if len(c.Parts) == 0 {
			nEmptyContent++
		}
		for _, p := range c.Parts {
			parts++
			switch {
			case len(p.FunctionCall) > 0:
				nCall++
				var probe map[string]any
				if err := json.Unmarshal(p.FunctionCall, &probe); err == nil {
					if a, ok := probe["args"]; ok {
						if _, isObj := a.(map[string]any); !isObj && a != nil {
							nonObjectArgs++
						}
					}
				}
			case len(p.FunctionResp) > 0:
				nResp++
			case len(p.InlineData) > 0:
				nImage++
			case p.Text != nil:
				nText++
			}
			if p.Text != nil && len(p.FunctionCall) > 0 {
				bothTextAndCall++
			}
			sig := strings.TrimSpace(p.ThoughtSig)
			switch {
			case sig == "":
			case sig == sigSkipValidator:
				sentinelSig++
			case isValidSignature(sig):
				realSig++
			default:
				badSig++
			}
		}
	}

	toolCount, longNames := 0, 0
	for _, t := range r.Tools {
		for _, d := range t.FunctionDeclarations {
			toolCount++
			if len(d.Name) > 64 { // Gemini 函数名上限 64
				longNames++
			}
		}
	}
	sysBytes := 0
	if r.SystemInstruction != nil {
		for _, p := range r.SystemInstruction.Parts {
			sysBytes += len(p.Text)
		}
	}
	budget := "nil"
	if r.GenerationConfig.ThinkingConfig != nil && r.GenerationConfig.ThinkingConfig.ThinkingBudget != nil {
		budget = fmt.Sprintf("%d", *r.GenerationConfig.ThinkingConfig.ThinkingBudget)
	}

	// 角色序列太长时压缩成「数量 + 首尾」，避免日志行爆掉
	roleSeq := strings.Join(roles, ",")
	if len(roles) > 24 {
		roleSeq = fmt.Sprintf("(%d个)%s … %s", len(roles),
			strings.Join(roles[:8], ","), strings.Join(roles[len(roles)-8:], ","))
	}

	sa := auditSchemas(r.Tools)

	return fmt.Sprintf(
		"bytes=%d model=%s contents=%d roles=%s parts=%d[text=%d call=%d resp=%d img=%d] "+
			"emptyContents=%d bothTextAndCall=%d nonObjectArgs=%d "+
			"sig[real=%d sentinel=%d bad=%d] tools=%d longNames=%d sysBytes=%d maxOut=%d budget=%s "+
			"schema[reqMiss=%d noType=%d arrNoItems=%d enumNonStr=%d unknownKey=%d maxDepth=%d first=%s]",
		len(body), sh.Model, len(r.Contents), roleSeq, parts, nText, nCall, nResp, nImage,
		nEmptyContent, bothTextAndCall, nonObjectArgs,
		realSig, sentinelSig, badSig, toolCount, longNames, sysBytes,
		r.GenerationConfig.MaxOutputTokens, budget,
		sa.reqMissing, sa.noType, sa.arrNoItems, sa.enumNonStr, sa.unknownKey, sa.maxDepth, sa.firstBad)
}

// shortRole 把角色缩成一个字符，让序列可读。
func shortRole(r string) string {
	switch r {
	case "user":
		return "u"
	case "model":
		return "m"
	case "system":
		return "s"
	case "":
		return "_"
	}
	return "?"
}

// DumpFailedBody 落盘出错请求体的钩子（由 app 装配时注入；nil = 不落盘）。
//
// 之所以用钩子而不是让本包直接写文件：本包不知道 `data/` 目录在哪，
// 也不该自己决定「诊断开关是否开启」（那是 app 的 config.body_log）。
var DumpFailedBody func(body, upstreamRaw []byte, status int)

// dumpFailed 在 400 现场调用：写结构摘要（始终）+ 可选全量落盘。
func dumpFailed(body, upstreamRaw []byte, status int) {
	if status != 400 {
		return
	}
	if DumpFailedBody != nil {
		DumpFailedBody(body, upstreamRaw, status)
	}
}

// SaveFailedBody 把一个出错的请求体与上游响应写到 dir（调用方保证 dir 已存在）。
//
// 只保留最近 maxKeep 份，避免无限增长（每次 400 都写一份可能很大）。
func SaveFailedBody(dir string, body, upstreamRaw []byte, status int) (string, error) {
	const maxKeep = 5
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, "gemini-400-"+time.Now().Format("20060102-150405"))
	reqPath := base + "-request.json"
	if err := os.WriteFile(reqPath, body, 0o600); err != nil {
		return "", err
	}
	_ = os.WriteFile(base+"-upstream.txt", upstreamRaw, 0o600)

	// 轮转：按文件名排序（时间戳天然有序），删掉最旧的
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "gemini-400-") && strings.HasSuffix(e.Name(), "-request.json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > maxKeep {
		old := strings.TrimSuffix(names[0], "-request.json")
		_ = os.Remove(filepath.Join(dir, old+"-request.json"))
		_ = os.Remove(filepath.Join(dir, old+"-upstream.txt"))
		names = names[1:]
	}
	return reqPath, nil
}

// schemaAudit 工具 schema 的结构异常计数（**不含任何 schema 内容**，只报计数与首个违规位置）。
type schemaAudit struct {
	reqMissing int // required 指向不存在的 property
	noType     int // 有 properties 但缺 type
	arrNoItems int // type=array 但缺 items
	enumNonStr int // enum 含非字符串
	unknownKey int // 清洗后仍残留白名单外的键（应恒为 0；非 0 说明清洗有漏洞）
	maxDepth   int // 实测最大嵌套深度
	firstBad   string
}

// auditSchemas 审计全部工具声明的 parameters。
//
// 为什么放进 400 摘要：上游对 schema 语义错误**会给明确文案**，但历史上也出现过
// 只回泛化 invalid_argument 的情形；把这几类计数带上，一次复发就能判断是否 schema 所致，
// 而不必再要全量请求体。
func auditSchemas(tools []struct {
	FunctionDeclarations []struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"functionDeclarations"`
}) schemaAudit {
	var a schemaAudit
	note := func(tool, what string) {
		if a.firstBad == "" {
			a.firstBad = tool + ":" + what
		}
	}
	var walk func(tool string, m map[string]any, depth int)
	walk = func(tool string, m map[string]any, depth int) {
		if depth > a.maxDepth {
			a.maxDepth = depth
		}
		props, hasProps := m["properties"].(map[string]any)
		if hasProps {
			if _, hasType := m["type"]; !hasType {
				a.noType++
				note(tool, "noType")
			}
		}
		if t, _ := m["type"].(string); t == "array" {
			if _, hasItems := m["items"]; !hasItems {
				a.arrNoItems++
				note(tool, "arrNoItems")
			}
		}
		if req, ok := m["required"].([]any); ok {
			for _, it := range req {
				name, _ := it.(string)
				if name == "" {
					continue
				}
				if _, exists := props[name]; !exists {
					a.reqMissing++
					note(tool, "reqMissing="+name)
				}
			}
		}
		if en, ok := m["enum"].([]any); ok {
			for _, it := range en {
				if _, isStr := it.(string); !isStr {
					a.enumNonStr++
					note(tool, "enumNonStr")
					break
				}
			}
		}
		for k := range m {
			if !geminiSchemaKeys[k] {
				a.unknownKey++
				note(tool, "unknownKey="+k)
			}
		}
		for _, child := range props {
			if cm, ok := child.(map[string]any); ok {
				walk(tool, cm, depth+1)
			}
		}
		if im, ok := m["items"].(map[string]any); ok {
			walk(tool, im, depth+1)
		}
		if arr, ok := m["anyOf"].([]any); ok {
			for _, it := range arr {
				if cm, ok := it.(map[string]any); ok {
					walk(tool, cm, depth+1)
				}
			}
		}
	}
	for _, t := range tools {
		for _, d := range t.FunctionDeclarations {
			if len(d.Parameters) == 0 {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(d.Parameters, &m); err != nil {
				note(d.Name, "unparsable")
				continue
			}
			walk(d.Name, m, 1)
		}
	}
	return a
}
