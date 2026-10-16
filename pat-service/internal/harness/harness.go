// Package harness guesses which agent harness (Hermes, Qwen Code, opencode,
// pi, ...) sent a chat-completion request, and what kind of call it is, from
// the request's first system message and tool names (docs/adr/0025). The
// rules were derived from real Langfuse traces; a request no rule matches is
// "unknown", never an error.
package harness

import (
	"encoding/json"
	"strings"
)

const (
	Unknown = "unknown"
	// promptWindow bounds how much of the system prompt rules look at;
	// every signature sits in the opening lines.
	promptWindow = 2048
)

// Result is one request's detected harness and call role.
type Result struct {
	Harness string
	Role    string
}

// Header is the X-Agent-Harness value: "<harness>/<role>".
func (r Result) Header() string { return r.Harness + "/" + r.Role }

type rule struct {
	harness, role string
	// prompt is a substring of the first system message's opening window.
	prompt string
	// tools, when set, must also hold for the request's tool names.
	tools func(map[string]bool) bool
}

func anyTool(names ...string) func(map[string]bool) bool {
	return func(t map[string]bool) bool {
		for _, n := range names {
			if t[n] {
				return true
			}
		}
		return false
	}
}

// rules are ordered: first match wins, so specific prompts come before the
// broader ones that could also contain them.
var rules = []rule{
	{"hermes", "compaction", "You are a summarization agent creating a context checkpoint", nil},
	{"hermes", "title", "You name chat sessions", nil},
	{"hermes", "main", "Hermes Agent", nil},
	{"hermes", "main", "personal Hermes agent", nil},

	{"opencode", "title", "You are a title generator. You output ONLY a thread title", nil},
	{"opencode", "main", "You are opencode", nil},

	{"qwen-code", "main", "You are Qwen Code", nil},
	{"qwen-code", "memory", "managed memory dream", nil},
	{"qwen-code", "memory", "managed memory extraction", nil},
	{"qwen-code", "memory", "You are selecting memories that will be useful to an AI coding assistant", nil},
	{"qwen-code", "classifier", "security classifier for an AI coding agent operating in auto mode", nil},
	{"qwen-code", "title", "You generate session recaps", nil},
	{"qwen-code", "router", "You are deciding how a user's new message should be handled", nil},
	{"qwen-code", "compaction", "summarizes a conversation when its context window is about to overflow", nil},

	{"pi", "main", "operating inside pi, a coding agent harness", nil},
	{"deepseek-harness", "main", "powered by DeepSeek Harness", nil},
	{"kilo", "main", "You are Kilo,", nil},

	{"qwen-code", "subagent", "You are a file search specialist", anyTool("grep_search", "run_shell_command")},
	{"opencode", "subagent", "You are a file search specialist", anyTool("webfetch", "read")},
}

// toolRules apply when no prompt rule matched.
var toolRules = []rule{
	{"hermes", "main", "", anyTool("browser_vault_list", "delegate_task", "skill_manage")},
	{"qwen-code", "main", "", anyTool("grep_search", "run_shell_command")},
}

// userAgents is the last resort, for clients whose prompt no rule knows.
var userAgents = []struct{ substr, harness string }{
	{"opencode", "opencode"},
	{"qwen-code", "qwen-code"},
	{"hermes", "hermes"},
	{"kilo", "kilo"},
}

type chatBody struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// Detect classifies a chat-completion body. userAgent may be empty.
func Detect(body []byte, userAgent string) Result {
	var req chatBody
	if json.Unmarshal(body, &req) != nil {
		return Result{Unknown, Unknown}
	}
	prompt := systemPrompt(req)
	if len(prompt) > promptWindow {
		prompt = prompt[:promptWindow]
	}
	tools := map[string]bool{}
	for _, t := range req.Tools {
		if t.Function.Name != "" {
			tools[t.Function.Name] = true
		} else if t.Name != "" {
			tools[t.Name] = true
		}
	}
	for _, r := range rules {
		if strings.Contains(prompt, r.prompt) && (r.tools == nil || r.tools(tools)) {
			return Result{r.harness, r.role}
		}
	}
	for _, r := range toolRules {
		if r.tools(tools) {
			return Result{r.harness, r.role}
		}
	}
	ua := strings.ToLower(userAgent)
	for _, u := range userAgents {
		if strings.Contains(ua, u.substr) {
			return Result{u.harness, Unknown}
		}
	}
	return Result{Unknown, Unknown}
}

// systemPrompt returns the first system (or developer) message's text,
// whether content is a string or an array of text parts. Some auxiliary
// calls (Hermes compaction, Qwen Code's message router) carry their
// instructions in a user message and have no system message; for those the
// first message is used.
func systemPrompt(req chatBody) string {
	if len(req.Messages) == 0 {
		return ""
	}
	content := req.Messages[0].Content
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			content = m.Content
			break
		}
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
		if b.Len() >= promptWindow {
			break
		}
	}
	return b.String()
}
