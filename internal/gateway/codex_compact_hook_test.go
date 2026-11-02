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

// Codex's compaction request must run request hooks with compact set, while
// an ordinary turn keeps the same hook contract it had before.
func TestCodexCompactRunsGatewayHooksInCompactMode(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"SUMMARY"},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	setup(t, provider.Chat, f)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("third-party model compaction must not reach ChatGPT")
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
		t.Fatalf("ordinary request returned %d: %s", code, body)
	}
	if !strings.Contains(string(f.got), "data:image/") {
		t.Fatalf("ordinary request image did not reach upstream: %s", f.got)
	}
	assertHookModes(t, logPath, []bool{false})

	code, body = codexPost(t, `{"model":"fake/m1","stream":true,"input":[`+conversation+`,{"type":"compaction_trigger"}]}`)
	if code != http.StatusOK || !strings.Contains(body, magpieCompaction) {
		t.Fatalf("compaction returned %d: %s", code, body)
	}
	if strings.Contains(string(f.got), "data:image/") {
		t.Fatalf("compaction sent an inline image upstream: %s", f.got)
	}
	if !strings.Contains(string(f.got), "[image removed by compact hook]") {
		t.Fatalf("compaction did not use the hooked body: %s", f.got)
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
		t.Fatalf("hook called %d times, want %d: %s", len(lines), len(want), b)
	}
	for i, line := range lines {
		if strings.Contains(line, `"compact":true`) != want[i] {
			t.Fatalf("hook call %d compact=%t, want %t: %s", i+1, !want[i], want[i], line)
		}
		if !strings.Contains(line, `"path":"`+CodexPath+`/responses"`) {
			t.Fatalf("hook call %d path is wrong: %s", i+1, line)
		}
	}
}
