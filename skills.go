package lmchatkit

import (
	"context"
	"encoding/json"
	"strings"
)

// SkillToolName is the name of the virtual skill-retrieval tool. The
// lmchatkit__ prefix prevents collisions with local scriptling tools (no
// prefix) and remote MCP tools (namespace__ prefix).
const SkillToolName = "lmchatkit__get_skill"

// SkillTool is the virtual tool definition appended to the tool list
// when the host has skill:// resources. It lets the LLM pull in skill
// instructions on demand — the model sees skill descriptions in the
// system prompt, then calls this tool to retrieve the full content.
var SkillTool = Tool{
	Name:        SkillToolName,
	Description: "Retrieve a skill's detailed instructions by URI. Pass the skill URI (e.g. 'skill://golang/SKILL.md'). Returns the skill content as text.",
	InputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"uri": map[string]interface{}{
				"type":        "string",
				"description": "The skill URI to retrieve (e.g. skill://golang/SKILL.md)",
			},
		},
		"required": []string{"uri"},
	},
}

// hasSkillResources returns true if the host exposes any resources with
// a "skill://" URI prefix. Called by the tool listing handler to decide
// whether to include the virtual skill tool.
func (s *Server) hasSkillResources(ctx context.Context) bool {
	resources, err := s.host.ListResources(ctx)
	if err != nil {
		return false
	}
	for _, res := range resources {
		if strings.HasPrefix(res.URI, "skill://") {
			return true
		}
	}
	return false
}

// trySkillToolCall intercepts the lmchatkit__get_skill virtual tool call
// and routes it to Host.ReadResource. Returns (result, true) if handled,
// (_, false) if the tool name doesn't match.
//
// The tool takes exactly one argument, uri, as declared on every request;
// anything else is an error the model corrects on its next call. A leading
// @ is stripped (the chat uses @ for resource references).
func (s *Server) trySkillToolCall(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, bool) {
	if name != SkillToolName {
		return ToolResult{}, false
	}

	var args struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(arguments, &args)

	uri := strings.TrimPrefix(strings.TrimSpace(args.URI), "@")
	if uri == "" {
		return ToolResult{Content: "Error: skill URI is required", IsError: true}, true
	}

	result, err := s.host.ReadResource(ctx, uri)
	if err != nil {
		return ToolResult{Content: "Error: skill not found: " + uri, IsError: true}, true
	}

	content := result.Text
	if content == "" && result.Blob != "" {
		content = result.Blob
	}
	return ToolResult{Content: content}, true
}
