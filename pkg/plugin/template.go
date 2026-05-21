package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template" // nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template
)

// ErrMissingKey is returned when a {{ pushed.key }} reference cannot be
// resolved because the pushed value has not yet been delivered.
var ErrMissingKey = errors.New("missing pushed key")

// TemplateContext holds all variable namespaces available in plugin templates.
type TemplateContext struct {
	Instance map[string]string // instance.id, instance.name, instance.ip
	Config   map[string]string // config.<key>
	Outputs  map[string]string // outputs.<key> — captured from prior steps
	Pushed   map[string]string // pushed.<key> — received via push API
}

// NewTemplateContext creates an empty TemplateContext with initialised maps.
func NewTemplateContext() TemplateContext {
	return TemplateContext{
		Instance: make(map[string]string),
		Config:   make(map[string]string),
		Outputs:  make(map[string]string),
		Pushed:   make(map[string]string),
	}
}

// Render renders a template string using the given context.
//
// Template syntax: {{ namespace.key }}
//
//	{{ instance.id }}       EC2 instance ID
//	{{ instance.name }}     instance Name tag
//	{{ instance.ip }}       public IP
//	{{ config.key }}        user-supplied config parameter
//	{{ outputs.key }}       value captured from a prior step
//	{{ pushed.key }}        value received via the push API
func Render(tmpl string, ctx TemplateContext) (string, error) {
	funcMap := template.FuncMap{
		"instance": func(key string) (string, error) {
			v, ok := ctx.Instance[key]
			if !ok {
				return "", fmt.Errorf("instance.%s not set", key)
			}
			return v, nil
		},
		"config": func(key string) (string, error) {
			v, ok := ctx.Config[key]
			if !ok {
				return "", fmt.Errorf("config.%s not set", key)
			}
			return v, nil
		},
		"outputs": func(key string) (string, error) {
			v, ok := ctx.Outputs[key]
			if !ok {
				return "", fmt.Errorf("outputs.%s not set", key)
			}
			return v, nil
		},
		"pushed": func(key string) (string, error) {
			v, ok := ctx.Pushed[key]
			if !ok {
				return "", fmt.Errorf("%w: pushed.%s not set", ErrMissingKey, key)
			}
			return v, nil
		},
	}

	// Convert {{ namespace.key }} to Go template function-call syntax.
	converted := convertTemplateSyntax(tmpl)

	t, err := template.New("plugin").Funcs(funcMap).Parse(converted)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", tmpl, err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, nil); err != nil {
		return "", fmt.Errorf("render template %q: %w", tmpl, err)
	}

	return buf.String(), nil
}

// convertTemplateSyntax rewrites {{ namespace.key }} to {{ namespace "key" }}
// inside {{ ... }} delimiters so that Go's text/template can evaluate them as
// function calls rather than field accesses.
func convertTemplateSyntax(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		if open < 0 {
			result.WriteString(s[i:])
			break
		}
		result.WriteString(s[i : i+open])
		i += open

		close := strings.Index(s[i:], "}}")
		if close < 0 {
			result.WriteString(s[i:])
			break
		}

		expr := strings.TrimSpace(s[i+2 : i+close])
		result.WriteString("{{ ")
		result.WriteString(convertExpr(expr))
		result.WriteString(" }}")
		i += close + 2
	}
	return result.String()
}

// convertExpr converts a "namespace.key" expression to a template function call.
func convertExpr(expr string) string {
	for _, ns := range []string{"instance", "config", "outputs", "pushed"} {
		prefix := ns + "."
		if strings.HasPrefix(expr, prefix) {
			key := strings.TrimPrefix(expr, prefix)
			if !strings.ContainsAny(key, ". \t") {
				return fmt.Sprintf(`%s "%s"`, ns, key)
			}
		}
	}
	return expr
}

// shellQuoteValue wraps s in single quotes, escaping embedded single quotes.
// Used to safely embed user-supplied values into shell command strings.
func shellQuoteValue(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellEscapeContext returns a copy of ctx where Config, Outputs, and Pushed
// values are single-quote-escaped for safe interpolation into shell commands.
// Instance values are trusted (from EC2 metadata) and left unescaped.
func shellEscapeContext(ctx TemplateContext) TemplateContext {
	escaped := TemplateContext{
		Instance: make(map[string]string, len(ctx.Instance)),
		Config:   make(map[string]string, len(ctx.Config)),
		Outputs:  make(map[string]string, len(ctx.Outputs)),
		Pushed:   make(map[string]string, len(ctx.Pushed)),
	}
	for k, v := range ctx.Instance {
		escaped.Instance[k] = shellQuoteValue(v)
	}
	for k, v := range ctx.Config {
		escaped.Config[k] = shellQuoteValue(v)
	}
	for k, v := range ctx.Outputs {
		escaped.Outputs[k] = shellQuoteValue(v)
	}
	for k, v := range ctx.Pushed {
		escaped.Pushed[k] = shellQuoteValue(v)
	}
	return escaped
}

// RenderShellStep renders a step for use as a shell command. Values from the
// config, outputs, and pushed namespaces are single-quote-escaped to prevent
// shell injection. Use this instead of RenderStep for "run" steps.
func RenderShellStep(step Step, ctx TemplateContext) (Step, error) {
	return RenderStep(step, shellEscapeContext(ctx))
}

// RenderStep returns a copy of step with all string fields rendered.
func RenderStep(step Step, ctx TemplateContext) (Step, error) {
	rendered := step
	var err error

	if step.Run != "" {
		if rendered.Run, err = Render(step.Run, ctx); err != nil {
			return rendered, fmt.Errorf("step.run: %w", err)
		}
	}
	if step.URL != "" {
		if rendered.URL, err = Render(step.URL, ctx); err != nil {
			return rendered, fmt.Errorf("step.url: %w", err)
		}
	}
	if step.Dest != "" {
		if rendered.Dest, err = Render(step.Dest, ctx); err != nil {
			return rendered, fmt.Errorf("step.dest: %w", err)
		}
	}
	if step.Src != "" {
		if rendered.Src, err = Render(step.Src, ctx); err != nil {
			return rendered, fmt.Errorf("step.src: %w", err)
		}
	}
	if step.Value != "" {
		if rendered.Value, err = Render(step.Value, ctx); err != nil {
			return rendered, fmt.Errorf("step.value: %w", err)
		}
	}

	return rendered, nil
}
