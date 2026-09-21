// Package configs carries the starter config baked into the agent binary.
package configs

import _ "embed"

// ExampleConfigYAML is the commented starter config baked into the binary, used
// by `agent config` to seed ~/.config/agent/config.yaml when it does not exist yet.
//
//go:embed agent.example.yaml
var ExampleConfigYAML []byte
