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
				Name string `json:"name"`
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

	return fmt.Sprintf(
		"bytes=%d model=%s contents=%d roles=%s parts=%d[text=%d call=%d resp=%d img=%d] "+
			"emptyContents=%d bothTextAndCall=%d nonObjectArgs=%d "+
			"sig[real=%d sentinel=%d bad=%d] tools=%d longNames=%d sysBytes=%d maxOut=%d budget=%s",
		len(body), sh.Model, len(r.Contents), roleSeq, parts, nText, nCall, nResp, nImage,
		nEmptyContent, bothTextAndCall, nonObjectArgs,
		realSig, sentinelSig, badSig, toolCount, longNames, sysBytes,
		r.GenerationConfig.MaxOutputTokens, budget)
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
