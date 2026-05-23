package provider

import "testing"

func TestModelSupportsVisionByPrefix(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"", false},
		{"gpt-5", true},
		{"gpt-5.5", true},
		{"GPT-5.5", true},
		{"gpt-4o", true},
		{"gpt-4o-mini", true},
		{"openai/gpt-4o", true},
		{"openai/gpt-5.5", true},
		{"gpt-3.5-turbo", false},
		{"claude-3-5-sonnet", true},
		{"claude-sonnet-4-6", true},
		{"meta-llama/llama-3.3-70b-instruct", false},
		{"meta-llama/llama-3.2-90b-vision-instruct", true},
		{"qwen2-vl-72b-instruct", true},
		{"google/gemini-1.5-flash", true},
		{"  gpt-5  ", true}, // trimmed
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			got := modelSupportsVisionByPrefix(c.model)
			if got != c.want {
				t.Errorf("modelSupportsVisionByPrefix(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

func TestClientSupportsVisionDelegates(t *testing.T) {
	// Each client type exposes SupportsVision; all should agree with the
	// shared helper so capability checks stay symmetric across LLM_MODE.
	cc := &completionsClient{}
	rc := &responsesClient{}
	oc := &oauthClient{}
	for _, name := range []string{"gpt-5.5", "claude-3-5-sonnet", "no-such-model"} {
		want := modelSupportsVisionByPrefix(name)
		if cc.SupportsVision(name) != want {
			t.Errorf("completionsClient.SupportsVision(%q) != helper", name)
		}
		if rc.SupportsVision(name) != want {
			t.Errorf("responsesClient.SupportsVision(%q) != helper", name)
		}
		if oc.SupportsVision(name) != want {
			t.Errorf("oauthClient.SupportsVision(%q) != helper", name)
		}
	}
}
