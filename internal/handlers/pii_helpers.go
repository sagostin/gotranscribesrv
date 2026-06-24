package handlers

import "github.com/shaunagostinho/gotranscribesrv/internal/sidecar"

// piiEntityTypes returns a deduplicated list of entity types from a
// Presidio result. Used to populate the pii_entity_types log field on
// *_COMPLETED events so operators can see what was scrubbed without
// having to ship every individual entity's character offsets.
func piiEntityTypes(items []sidecar.PresidioEntity) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, e := range items {
		if e.EntityType == "" {
			continue
		}
		if _, ok := seen[e.EntityType]; ok {
			continue
		}
		seen[e.EntityType] = struct{}{}
		out = append(out, e.EntityType)
	}
	return out
}
