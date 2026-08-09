package agent

import (
	"encoding/json"
	"sort"

	"reasonix/internal/tool"
)

func toolIdentity(reg *tool.Registry) ([]string, string) {
	if reg == nil {
		return nil, bytesHash(nil)
	}
	names := reg.Names()
	sort.Strings(names)
	schemas := normalizeToolSchemas(reg.Schemas())
	data, _ := json.Marshal(schemas)
	return names, bytesHash(data)
}
