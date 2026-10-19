package agent

import (
	"cmp"
	"slices"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// Config option ids.
const (
	configMode   = "mode"
	configModel  = "model"
	configEffort = "reasoning_effort"
)

// effortDefault leaves reasoning effort to the provider.
const effortDefault = "default"

var efforts = []acp.ConfigOptionValue{
	{Value: effortDefault, Name: "Default", Description: "The provider's default"},
	{Value: "minimal", Name: "Minimal"},
	{Value: "low", Name: "Low"},
	{Value: "medium", Name: "Medium"},
	{Value: "high", Name: "High"},
}

// catalog is the set of models a session may use: those that accept tools.
type catalog struct {
	models  map[string]llm.Model
	choices []acp.ConfigOptionValue
}

func newCatalog(listed []llm.Model) *catalog {
	c := &catalog{models: make(map[string]llm.Model)}
	for _, m := range listed {
		if m.ID != "" && m.Supports("tools") {
			c.models[m.ID] = m
		}
	}
	for _, m := range c.models {
		c.choices = append(c.choices, acp.ConfigOptionValue{Value: m.ID, Name: cmp.Or(m.Name, m.ID)})
	}
	slices.SortFunc(c.choices, func(x, y acp.ConfigOptionValue) int { return cmp.Compare(x.Value, y.Value) })
	return c
}

func (c *catalog) has(model string) bool {
	_, ok := c.models[model]
	return ok
}

// contextLength is the model's context window, or 0 if unknown.
func (c *catalog) contextLength(model string) uint64 { return c.models[model].ContextLength }

// outputLimit is the advertised output ceiling, which can be much smaller
// than the context window. Zero means the provider did not publish a limit.
func (c *catalog) outputLimit(model string) uint64 {
	return c.models[model].TopProvider.MaxCompletionTokens
}

// vision reports whether the model is known to accept images. Unlike
// parameters, missing modality metadata is unknown, not support (ADR 0009).
func (c *catalog) vision(model string) bool { return c.models[model].Vision() }

// reasons reports whether the model is known to take a reasoning effort.
func (c *catalog) reasons(model string) bool {
	m := c.models[model]
	return m.SupportedParameters != nil && m.Supports("reasoning")
}

// options lists the config options for a session's current settings. The
// effort option only exists for models that reason.
func (c *catalog) options(model, effort, mode string) []acp.ConfigOption {
	choices := c.choices
	if model == "" {
		// ACP requires a currentValue even before the client selects a model.
		choices = append([]acp.ConfigOptionValue{{Value: "", Name: "Select a model"}}, choices...)
	}
	options := []acp.ConfigOption{{
		ConfigID:     configModel,
		Name:         "Model",
		Category:     acp.CategoryModel,
		Type:         acp.ConfigTypeSelect,
		CurrentValue: model,
		Options:      choices,
	}}
	if c.reasons(model) {
		options = append(options, acp.ConfigOption{
			ConfigID:     configEffort,
			Name:         "Reasoning effort",
			Category:     acp.CategoryThoughtLevel,
			Type:         acp.ConfigTypeSelect,
			CurrentValue: effort,
			Options:      efforts,
		})
	}
	return append(options, acp.ConfigOption{
		ConfigID: configMode, Name: "Mode", Category: "mode", Type: acp.ConfigTypeSelect, CurrentValue: mode,
		Options: []acp.ConfigOptionValue{{Value: modeBuild, Name: "Build"}, {Value: modePlan, Name: "Plan"}},
	})
}

func validEffort(effort string) bool {
	return slices.ContainsFunc(efforts, func(v acp.ConfigOptionValue) bool { return v.Value == effort })
}
