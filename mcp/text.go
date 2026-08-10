package mcp

// TextBundle supplies localizable MCP instructions and descriptions.
type TextBundle interface {
	Instructions() string
	Tool(name string) string
	Param(tool, name string) string
}

// BaseTextBundle is a complete map-backed text bundle.
type BaseTextBundle struct {
	Instr  string
	Tools  map[string]string
	Params map[string]map[string]string
}

func (bundle BaseTextBundle) Instructions() string           { return bundle.Instr }
func (bundle BaseTextBundle) Tool(name string) string        { return bundle.Tools[name] }
func (bundle BaseTextBundle) Param(tool, name string) string { return bundle.Params[tool][name] }

var _ TextBundle = BaseTextBundle{}
