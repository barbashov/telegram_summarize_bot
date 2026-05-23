package provider

import "strings"

// visionModelPrefixes lists model-name prefixes (case-insensitive, matched
// after the last '/' to allow vendor-prefixed names like "openai/gpt-4o") that
// are known to support image inputs. The list is intentionally conservative —
// adding a missing prefix is safer than mis-claiming vision support, which
// would surface as 4xx from the upstream API.
var visionModelPrefixes = []string{
	"gpt-4o",
	"gpt-4.1",
	"gpt-4-vision",
	"gpt-4-turbo",
	"gpt-5",
	"o1",
	"o3",
	"o4",
	"claude-3",
	"claude-sonnet-4",
	"claude-opus-4",
	"claude-haiku-4",
	"llama-3.2-vision",
	"llama-3.2-11b-vision",
	"llama-3.2-90b-vision",
	"qwen-vl",
	"qwen2-vl",
	"qwen2.5-vl",
	"gemini-",
	"pixtral",
	"mistral-large-2",
}

// modelSupportsVisionByPrefix returns true when the model name (case-insensitive,
// taking the segment after the last '/' as the model ID) starts with one of
// the known vision-capable prefixes.
func modelSupportsVisionByPrefix(model string) bool {
	if model == "" {
		return false
	}
	id := model
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		id = model[i+1:]
	}
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range visionModelPrefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// SupportsVision implements VisionCapable on each provider client. We expose
// the same check from every client so that capability gating is symmetric
// across LLM_MODE values.
func (c *completionsClient) SupportsVision(model string) bool {
	return modelSupportsVisionByPrefix(model)
}
func (c *responsesClient) SupportsVision(model string) bool {
	return modelSupportsVisionByPrefix(model)
}
func (c *oauthClient) SupportsVision(model string) bool { return modelSupportsVisionByPrefix(model) }
