package agent

import (
	"encoding/json"
	"sort"

	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func toolIdentity(reg *tool.Registry, schemas []provider.ToolSchema) ([]string, string) {
	if reg == nil {
		return nil, bytesHash(nil)
	}
	if schemas == nil {
		schemas = reg.Schemas()
	}
	names := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		names = append(names, schema.Name)
	}
	sort.Strings(names)
	schemas = normalizeToolSchemas(schemas)
	data, _ := json.Marshal(schemas)
	return names, bytesHash(data)
}
