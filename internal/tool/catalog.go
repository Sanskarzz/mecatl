package tool

import (
	"fmt"
	"sort"

	"github.com/stacklok/mecatl/internal/session"
)

// ErrDuplicateTool is returned by Catalog.Register when a tool with the same
// name is already registered.
var ErrDuplicateTool = fmt.Errorf("tool: duplicate registration")

// Catalog is a name→Tool registry. It is the registration seam for the core
// tools (and, later, MCP tools). It also computes the plan-mode-filtered view of
// the catalog, in which only read-only tools are visible.
type Catalog struct {
	tools map[string]Tool
}

// NewCatalog returns an empty Catalog.
func NewCatalog() *Catalog {
	return &Catalog{tools: make(map[string]Tool)}
}

// Register adds t to the catalog keyed by its Spec().Name. It returns
// ErrDuplicateTool if a tool with that name is already registered.
func (c *Catalog) Register(t Tool) error {
	name := t.Spec().Name
	if _, ok := c.tools[name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateTool, name)
	}
	c.tools[name] = t
	return nil
}

// MustRegister adds t and panics on a duplicate. It is a convenience for static
// registration at startup where a duplicate is a programming error.
func (c *Catalog) MustRegister(t Tool) {
	if err := c.Register(t); err != nil {
		panic(err)
	}
}

// Lookup returns the tool registered under name and true, or nil and false.
func (c *Catalog) Lookup(name string) (Tool, bool) {
	t, ok := c.tools[name]
	return t, ok
}

// Tools returns all registered tools, ordered by name for determinism.
func (c *Catalog) Tools() []Tool {
	return c.sorted(func(Tool) bool { return true })
}

// Specs returns the ToolSpecs of the tools available under the given permission
// mode, ordered by name. In ModePlan only read-only tools are exposed, enforcing
// plan-mode read-only gating at the catalog level before dispatch.
func (c *Catalog) Specs(mode session.PermissionMode) []ToolSpec {
	tools := c.Available(mode)
	specs := make([]ToolSpec, len(tools))
	for i, t := range tools {
		specs[i] = t.Spec()
	}
	return specs
}

// AdvertisedSpecs returns the per-turn tool inventory under progressive
// disclosure: for each available tool (mode-filtered, ordered by name) it returns
// the tool's Advertised() metadata spec when the tool implements Disclosable,
// otherwise its full Spec(). Because a tool that does not implement Disclosable
// falls back to its full Spec(), a catalog of only non-disclosable tools yields a
// result identical to Specs(mode) — making full-spec disclosure the default.
func (c *Catalog) AdvertisedSpecs(mode session.PermissionMode) []ToolSpec {
	tools := c.Available(mode)
	specs := make([]ToolSpec, len(tools))
	for i, t := range tools {
		if d, ok := t.(Disclosable); ok {
			specs[i] = d.Advertised()
			continue
		}
		specs[i] = t.Spec()
	}
	return specs
}

// Available returns the tools usable under the given permission mode, ordered by
// name. In ModePlan, non-read-only tools are filtered out.
func (c *Catalog) Available(mode session.PermissionMode) []Tool {
	if mode == session.ModePlan {
		return c.sorted(func(t Tool) bool { return t.ReadOnly() })
	}
	return c.sorted(func(Tool) bool { return true })
}

// sorted returns the tools matching keep, ordered by name.
func (c *Catalog) sorted(keep func(Tool) bool) []Tool {
	out := make([]Tool, 0, len(c.tools))
	for _, t := range c.tools {
		if keep(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Spec().Name < out[j].Spec().Name
	})
	return out
}
