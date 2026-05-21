package plugin_test

import (
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

func TestRender(t *testing.T) {
	ctx := plugin.NewTemplateContext()
	ctx.Instance["id"] = "i-0abc123"
	ctx.Config["endpoint_name"] = "my-ep"
	ctx.Outputs["setup_key"] = "sk-xyz"
	ctx.Pushed["token"] = "tok123"

	tests := []struct {
		tmpl string
		want string
	}{
		{"{{ instance.id }}", "i-0abc123"},
		{"{{ config.endpoint_name }}", "my-ep"},
		{"{{ outputs.setup_key }}", "sk-xyz"},
		{"{{ pushed.token }}", "tok123"},
		{"id={{ instance.id }} name={{ config.endpoint_name }}", "id=i-0abc123 name=my-ep"},
		{"no template here", "no template here"},
	}

	for _, tc := range tests {
		t.Run(tc.tmpl, func(t *testing.T) {
			got, err := plugin.Render(tc.tmpl, ctx)
			if err != nil {
				t.Fatalf("Render(%q): %v", tc.tmpl, err)
			}
			if got != tc.want {
				t.Errorf("Render(%q) = %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}

func TestRender_MissingKey(t *testing.T) {
	ctx := plugin.NewTemplateContext()
	_, err := plugin.Render("{{ config.missing }}", ctx)
	if err == nil {
		t.Fatal("expected error for missing config key")
	}
}

func TestRenderStep(t *testing.T) {
	ctx := plugin.NewTemplateContext()
	ctx.Outputs["setup_key"] = "sk-abc"

	step := plugin.Step{
		Type: "run",
		Run:  "/opt/gcp/globusconnect -setup {{ outputs.setup_key }}",
	}

	rendered, err := plugin.RenderStep(step, ctx)
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}

	want := "/opt/gcp/globusconnect -setup sk-abc"
	if rendered.Run != want {
		t.Errorf("rendered.Run = %q, want %q", rendered.Run, want)
	}
}
