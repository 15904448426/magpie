package gateway

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/testenv"
)

// 普通请求和压缩请求都必须运行 Gateway Hook；压缩标记要传给插件，
// 插件改写后的请求才应该到达上游。
func TestCodexCompactRunsGatewayHooksInCompactMode(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"SUMMARY"},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	setup(t, provider.Chat, f)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("第三方模型的压缩请求不应到达 ChatGPT")
		w.WriteHeader(http.StatusBadRequest)
	})

	t.Setenv("MAGPIE_BUN", testenv.Bun(t))
	t.Cleanup(plugin.Settle)
	logPath := filepath.Join(t.TempDir(), "hook-calls.ndjson")
	hookPath := filepath.Join(t.TempDir(), "compact.hooks.mjs")
	script := `
import { appendFile } from "node:fs/promises"

function scrub(value) {
  if (typeof value === "string") {
    return value.startsWith("data:image/") ? "[image removed by compact hook]" : value
  }
  if (Array.isArray(value)) return value.map(scrub)
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, child]) => [key, scrub(child)]))
  }
  return value
}

export async function onRequest(body, ctx) {
  await appendFile(ctx.options.log, JSON.stringify({ compact: ctx.compact, path: ctx.path }) + "\n")
  if (!ctx.compact) return body
  return { ...body, input: body.input.map(scrub) }
}`
	if err := os.WriteFile(hookPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Add(t.Context(), hookPath); err != nil {
		t.Fatal(err)
	}
	if err := plugin.SetOptions(hookPath, map[string]any{"log": logPath, "gatewayTimeoutMs": 10000}); err != nil {
		t.Fatal(err)
	}

	image := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("compact-hook-image"))
	conversation := `{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"` + image + `"}]}`

	code, body := codexPost(t, `{"model":"fake/m1","stream":true,"input":[`+conversation+`]}`)
	if code != http.StatusOK {
		t.Fatalf("普通请求返回 %d: %s", code, body)
	}
	if !strings.Contains(string(f.got), "data:image/") {
		t.Fatalf("普通请求的图片没有到达上游: %s", f.got)
	}
	assertHookModes(t, logPath, []bool{false})

	code, body = codexPost(t, `{"model":"fake/m1","stream":true,"input":[`+conversation+`,{"type":"compaction_trigger"}]}`)
	if code != http.StatusOK || !strings.Contains(body, magpieCompaction) {
		t.Fatalf("压缩请求返回 %d: %s", code, body)
	}
	if strings.Contains(string(f.got), "data:image/") {
		t.Fatalf("压缩请求仍把内联图片发给上游: %s", f.got)
	}
	if !strings.Contains(string(f.got), "[image removed by compact hook]") {
		t.Fatalf("压缩请求没有使用插件改写后的内容: %s", f.got)
	}
	assertHookModes(t, logPath, []bool{false, true})
}

func assertHookModes(t *testing.T, path string, want []bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != len(want) {
		t.Fatalf("Hook 调用 %d 次，期望 %d 次: %s", len(lines), len(want), b)
	}
	for i, line := range lines {
		if strings.Contains(line, `"compact":true`) != want[i] {
			t.Fatalf("第 %d 次 Hook compact=%t，期望 %t: %s", i+1, !want[i], want[i], line)
		}
		if !strings.Contains(line, `"path":"`+CodexPath+`/responses"`) {
			t.Fatalf("第 %d 次 Hook 路径不正确: %s", i+1, line)
		}
	}
}
