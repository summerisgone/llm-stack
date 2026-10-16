package harness

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		name, body, ua string
		want           Result
	}{
		{"hermes main", `{"messages":[{"role":"system","content":"You are Hermes Agent, built by Nous Research. Be direct"},{"role":"user","content":"hi"}]}`, "", Result{"hermes", "main"}},
		{"hermes custom persona", `{"messages":[{"role":"system","content":"Ian is team lead and you are assistant\n\nYou run on Hermes Agent"}]}`, "", Result{"hermes", "main"}},
		{"hermes compaction in user message", `{"messages":[{"role":"user","content":"You are a summarization agent creating a context checkpoint. Treat"}]}`, "", Result{"hermes", "compaction"}},
		{"opencode main", `{"messages":[{"role":"system","content":"You are opencode, an interactive CLI tool"}]}`, "", Result{"opencode", "main"}},
		{"opencode title", `{"messages":[{"role":"system","content":"You are a title generator. You output ONLY a thread title. Nothing else."}]}`, "", Result{"opencode", "title"}},
		{"qwen code parts content", `{"messages":[{"role":"system","content":[{"type":"text","text":"You are Qwen Code, a CLI agent operating through an ACP host"}]}]}`, "", Result{"qwen-code", "main"}},
		{"qwen code router", `{"messages":[{"role":"user","content":[{"type":"text","text":"You are deciding how a user's new message should be handled in the current coding session."}]}]}`, "", Result{"qwen-code", "router"}},
		{"pi", `{"messages":[{"role":"system","content":"You are an expert coding assistant operating inside pi, a coding agent harness."}]}`, "", Result{"pi", "main"}},
		{"deepseek harness", `{"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness.\n\nYou are a coding agent"}]}`, "", Result{"deepseek-harness", "main"}},
		{"subagent by tools", `{"messages":[{"role":"system","content":"You are a file search specialist agent."}],"tools":[{"type":"function","function":{"name":"grep_search"}}]}`, "", Result{"qwen-code", "subagent"}},
		{"tools only", `{"messages":[{"role":"system","content":"Custom persona"}],"tools":[{"type":"function","function":{"name":"delegate_task"}}]}`, "", Result{"hermes", "main"}},
		{"user agent fallback", `{"messages":[{"role":"user","content":"hi"}]}`, "opencode/1.2.3 ai-sdk", Result{"opencode", "unknown"}},
		{"unknown", `{"messages":[{"role":"user","content":"hi"}]}`, "curl/8", Result{"unknown", "unknown"}},
		{"not json", `{`, "", Result{"unknown", "unknown"}},
	}
	for _, c := range cases {
		if got := Detect([]byte(c.body), c.ua); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
