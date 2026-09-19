package responses

import (
	"bytes"
	"encoding/json"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := util.GetGJSONBytesNoCopy(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.SetBytes([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`), "0.content.0.text", inputResult.String())
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", input)
		inputResult = util.GetGJSONBytesNoCopy(rawJSON, "input")
	}

	rawJSON = setCodexRequiredBool(rawJSON, "stream", true)
	rawJSON = setCodexRequiredBool(rawJSON, "store", false)
	rawJSON = setCodexRequiredBool(rawJSON, "parallel_tool_calls", true)
	rawJSON = setCodexRequiredInclude(rawJSON)
	// Codex Responses rejects token limit fields, so strip them out before forwarding.
	rawJSON = deleteCodexRequestFields(rawJSON, "max_output_tokens", "max_completion_tokens", "temperature", "top_p")
	if serviceTier := gjson.GetBytes(rawJSON, "service_tier"); serviceTier.Exists() {
		if serviceTier.Type == gjson.String {
			switch strings.ToLower(strings.TrimSpace(serviceTier.String())) {
			case "priority", "fast":
				if serviceTier.String() != "priority" {
					rawJSON, _ = sjson.SetBytes(rawJSON, "service_tier", "priority")
				}
			case "ultrafast":
				if serviceTier.String() != "ultrafast" {
					rawJSON, _ = sjson.SetBytes(rawJSON, "service_tier", "ultrafast")
				}
			default:
				rawJSON = deleteCodexRequestFields(rawJSON, "service_tier")
			}
		} else {
			rawJSON = deleteCodexRequestFields(rawJSON, "service_tier")
		}
	}

	rawJSON = deleteCodexRequestFields(rawJSON, "truncation", "prompt_cache_options", "prompt_cache_retention")
	rawJSON = stripCodexResponsesCacheBreakpoints(rawJSON)
	rawJSON = applyResponsesCompactionCompatibility(rawJSON)

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON = deleteCodexRequestFields(rawJSON, "user")

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	rawJSON = convertSystemRoleToDeveloper(rawJSON)
	rawJSON = normalizeCodexBuiltinTools(rawJSON)
	rawJSON = bridgeToolSearchForCodex(rawJSON)

	return rawJSON
}

// bridgeToolSearchForCodex adapts the OpenAI proprietary tool_search handshake
// for Codex-format upstreams. Providers behind this translator reject the
// native {"type":"tool_search"} declaration and namespace tool declarations,
// so the handshake is replayed as a standard function round trip:
//
//  1. The native tool_search declaration becomes a callable function tool.
//  2. A completed handshake in the input history (tool_search_call plus
//     tool_search_output) becomes a standard function_call/function_call_output
//     pair, and the tools the client resolved are pre-registered on the
//     upstream tools list as flat qualified function names.
//  3. Namespace tool declarations are flattened the same way so the upstream
//     never sees a namespace object.
//
// ConvertCodexResponseToOpenAIResponses reverses the qualified names back into
// name + namespace items so the Codex client keeps executing the tools through
// their owning namespace.
func bridgeToolSearchForCodex(rawJSON []byte) []byte {
	hasToolSearch := false
	if tools := gjson.GetBytes(rawJSON, "tools"); tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "tool_search" {
				hasToolSearch = true
				break
			}
		}
	}
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		for _, item := range input.Array() {
			itemType := item.Get("type").String()
			if itemType == "tool_search_call" || itemType == "tool_search_output" {
				hasToolSearch = true
				break
			}
		}
	}
	if !hasToolSearch {
		return rawJSON
	}

	qualifiedNames := make(map[string]struct{})
	flatTools := make([][]byte, 0, 32)

	flattenChild := func(namespaceName string, child gjson.Result) {
		localName := child.Get("name").String()
		if localName == "" {
			// Nameless builtin tool declarations (e.g. web_search) have no
			// namespace identity; pass them through untouched so provider-side
			// injection and shaping keep working.
			flatTools = append(flatTools, []byte(child.Raw))
			return
		}
		qualified := localName
		if namespaceName != "" && !strings.HasPrefix(localName, "mcp__") && !strings.HasPrefix(localName, namespaceName) {
			qualified = namespaceName + "__" + localName
		}
		if _, dup := qualifiedNames[qualified]; dup {
			return
		}
		qualifiedNames[qualified] = struct{}{}
		updated, errSet := sjson.SetBytes([]byte(child.Raw), "name", qualified)
		if errSet != nil {
			return
		}
		flatTools = append(flatTools, updated)
	}

	flattenTool := func(tool gjson.Result) {
		switch strings.TrimSpace(tool.Get("type").String()) {
		case "tool_search":
			// Replace the proprietary declaration with a callable function so
			// the model can trigger the handshake through a standard call.
			if _, dup := qualifiedNames["tool_search"]; !dup {
				qualifiedNames["tool_search"] = struct{}{}
				synthetic := []byte(`{"type":"function","name":"tool_search","description":"Search for and activate tools from installed apps (e.g. Microsoft Teams, Gmail, Google Drive, Outlook, Jira, GitHub). Always call this tool when the task requires interacting with an external app or service.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"The name of the app or keywords of what you need, e.g. Microsoft Teams or Gmail"}},"required":["query"]}}`)
				flatTools = append(flatTools, synthetic)
			}
			return
		case "namespace":
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			if children := tool.Get("tools"); children.IsArray() {
				for _, child := range children.Array() {
					flattenChild(namespaceName, child)
				}
			}
		default:
			flattenChild("", tool)
		}
	}

	// Flatten top-level tools (includes the native tool_search declaration).
	if tools := gjson.GetBytes(rawJSON, "tools"); tools.IsArray() {
		for _, tool := range tools.Array() {
			flattenTool(tool)
		}
	}

	// Replay the handshake history and harvest the resolved tools.
	if input := gjson.GetBytes(rawJSON, "input"); input.IsArray() {
		rebuiltInput := make([]json.RawMessage, 0, len(input.Array()))
		changed := false
		for _, item := range input.Array() {
			itemRaw := []byte(item.Raw)
			switch item.Get("type").String() {
			case "tool_search_call":
				changed = true
				callID := item.Get("call_id").String()
				if callID == "" {
					callID = "call_codex_tool_search"
				}
				call, _ := sjson.SetBytes([]byte(`{"type":"function_call","call_id":"","name":"tool_search","arguments":"{}"}`), "call_id", callID)
				if arguments := item.Get("arguments"); arguments.Exists() {
					if arguments.Type == gjson.String && arguments.String() != "" {
						call, _ = sjson.SetBytes(call, "arguments", arguments.String())
					} else {
						call, _ = sjson.SetBytes(call, "arguments", arguments.Raw)
					}
				}
				rebuiltInput = append(rebuiltInput, call)
			case "tool_search_output":
				changed = true
				callID := item.Get("call_id").String()
				if callID == "" {
					callID = "call_codex_tool_search"
				}
				output, _ := sjson.SetBytes([]byte(`{"type":"function_call_output","call_id":"","output":""}`), "call_id", callID)
				output, _ = sjson.SetBytes(output, "output", `{"status":"success","tools_loaded":true}`)
				rebuiltInput = append(rebuiltInput, output)
				if tools := item.Get("tools"); tools.IsArray() {
					for _, tool := range tools.Array() {
						flattenTool(tool)
					}
				}
			default:
				rebuiltInput = append(rebuiltInput, itemRaw)
			}
		}
		if changed {
			if marshaled, errMarshal := json.Marshal(rebuiltInput); errMarshal == nil {
				if updated, errSet := sjson.SetRawBytes(rawJSON, "input", marshaled); errSet == nil {
					rawJSON = updated
				}
			}
		}
	}

	if len(flatTools) > 0 {
		updated, errSet := sjson.SetRawBytes(rawJSON, "tools", translatorcommon.JoinRawArray(flatTools))
		if errSet == nil {
			rawJSON = updated
		}
	}

	return rawJSON
}
func setCodexRequiredBool(rawJSON []byte, path string, value bool) []byte {
	current := gjson.GetBytes(rawJSON, path)
	if value && current.Type == gjson.True || !value && current.Type == gjson.False {
		return rawJSON
	}

	updated, errSet := sjson.SetBytes(rawJSON, path, value)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

func setCodexRequiredInclude(rawJSON []byte) []byte {
	current := gjson.GetBytes(rawJSON, "include")
	values := current.Array()
	if current.IsArray() && len(values) == 1 && values[0].Type == gjson.String && values[0].String() == "reasoning.encrypted_content" {
		return rawJSON
	}

	updated, errSet := sjson.SetRawBytes(rawJSON, "include", []byte(`["reasoning.encrypted_content"]`))
	if errSet != nil {
		return rawJSON
	}
	return updated
}

func deleteCodexRequestFields(rawJSON []byte, paths ...string) []byte {
	for _, path := range paths {
		if !gjson.GetBytes(rawJSON, path).Exists() {
			continue
		}

		updated, errDelete := sjson.DeleteBytes(rawJSON, path)
		if errDelete == nil {
			rawJSON = updated
		}
	}
	return rawJSON
}

// stripCodexResponsesCacheBreakpoints removes any "prompt_cache_breakpoint" hint
// attached to input items: inside content-part arrays (message input[].content[]
// and function_call_output input[].output[]) or as an item-level field. Some
// clients (e.g. GitHub Copilot CLI) attach this field per content item when
// targeting the OpenAI Responses format. Codex Responses rejects it outright:
// {"error":{"message":"prompt_cache_breakpoint is not supported on this model", ...}}.
// The top-level prompt_cache_options strip above does not cover these nested cases.
func stripCodexResponsesCacheBreakpoints(rawJSON []byte) []byte {
	if !bytes.Contains(rawJSON, []byte(`"prompt_cache_breakpoint"`)) {
		return rawJSON
	}

	input := util.GetGJSONBytesNoCopy(rawJSON, "input")
	if !input.IsArray() {
		return rawJSON
	}

	inputItems := input.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	changed := false
	rebuiltInput := make([][]byte, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw := []byte(item.Raw)
		for _, arrayPath := range []string{"content", "output"} {
			arrayResult := item.Get(arrayPath)
			if !arrayResult.IsArray() {
				continue
			}
			updatedArray, arrayChanged := stripPromptCacheBreakpointFromContent(arrayResult)
			if !arrayChanged {
				continue
			}
			if updatedItem, errSet := sjson.SetRawBytes(itemRaw, arrayPath, updatedArray); errSet == nil {
				itemRaw = updatedItem
				changed = true
			}
		}
		if item.Get("prompt_cache_breakpoint").Exists() {
			if updatedItem, errDelete := sjson.DeleteBytes(itemRaw, "prompt_cache_breakpoint"); errDelete == nil {
				itemRaw = updatedItem
				changed = true
			}
		}
		rebuiltInput = append(rebuiltInput, itemRaw)
	}
	if !changed {
		return rawJSON
	}

	updated, errSet := sjson.SetRawBytes(rawJSON, "input", translatorcommon.JoinRawArray(rebuiltInput))
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// stripPromptCacheBreakpointFromContent removes "prompt_cache_breakpoint" from each
// content part that carries it and reports whether anything changed.
func stripPromptCacheBreakpointFromContent(content gjson.Result) ([]byte, bool) {
	parts := content.Array()
	hasBreakpoint := false
	for _, part := range parts {
		if part.Get("prompt_cache_breakpoint").Exists() {
			hasBreakpoint = true
			break
		}
	}
	if !hasBreakpoint {
		return nil, false
	}

	changed := false
	rebuiltParts := make([][]byte, 0, len(parts))
	for _, part := range parts {
		partRaw := []byte(part.Raw)
		if part.Get("prompt_cache_breakpoint").Exists() {
			if updated, errDelete := sjson.DeleteBytes(partRaw, "prompt_cache_breakpoint"); errDelete == nil {
				partRaw = updated
				changed = true
			}
		}
		rebuiltParts = append(rebuiltParts, partRaw)
	}
	if !changed {
		return nil, false
	}
	return translatorcommon.JoinRawArray(rebuiltParts), true
}

// applyResponsesCompactionCompatibility handles OpenAI Responses context_management.compaction
// for Codex upstream compatibility.
//
// Codex /responses currently rejects context_management with:
// {"detail":"Unsupported parameter: context_management"}.
//
// Compatibility strategy:
// 1) Remove context_management before forwarding to Codex upstream.
func applyResponsesCompactionCompatibility(rawJSON []byte) []byte {
	if !gjson.GetBytes(rawJSON, "context_management").Exists() {
		return rawJSON
	}

	rawJSON, _ = sjson.DeleteBytes(rawJSON, "context_management")
	return rawJSON
}

// convertSystemRoleToDeveloper traverses the input array and converts any message items
// with role "system" to role "developer". This is necessary because Codex API does not
// accept "system" role in the input array.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	return convertSystemRoleToDeveloperWithInput(rawJSON, util.GetGJSONBytesNoCopy(rawJSON, "input"))
}

func convertSystemRoleToDeveloperWithInput(rawJSON []byte, inputResult gjson.Result) []byte {
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputItems := inputResult.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	hasSystemRole := false
	for _, item := range inputItems {
		if item.IsObject() && item.Get("role").String() == "system" {
			hasSystemRole = true
			break
		}
	}
	if !hasSystemRole {
		return rawJSON
	}

	changed := false
	rebuiltInput := make([]json.RawMessage, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw := []byte(item.Raw)
		if item.IsObject() && item.Get("role").String() == "system" {
			updatedItem, errSetItem := sjson.SetRawBytes(itemRaw, "role", []byte(`"developer"`))
			if errSetItem != nil {
				return rawJSON
			}
			itemRaw = updatedItem
			changed = true
		}
		rebuiltInput = append(rebuiltInput, json.RawMessage(itemRaw))
	}
	if !changed {
		return rawJSON
	}

	inputRaw, errMarshalInput := json.Marshal(rebuiltInput)
	if errMarshalInput != nil {
		return rawJSON
	}
	updated, errSetInput := sjson.SetRawBytes(rawJSON, "input", inputRaw)
	if errSetInput != nil {
		return rawJSON
	}
	return updated
}

// normalizeCodexBuiltinTools rewrites legacy/preview built-in tool variants to the
// stable names expected by the current Codex upstream.
func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	result := normalizeCodexBuiltinToolArray(rawJSON, "tools")
	result = normalizeCodexBuiltinToolAtPath(result, "tool_choice.type")
	return normalizeCodexBuiltinToolArray(result, "tool_choice.tools")
}

func normalizeCodexBuiltinToolArray(rawJSON []byte, path string) []byte {
	tools := gjson.GetBytes(rawJSON, path)
	if !tools.IsArray() {
		return rawJSON
	}

	changed := false
	var toolItems [][]byte
	tools.ForEach(func(_, tool gjson.Result) bool {
		item := []byte(tool.Raw)
		currentType := tool.Get("type").String()
		normalizedType := normalizeCodexBuiltinToolType(currentType)
		if normalizedType != "" {
			updated, errSetType := sjson.SetBytes(item, "type", normalizedType)
			if errSetType == nil {
				item = updated
				changed = true
				log.Debugf("codex responses: normalized builtin tool type at %s.%d.type from %q to %q", path, len(toolItems), currentType, normalizedType)
			}
		}
		toolItems = append(toolItems, item)
		return true
	})
	if !changed {
		return rawJSON
	}

	updated, errSetTools := sjson.SetRawBytes(rawJSON, path, translatorcommon.JoinRawArray(toolItems))
	if errSetTools != nil {
		return rawJSON
	}
	return updated
}

func normalizeCodexBuiltinToolAtPath(rawJSON []byte, path string) []byte {
	currentType := gjson.GetBytes(rawJSON, path).String()
	normalizedType := normalizeCodexBuiltinToolType(currentType)
	if normalizedType == "" {
		return rawJSON
	}

	updated, err := sjson.SetBytes(rawJSON, path, normalizedType)
	if err != nil {
		return rawJSON
	}

	log.Debugf("codex responses: normalized builtin tool type at %s from %q to %q", path, currentType, normalizedType)
	return updated
}

// normalizeCodexBuiltinToolType centralizes the current known Codex Responses
// built-in tool alias compatibility. If Codex introduces more legacy aliases,
// extend this helper instead of adding path-specific rewrite logic elsewhere.
func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
}
